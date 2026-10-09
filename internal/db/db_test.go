package db

import (
	"path/filepath"
	"sync"
	"testing"
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
