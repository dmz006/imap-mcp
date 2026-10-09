package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmz006/imap-mcp/internal/db"
)

func TestEncryptFilesReport(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "imap.db")
	ca := filepath.Join(dir, "cache.db")
	d, err := db.Open(db.Options{Path: st}, db.Options{Path: ca})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	var out bytes.Buffer
	targets := []encryptTarget{
		{"state", "db.encryption_key", st, "state-key", true},
		{"cache", "db.cache.encryption_key", ca, "", true},
	}
	if err := encryptFiles(&out, targets); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"state: encrypted", "plaintext original removed", "cache: db.cache.encryption_key not set"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "state-key") {
		t.Error("key printed")
	}

	out.Reset()
	if err := encryptFiles(&out, targets); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already encrypted") {
		t.Errorf("rerun output:\n%s", out.String())
	}

	// A failure is reported and returned.
	busy, err := db.Open(db.Options{Path: ca}, db.Options{Path: filepath.Join(dir, "c2.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	out.Reset()
	err = encryptFiles(&out, []encryptTarget{{"cache", "db.cache.encryption_key", ca, "k", true}})
	if err == nil || !strings.Contains(out.String(), "FAILED") || !strings.Contains(out.String(), "in use") {
		t.Errorf("busy file: err=%v out=%s", err, out.String())
	}
}
