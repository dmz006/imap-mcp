package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plaintextPair creates plaintext state + cache DBs with a few rules and an
// FTS-indexed message, plus a plaintext backup next to the state file.
func plaintextPair(t *testing.T) (st, ca Options, backup string) {
	t.Helper()
	dir := t.TempDir()
	st = Options{Path: filepath.Join(dir, "imap.db")}
	ca = Options{Path: filepath.Join(dir, "cache.db")}
	d, err := Open(st, ca)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"secret-rule-a", "secret-rule-b", "secret-rule-c"} {
		if _, err := d.Rules.Create(&Rule{Name: n, Conditions: RuleConditions{From: n + "@example.com"}, Actions: []RuleAction{{Type: "trash"}}}); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, d.SQL(), `INSERT INTO messages(account, folder, uid, subject, body_text, from_addr, date) VALUES('a','INBOX',1,'needle-subject','needle body','x@example.com',1)`)
	d.Close()

	backup = st.Path + ".bak-20261009-000000-pre-0.6.0"
	b, err := os.ReadFile(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return st, ca, backup
}

func TestEncryptConvertsInPlace(t *testing.T) {
	st, ca, backup := plaintextPair(t)
	const sk, ck = "state passphrase", "cache passphrase"

	rep, err := Encrypt(st.Path, sk)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables == 0 || rep.Rows < 3 {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.PlaintextCopies) != 1 || rep.PlaintextCopies[0] != backup {
		t.Errorf("plaintext copies = %v, want [%s]", rep.PlaintextCopies, backup)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("backup must be listed, never deleted: %v", err)
	}
	if _, err := Encrypt(ca.Path, ck); err != nil {
		t.Fatal(err)
	}

	// No plaintext left in the files or any sidecar; no temp file left over.
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
	if _, err := os.Stat(st.Path + ".encrypting"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file left behind: %v", err)
	}
	if fi, err := os.Stat(st.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("encrypted file mode = %v, %v", fi.Mode().Perm(), err)
	}

	// Data intact and usable with the keys; FTS still matches.
	d, err := Open(Options{Path: st.Path, Key: sk}, Options{Path: ca.Path, Key: ck})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rules, _ := d.Rules.List()
	if len(rules) != 3 || rules[0].Conditions.From != "secret-rule-a@example.com" {
		t.Errorf("rules after encrypt = %+v", rules)
	}
	var hits int
	d.SQL().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'needle'`).Scan(&hits)
	if hits != 1 {
		t.Errorf("FTS hits after encrypt = %d", hits)
	}

	// Without the key it fails closed.
	if _, err := OpenState(Options{Path: st.Path}); err == nil {
		t.Error("encrypted state opened without a key")
	}
}

func TestEncryptRefusesWhileInUse(t *testing.T) {
	st, ca, _ := plaintextPair(t)
	d, err := Open(st, ca) // the "running service"
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Rules.List(); err != nil {
		t.Fatal(err)
	}

	_, err = Encrypt(st.Path, "k")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("encrypt while open: %v", err)
	}
	if k, _ := detect(st.Path); k != filePlain {
		t.Error("original changed after a refused encrypt")
	}
	if rules, err := d.Rules.List(); err != nil || len(rules) != 3 {
		t.Errorf("open handle broken after refused encrypt: %v %d", err, len(rules))
	}
}

func TestEncryptEdgeCases(t *testing.T) {
	st, _, _ := plaintextPair(t)
	if _, err := Encrypt(st.Path, ""); err == nil {
		t.Error("empty key accepted")
	}
	if _, err := Encrypt(filepath.Join(t.TempDir(), "missing.db"), "k"); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := Encrypt(st.Path, "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Encrypt(st.Path, "k2"); !errors.Is(err, ErrAlreadyEncrypted) {
		t.Errorf("second encrypt: %v", err)
	}
}

func TestRedactHidesKey(t *testing.T) {
	key := "p@ss word/with&chars"
	err := errors.New("open file:x?textkey=" + "p%40ss+word%2Fwith%26chars" + " and " + key)
	if s := redact(err, key); strings.Contains(s, "p@ss") || strings.Contains(s, "p%40ss") {
		t.Errorf("redact leaked key: %s", s)
	}
}
