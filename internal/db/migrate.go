package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// stateTables are the tables that must survive the legacy split unchanged.
var stateTables = []string{"rules", "webhooks", "inbound_nonces"}

// legacyCacheObjects are dropped from a pre-0.6.0 imap.db, children first so
// foreign keys never block a drop. Their data is disposable (rebuilt from IMAP
// into cache.db); the pre-migration backup keeps a full copy regardless.
var legacyCacheObjects = []string{
	"TRIGGER messages_fts_insert",
	"TRIGGER messages_fts_delete",
	"TRIGGER messages_fts_update",
	"TABLE messages_fts",
	"TABLE message_vectors",
	"TABLE enrichment_queue",
	"TABLE anomalies",
	"TABLE kg_relationships",
	"TABLE kg_entities",
	"TABLE senders",
	"TABLE folders",
	"TABLE sync_state",
	"TABLE messages",
}

// MigrationReport describes a completed legacy split.
type MigrationReport struct {
	BackupPath string
	Counts     map[string]int64  // rows per state table (identical before/after)
	Digests    map[string]string // SHA-256 of each state table's rows
}

// LastMigration is set when OpenState performed a legacy split in this
// process, so callers can log it.
var LastMigration *MigrationReport

// migrateLegacy splits a pre-0.6.0 single-file imap.db: it backs the file up
// with VACUUM INTO, fingerprints the state tables, drops the cache tables in
// one IMMEDIATE transaction, and verifies the fingerprints are unchanged
// before committing. It is a no-op for new files, already-split files and
// encrypted files (which can only have been created by 0.6.0+).
func migrateLegacy(o Options) error {
	kind, err := detect(o.Path)
	if err != nil || kind != filePlain {
		return err
	}
	conn, err := driver.Open(dsn(Options{Path: o.Path}), fts5.Register)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)

	legacy, err := hasTable(conn, "messages")
	if err != nil || !legacy {
		return err
	}

	backup := fmt.Sprintf("%s.bak-%s-pre-0.6.0", o.Path, time.Now().Format("20060102-150405"))
	if _, err := conn.Exec(`VACUUM INTO ?`, backup); err != nil {
		return fmt.Errorf("backup to %s: %w", backup, err)
	}
	before, err := fingerprint(conn)
	if err != nil {
		return fmt.Errorf("fingerprint before: %w", err)
	}
	if err := verifyBackup(backup, before); err != nil {
		return err
	}

	// IMMEDIATE takes the write lock up front, so a concurrent opener (service
	// vs. run-rules) waits and then sees the split already done.
	if _, err := conn.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	rollback := func(e error) error {
		_, _ = conn.Exec(`ROLLBACK`)
		return e
	}
	if legacy, err = hasTable(conn, "messages"); err != nil || !legacy {
		return rollback(err)
	}
	for _, obj := range legacyCacheObjects {
		if _, err := conn.Exec(`DROP ` + strings.Replace(obj, " ", " IF EXISTS ", 1)); err != nil {
			return rollback(fmt.Errorf("drop %s: %w", obj, err))
		}
	}
	after, err := fingerprint(conn)
	if err != nil {
		return rollback(fmt.Errorf("fingerprint after: %w", err))
	}
	if err := sameFingerprint(before, after); err != nil {
		return rollback(err)
	}
	if _, err := conn.Exec(`COMMIT`); err != nil {
		return rollback(err)
	}
	if _, err := conn.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("vacuum after split: %w", err)
	}
	LastMigration = &MigrationReport{BackupPath: backup, Counts: before.counts, Digests: before.digests}
	return nil
}

type tableFingerprint struct {
	counts  map[string]int64
	digests map[string]string
}

// fingerprint counts and hashes every row of each state table, ordered by
// rowid/primary key, so any change in content is detected.
func fingerprint(conn *sql.DB) (*tableFingerprint, error) {
	fp := &tableFingerprint{counts: map[string]int64{}, digests: map[string]string{}}
	for _, t := range stateTables {
		ok, err := hasTable(conn, t)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		rows, err := conn.Query(`SELECT * FROM ` + t + ` ORDER BY 1, 2`)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		cols, _ := rows.Columns()
		h := sha256.New()
		var n int64
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			for _, v := range vals {
				fmt.Fprintf(h, "%T:%v\x1f", v, v)
			}
			h.Write([]byte{0x1e})
			n++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		fp.counts[t] = n
		fp.digests[t] = hex.EncodeToString(h.Sum(nil))
	}
	return fp, nil
}

func sameFingerprint(a, b *tableFingerprint) error {
	for _, t := range stateTables {
		if a.counts[t] != b.counts[t] || a.digests[t] != b.digests[t] {
			return fmt.Errorf("state table %s changed during migration (rows %d → %d); rolled back", t, a.counts[t], b.counts[t])
		}
	}
	return nil
}

// verifyBackup opens the backup read-only and checks it holds the same state.
func verifyBackup(path string, want *tableFingerprint) error {
	_ = os.Chmod(path, 0o600)
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	conn, err := driver.Open(u.String(), fts5.Register)
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	defer conn.Close()
	got, err := fingerprint(conn)
	if err != nil {
		return fmt.Errorf("fingerprint backup: %w", err)
	}
	if err := sameFingerprint(want, got); err != nil {
		return fmt.Errorf("backup %s does not match source: %w", path, err)
	}
	return nil
}

func hasTable(conn *sql.DB, name string) (bool, error) {
	var n int
	err := conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0, err
}
