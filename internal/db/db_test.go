package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestOpenAppliesPragmas guards against DSN params the driver silently ignores:
// the connection must actually run in WAL mode with a busy timeout and FKs on.
func TestOpenAppliesPragmas(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(Options{Path: filepath.Join(dir, "imap.db")}, Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	var journal string
	if err := d.SQL().QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var timeout int
	if err := d.SQL().QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}

	var fk int
	if err := d.SQL().QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

// TestConcurrentOpenersShareFile reproduces the service + run-rules setup: two
// independent handles on one file writing at the same time must not fail with
// SQLITE_BUSY ("database is locked").
func TestConcurrentOpenersShareFile(t *testing.T) {
	dir := t.TempDir()
	st, ca := Options{Path: filepath.Join(dir, "imap.db")}, Options{Path: filepath.Join(dir, "cache.db")}
	a, err := Open(st, ca)
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	defer a.Close()
	b, err := Open(st, ca)
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer b.Close()

	const n = 200
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	write := func(d *DB, account string) {
		defer wg.Done()
		for i := 0; i < n; i++ {
			_, err := d.SQL().Exec(`
				INSERT INTO sync_state(account, folder, last_synced) VALUES(?, ?, unixepoch())
				ON CONFLICT(account, folder) DO UPDATE SET last_synced=unixepoch()`,
				account, "INBOX")
			if err != nil {
				errs <- err
				return
			}
		}
	}
	wg.Add(2)
	go write(a, "a")
	go write(b, "b")
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}
}

// TestIntelTablesMoveToState (D19): opening a cache.db that still has the old,
// empty intelligence tables drops them without rebuilding the cache, and the
// state DB gains them.
func TestIntelTablesMoveToState(t *testing.T) {
	dir := t.TempDir()
	st, ca := Options{Path: filepath.Join(dir, "imap.db")}, Options{Path: filepath.Join(dir, "cache.db")}
	d, err := Open(st, ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Messages.Insert(context.Background(), &CachedMessage{Account: "a", Folder: "INBOX", UID: 1, FromAddr: "x@example.com",
		InternalDate: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Simulate a 0.11 cache that still carries the tables.
	for _, q := range []string{`CREATE TABLE senders (id INTEGER PRIMARY KEY, address TEXT)`, `CREATE TABLE anomalies (id INTEGER PRIMARY KEY)`} {
		if _, err := d.SQL().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	d, err = Open(st, ca)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	d.SQL().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('senders','anomalies','kg_entities','kg_relationships')`).Scan(&n) //nolint:errcheck
	if n != 0 {
		t.Errorf("%d intelligence tables left in cache.db", n)
	}
	d.SQL().QueryRow(`SELECT count(*) FROM messages`).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("cache was rebuilt: %d messages", n)
	}
	d.StateSQL().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('senders','anomalies','kg_entities','kg_relationships','intel_messages','intel_scan')`).Scan(&n) //nolint:errcheck
	if n != 6 {
		t.Errorf("state DB has %d of 6 intelligence tables", n)
	}
}

// TestStateMigrationAddsKGColumns: a 0.12 imap.db (index without the graph
// flags, scan complete) gains the columns and indexes, keeps its rows, and has
// its header-scan progress reset so the graph is built once for every message.
func TestStateMigrationAddsKGColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imap.db")
	old, err := OpenState(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the 0.12 shapes of the two changed tables.
	for _, q := range []string{
		`DROP TABLE intel_messages`, `DROP TABLE kg_relationships`,
		`CREATE TABLE intel_messages (account TEXT NOT NULL, msg_hash INTEGER NOT NULL, date INTEGER NOT NULL, sender_id INTEGER,
			outgoing INTEGER NOT NULL DEFAULT 0, reply_hash INTEGER, paired INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (account, msg_hash)) WITHOUT ROWID`,
		`CREATE TABLE kg_relationships (id INTEGER PRIMARY KEY AUTOINCREMENT, subject_id INTEGER NOT NULL, predicate TEXT NOT NULL,
			object_id INTEGER NOT NULL, valid_from INTEGER, valid_to INTEGER, confidence REAL DEFAULT 1.0, properties TEXT, created_at INTEGER)`,
		`INSERT INTO intel_messages(account, msg_hash, date) VALUES('a', 1, 0), ('a', 2, 0)`,
		`INSERT INTO intel_scan(account, folder, uidvalidity, last_uid, scanned, completed_at) VALUES('a','INBOX',7,500,500,unixepoch())`,
	} {
		if _, err := old.StateSQL().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	d, err := OpenState(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var rows, flagged int
	d.StateSQL().QueryRow(`SELECT count(*), sum(kg_done + kg_tags_done + kg_llm_done) FROM intel_messages`).Scan(&rows, &flagged) //nolint:errcheck
	if rows != 2 || flagged != 0 {
		t.Errorf("index rows=%d flags=%d", rows, flagged)
	}
	var last int
	var completed sql.NullInt64
	d.StateSQL().QueryRow(`SELECT last_uid, completed_at FROM intel_scan`).Scan(&last, &completed) //nolint:errcheck
	if last != 0 || completed.Valid {
		t.Errorf("scan not reset: last_uid=%d completed=%v", last, completed)
	}
	if _, err := d.StateSQL().Exec(`INSERT INTO kg_relationships(subject_id, predicate, object_id, weight, last_seen) VALUES(1,'p',2,1,0)`); err != nil {
		t.Fatalf("new columns missing: %v", err)
	}
	if _, err := d.StateSQL().Exec(`INSERT INTO kg_relationships(subject_id, predicate, object_id) VALUES(1,'p',2)`); err == nil {
		t.Error("unique (subject, predicate, object) index missing")
	}
	// A second open is a no-op: the scan is not reset again.
	d.StateSQL().Exec(`UPDATE intel_scan SET last_uid = 9`) //nolint:errcheck
	d.Close()
	d, _ = OpenState(Options{Path: path})
	defer d.Close()
	d.StateSQL().QueryRow(`SELECT last_uid FROM intel_scan`).Scan(&last) //nolint:errcheck
	if last != 9 {
		t.Errorf("second open reset the scan: last_uid=%d", last)
	}
}
