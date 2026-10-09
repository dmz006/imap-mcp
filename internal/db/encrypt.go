package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// ErrAlreadyEncrypted is returned by Encrypt for a file that is not plaintext.
var ErrAlreadyEncrypted = errors.New("already encrypted (or not a SQLite database)")

// ErrNoDatabase is returned by Encrypt when the file does not exist yet.
var ErrNoDatabase = errors.New("no database file to encrypt")

// EncryptReport describes a completed conversion.
type EncryptReport struct {
	Path   string
	Tables int
	Rows   int64
	// PlaintextCopies lists other plaintext SQLite files next to Path whose
	// name starts with its base name (e.g. pre-migration backups). Encrypt
	// never deletes them; the operator decides (D14).
	PlaintextCopies []string
}

// Encrypt converts a plaintext database to an adiantum-encrypted one in place
// (AGENT.md D14). It refuses while any other connection has the file open,
// writes an encrypted copy with VACUUM INTO, verifies it (opens with key,
// integrity check, identical per-table row counts and content digests) and
// only then atomically replaces the plaintext original.
func Encrypt(path, key string) (*EncryptReport, error) {
	if key == "" {
		return nil, errors.New("no encryption key configured")
	}
	kind, err := detect(path)
	switch {
	case err != nil:
		return nil, err
	case kind == fileNew:
		return nil, ErrNoDatabase
	case kind == fileOpaque:
		return nil, ErrAlreadyEncrypted
	}

	ctx := context.Background()
	src, err := lockExclusive(ctx, path)
	if err != nil {
		return nil, err
	}
	defer src.close()

	want, err := contentDigest(ctx, src.conn)
	if err != nil {
		return nil, fmt.Errorf("fingerprint %s: %w", path, err)
	}

	tmp := path + ".encrypting"
	for _, p := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	// Create the target private and empty; VACUUM INTO fills it.
	if err := restrictFiles(tmp); err != nil {
		return nil, err
	}
	fail := func(e error) (*EncryptReport, error) {
		_ = os.Remove(tmp)
		return nil, e
	}
	if _, err := src.conn.ExecContext(ctx, `VACUUM INTO ?`, encryptedURI(tmp, key, false)); err != nil {
		return fail(fmt.Errorf("write encrypted copy: %s", redact(err, key)))
	}
	if k, err := detect(tmp); err != nil || k != fileOpaque {
		return fail(fmt.Errorf("encrypted copy %s is not encrypted; original left untouched", tmp))
	}
	if err := verifyEncrypted(ctx, tmp, key, want); err != nil {
		return fail(fmt.Errorf("verify encrypted copy (original left untouched): %s", redact(err, key)))
	}

	if err := syncFile(tmp); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fail(fmt.Errorf("replace %s: %w", path, err))
	}
	_ = syncFile(filepath.Dir(path))
	for _, p := range []string{path + "-wal", path + "-shm", path + "-journal"} {
		_ = os.Remove(p) // sidecars of the plaintext file; none after journal_mode=DELETE
	}

	rep := &EncryptReport{Path: path, Tables: len(want)}
	for _, d := range want {
		rep.Rows += d.rows
	}
	rep.PlaintextCopies, _ = PlaintextCopies(path)
	return rep, nil
}

// PlaintextCopies lists plaintext SQLite files in path's directory whose name
// starts with path's base name, other than path itself and its sidecars.
func PlaintextCopies(path string) ([]string, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || n == base || !strings.HasPrefix(n, base) ||
			strings.HasSuffix(n, "-wal") || strings.HasSuffix(n, "-shm") || strings.HasSuffix(n, "-journal") {
			continue
		}
		p := filepath.Join(dir, n)
		if k, err := detect(p); err == nil && k == filePlain {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// exclusiveConn is a single connection holding an EXCLUSIVE lock on a
// rollback-journal database until closed.
type exclusiveConn struct {
	db   *sql.DB
	conn *sql.Conn
}

func (c *exclusiveConn) close() {
	c.conn.Close()
	c.db.Close()
}

// lockExclusive opens path, leaves WAL mode and takes a persistent EXCLUSIVE
// lock. Leaving WAL fails while any other process has the file open, which is
// how a running service is detected.
func lockExclusive(ctx context.Context, path string) (*exclusiveConn, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(0)")
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	db, err := driver.Open(u.String(), fts5.Register)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	c := &exclusiveConn{db: db, conn: conn}
	inUse := fmt.Errorf("%s is in use by another process; stop the service first (systemctl --user stop imap-mcp) and retry", path)

	var mode string
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		c.close()
		if strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy") {
			return nil, inUse
		}
		return nil, fmt.Errorf("leave WAL mode on %s: %w", path, err)
	}
	if mode != "delete" {
		c.close()
		return nil, inUse
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		c.close()
		return nil, err
	}
	// In exclusive locking mode the lock taken by a write transaction is kept
	// until the connection closes.
	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		c.close()
		return nil, inUse
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

// encryptedURI is the SQLite URI of an adiantum-encrypted file.
func encryptedURI(path, key string, readOnly bool) string {
	q := url.Values{}
	q.Set("vfs", "adiantum")
	q.Set("textkey", key)
	if readOnly {
		q.Set("mode", "ro")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

func verifyEncrypted(ctx context.Context, path, key string, want map[string]tableDigest) error {
	db, err := driver.Open(encryptedURI(path, key, true)+"&_pragma=temp_store(memory)", fts5.Register)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var ok string
	if err := conn.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if ok != "ok" {
		return fmt.Errorf("integrity check: %s", ok)
	}
	got, err := contentDigest(ctx, conn)
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("table count differs: %d → %d", len(want), len(got))
	}
	for t, w := range want {
		if g, ok := got[t]; !ok || g != w {
			return fmt.Errorf("table %s differs (rows %d → %d)", t, w.rows, g.rows)
		}
	}
	return nil
}

type tableDigest struct {
	rows int64
	sum  [2]uint64
}

// contentDigest counts and hashes every ordinary table (virtual tables are
// covered by their shadow tables; sqlite_* internals are skipped). The digest
// is a sum of per-row hashes, so it is independent of row order.
func contentDigest(ctx context.Context, conn *sql.Conn) (map[string]tableDigest, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_master
		WHERE type='table' AND name NOT LIKE 'sqlite_%' AND sql NOT LIKE 'CREATE VIRTUAL%'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]tableDigest, len(tables))
	for _, t := range tables {
		d, err := digestTable(ctx, conn, t)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		out[t] = d
	}
	return out, nil
}

func digestTable(ctx context.Context, conn *sql.Conn, table string) (tableDigest, error) {
	var d tableDigest
	rows, err := conn.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(table, `"`, `""`)+`"`)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return d, err
		}
		h := sha256.New()
		for _, v := range vals {
			fmt.Fprintf(h, "%T:%v\x1f", v, v)
		}
		s := h.Sum(nil)
		d.sum[0] += binary.LittleEndian.Uint64(s[0:8])
		d.sum[1] += binary.LittleEndian.Uint64(s[8:16])
		d.rows++
	}
	return d, rows.Err()
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// redact removes the key (raw and URI-escaped) from an error message.
func redact(err error, key string) string {
	s := err.Error()
	s = strings.ReplaceAll(s, url.QueryEscape(key), "***")
	return strings.ReplaceAll(s, key, "***")
}
