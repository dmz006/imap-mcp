package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// legacySchema is the pre-0.6.0 single-file layout: cache + state together.
var legacySchema = cacheSchema + stateSchema

// makeLegacy creates a pre-0.6.0 imap.db with nRules rules, a webhook, nonces
// and some cache rows, returning the rules' (name, conditions) for comparison.
func makeLegacy(t *testing.T, path string, nRules int) map[string]string {
	t.Helper()
	conn, err := driver.Open(dsn(Options{Path: path}), fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for i := 0; i < nRules; i++ {
		name := fmt.Sprintf("rule-%03d", i)
		cond := fmt.Sprintf(`{"from":"sender%d@example.com"}`, i)
		if _, err := conn.Exec(`INSERT INTO rules(name, conditions, actions, active, run_count) VALUES(?,?,?,?,?)`,
			name, cond, `[{"type":"trash"}]`, i%3 != 0, i); err != nil {
			t.Fatal(err)
		}
		want[name] = cond
	}
	mustExec(t, conn, `INSERT INTO webhooks(url, events, secret) VALUES('https://hooks.example.com/x', '["rule.fired"]', 's3cret')`)
	mustExec(t, conn, `INSERT INTO inbound_nonces(account, nonce, cmd_ts) VALUES('personal', 'n-1', 1700000000), ('work', 'n-2', NULL)`)
	mustExec(t, conn, `INSERT INTO messages(account, folder, uid, subject, from_addr, date) VALUES('personal','INBOX',1,'hello','a@example.com',1700000000)`)
	mustExec(t, conn, `INSERT INTO sync_state(account, folder, last_uid) VALUES('personal','INBOX',1)`)
	return want
}

func mustExec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestLegacySplitPreservesState(t *testing.T) {
	LastMigration = nil
	dir := t.TempDir()
	statePath := filepath.Join(dir, "imap.db")
	want := makeLegacy(t, statePath, 250)

	d, err := Open(Options{Path: statePath}, Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	m := LastMigration
	if m == nil {
		t.Fatal("expected a migration report")
	}
	if m.Counts["rules"] != 250 || m.Counts["webhooks"] != 1 || m.Counts["inbound_nonces"] != 2 {
		t.Errorf("counts = %v", m.Counts)
	}

	// Every rule survives, readable through the repository.
	rules, err := d.Rules.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 250 {
		t.Fatalf("rules after split = %d, want 250", len(rules))
	}
	for _, r := range rules {
		if _, ok := want[r.Name]; !ok {
			t.Errorf("unexpected rule %q", r.Name)
		}
	}
	// State is byte-for-byte the same as the backup's.
	after, err := fingerprint(d.StateSQL())
	if err != nil {
		t.Fatal(err)
	}
	if err := sameFingerprint(&tableFingerprint{m.Counts, m.Digests}, after); err != nil {
		t.Fatal(err)
	}

	// Cache tables are gone from imap.db and present (empty) in cache.db.
	if ok, _ := hasTable(d.StateSQL(), "messages"); ok {
		t.Error("imap.db still has messages table")
	}
	if ok, _ := hasTable(d.SQL(), "messages"); !ok {
		t.Error("cache.db has no messages table")
	}

	// Backup exists, is private, and still holds the full legacy layout.
	fi, err := os.Stat(m.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600", fi.Mode().Perm())
	}
	bk, _ := driver.Open("file:"+m.BackupPath+"?mode=ro", fts5.Register)
	defer bk.Close()
	var msgs, nrules int
	bk.QueryRow(`SELECT count(*) FROM messages`).Scan(&msgs)
	bk.QueryRow(`SELECT count(*) FROM rules`).Scan(&nrules)
	if msgs != 1 || nrules != 250 {
		t.Errorf("backup has %d messages, %d rules", msgs, nrules)
	}

	// Re-opening is a no-op (idempotent).
	d.Close()
	LastMigration = nil
	d2, err := Open(Options{Path: statePath}, Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	d2.Close()
	if LastMigration != nil {
		t.Error("second open migrated again")
	}
}

func TestOpenStateNeverTouchesCache(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenState(Options{Path: filepath.Join(dir, "imap.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.SQL() != nil || d.Messages != nil {
		t.Error("state-only DB exposes cache")
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); !os.IsNotExist(err) {
		t.Error("OpenState created cache.db")
	}
	if _, err := d.Rules.Create(&Rule{Name: "x", Actions: []RuleAction{{Type: "trash"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedFiles(t *testing.T) {
	dir := t.TempDir()
	st := Options{Path: filepath.Join(dir, "imap.db"), Key: "state passphrase"}
	ca := Options{Path: filepath.Join(dir, "cache.db"), Key: "cache passphrase"}

	d, err := Open(st, ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Rules.Create(&Rule{Name: "secret-rule", Actions: []RuleAction{{Type: "trash"}}}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, d.SQL(), `INSERT INTO messages(account, folder, uid, subject, body_text, from_addr, date) VALUES('a','INBOX',1,'needle-subject','needle body','x@example.com',1)`)
	var hits int
	d.SQL().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'needle'`).Scan(&hits)
	if hits != 1 {
		t.Errorf("FTS over encrypted cache: %d hits", hits)
	}
	var mode string
	d.SQL().QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("encrypted journal_mode = %q", mode)
	}
	d.Close()

	// No plaintext on disk, in the DB or its WAL.
	for _, f := range []string{st.Path, ca.Path, st.Path + "-wal", ca.Path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, needle := range []string{"secret-rule", "needle-subject", sqliteMagic} {
			if strings.Contains(string(b), needle) {
				t.Errorf("%s contains plaintext %q", filepath.Base(f), needle)
			}
		}
	}

	// Reopen with the right keys works.
	d, err = Open(st, ca)
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := d.Rules.List()
	d.Close()
	if len(rules) != 1 {
		t.Fatalf("rules after reopen = %d", len(rules))
	}

	// Fail closed: wrong key, missing key.
	if _, err := OpenState(Options{Path: st.Path, Key: "wrong"}); err == nil || !strings.Contains(err.Error(), "wrong encryption key") {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := OpenState(Options{Path: st.Path}); err == nil || !strings.Contains(err.Error(), "no encryption key") {
		t.Errorf("missing key: %v", err)
	}
}

func TestKeyOnPlaintextFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "imap.db")
	d, err := OpenState(Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	_, err = OpenState(Options{Path: p, Key: "k"})
	if err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("err = %v", err)
	}
}

func TestDSNDoesNotBreakOnSpecialPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with space#and?marks")
	d, err := Open(Options{Path: filepath.Join(dir, "imap.db"), Key: "k'\"&="}, Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := os.Stat(filepath.Join(dir, "imap.db")); err != nil {
		t.Fatal(err)
	}
}

func TestFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	st, ca := Options{Path: filepath.Join(dir, "imap.db")}, Options{Path: filepath.Join(dir, "cache.db"), Key: "k"}
	d, err := Open(st, ca)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, d.SQL(), `INSERT INTO sync_state(account, folder) VALUES('a','b')`)
	mustExec(t, d.StateSQL(), `INSERT INTO inbound_nonces(account, nonce) VALUES('a','n')`)
	defer d.Close()
	for _, f := range []string{st.Path, st.Path + "-wal", st.Path + "-shm", ca.Path, ca.Path + "-wal", ca.Path + "-shm"} {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(f), fi.Mode().Perm())
		}
	}
}
