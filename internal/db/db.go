// Package db manages the local SQLite databases (AGENT.md D1, D1a, D1b):
//
//   - the state DB (imap.db): rules, webhooks, webhook deliveries, inbound
//     nonces, and intelligence (sender profiles, knowledge graph, anomalies,
//     the per-message hash index and header-scan progress; D19, D28).
//   - the cache DB (cache.db): message cache, FTS5, vectors, sync state and
//     the enrichment queue. Disposable: every row can be rebuilt from IMAP.
//
// Both use the pure-Go ncruces/go-sqlite3 driver. Each file is independently
// and optionally encrypted at rest with the adiantum VFS, keyed by an operator
// passphrase (Argon2id). A missing or wrong key fails closed.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum" // registers the "adiantum" VFS
)

// Options locates one database file. Key is the resolved passphrase; empty
// means the file is plaintext.
type Options struct {
	Path string
	Key  string
}

// DB holds the state and cache connections and exposes typed repositories.
// A state-only DB (OpenState, used by run-rules) has no cache connection and
// nil cache repositories.
type DB struct {
	state *sql.DB
	cache *sql.DB

	// cache.db
	Messages *MessageRepo
	Vectors  *VectorRepo
	Folders  *FolderRepo
	Sync     *SyncRepo
	Enrich   *EnrichRepo

	// imap.db
	Webhooks  *WebhookRepo
	Rules     *RuleRepo
	Nonces    *NonceRepo
	Senders   *SenderRepo
	KG        *KGRepo
	Anomalies *AnomalyRepo
}

// Open opens (creating if needed) the state and cache databases. A legacy
// single-file imap.db is split first (see migrateLegacy).
func Open(state, cache Options) (*DB, error) {
	d, err := OpenState(state)
	if err != nil {
		return nil, err
	}
	c, err := openFile(cache, "")
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("cache db: %w", err)
	}
	if err := ensureCacheSchema(c); err != nil {
		c.Close()
		d.Close()
		return nil, fmt.Errorf("cache db: %w", err)
	}
	d.cache = c
	d.Messages = &MessageRepo{db: c}
	d.Vectors = &VectorRepo{db: c}
	d.Folders = &FolderRepo{db: c}
	d.Sync = &SyncRepo{db: c}
	d.Enrich = &EnrichRepo{db: c}
	return d, nil
}

// OpenState opens only the state database (rules, webhooks, nonces). It never
// touches the cache file or needs the cache key.
func OpenState(state Options) (*DB, error) {
	if err := migrateLegacy(state); err != nil {
		return nil, fmt.Errorf("state db: migrate legacy layout: %w", err)
	}
	s, err := openFile(state, stateSchema)
	if err != nil {
		return nil, fmt.Errorf("state db: %w", err)
	}
	return &DB{
		state:     s,
		Webhooks:  &WebhookRepo{db: s},
		Rules:     &RuleRepo{db: s},
		Nonces:    &NonceRepo{db: s},
		Senders:   &SenderRepo{db: s},
		KG:        &KGRepo{db: s},
		Anomalies: &AnomalyRepo{db: s},
	}, nil
}

// Close closes both connections.
func (db *DB) Close() error {
	var errs []error
	if db.cache != nil {
		errs = append(errs, db.cache.Close())
	}
	if db.state != nil {
		errs = append(errs, db.state.Close())
	}
	return errors.Join(errs...)
}

// SQL returns the cache connection (messages, sync state, enrichment queue).
// It is nil for a state-only DB.
func (db *DB) SQL() *sql.DB { return db.cache }

// StateSQL returns the state connection (rules, webhooks, nonces).
func (db *DB) StateSQL() *sql.DB { return db.state }

// sqliteMagic is the header of every plaintext SQLite database file.
const sqliteMagic = "SQLite format 3\x00"

// fileKind reports whether path is missing/empty, a plaintext SQLite file, or
// something else (an encrypted file, or not a database at all).
type fileKind int

const (
	fileNew fileKind = iota
	filePlain
	fileOpaque
)

func detect(path string) (fileKind, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileNew, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(f, buf)
	if n == 0 && (err == io.EOF || err == io.ErrUnexpectedEOF) {
		return fileNew, nil
	}
	if string(buf[:n]) == sqliteMagic {
		return filePlain, nil
	}
	return fileOpaque, nil
}

// dsn builds the driver DSN. busy_timeout comes first so it applies while
// switching journal mode; the service and the hourly run-rules CLI share the
// state file, so WAL + busy_timeout prevent SQLITE_BUSY. The key travels as a
// URI parameter (never logged); adiantum derives the real key with Argon2id.
func dsn(o Options) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(wal)")
	q.Add("_pragma", "foreign_keys(1)")
	if o.Key != "" {
		q.Set("vfs", "adiantum")
		q.Set("textkey", o.Key)
		q.Add("_pragma", "temp_store(memory)") // keep temp data out of files
	}
	u := url.URL{Scheme: "file", Path: o.Path, RawQuery: q.Encode()}
	return u.String()
}

// openFile opens one database, checking its encryption state against the
// configured key before touching it, then applies schema.
func openFile(o Options, schema string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o750); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	kind, err := detect(o.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", o.Path, err)
	}
	// Private before first use: SQLite derives the -wal/-shm modes from the
	// database file, so create it 0600 rather than chmod-ing afterwards.
	if err := restrictFiles(o.Path); err != nil {
		return nil, err
	}
	switch {
	case kind == filePlain && o.Key != "":
		return nil, fmt.Errorf("%s is not encrypted but an encryption key is configured; "+
			"refusing to open (run `imap-mcp db encrypt` with the service stopped to convert it)", o.Path)
	case kind == fileOpaque && o.Key == "":
		return nil, fmt.Errorf("%s is encrypted (or not a SQLite database) and no encryption key is configured", o.Path)
	}

	conn, err := driver.Open(dsn(o), fts5.Register)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", o.Path, err)
	}
	conn.SetMaxOpenConns(1) // SQLite WAL: one writer per process

	// Read the schema first so a wrong key surfaces as such, then apply ours.
	if _, err := conn.Exec(`SELECT count(*) FROM sqlite_master`); err != nil {
		conn.Close()
		if o.Key != "" && strings.Contains(err.Error(), "not a database") {
			return nil, fmt.Errorf("%s: wrong encryption key or corrupt file", o.Path)
		}
		return nil, fmt.Errorf("open %s: %w", o.Path, err)
	}
	if schema != "" {
		if _, err := conn.Exec(schema); err != nil {
			conn.Close()
			return nil, fmt.Errorf("apply schema to %s: %w", o.Path, err)
		}
	}
	_ = restrictFiles(o.Path) // WAL/SHM now exist; tighten any created earlier
	return conn, nil
}

// restrictFiles creates path (if missing) and sets it and any existing
// -wal/-shm sidecars to 0600.
func restrictFiles(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	return nil
}

// ensureCacheSchema creates the cache schema, or drops and recreates it when
// the stored user_version differs from cacheSchemaVersion. Only cache.db is
// ever rebuilt this way: its contents come back from IMAP on the next sync.
func ensureCacheSchema(conn *sql.DB) error {
	var v int
	if err := conn.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v != cacheSchemaVersion {
		var n int
		if err := conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			for _, obj := range legacyCacheObjects {
				if _, err := conn.Exec(`DROP ` + strings.Replace(obj, " ", " IF EXISTS ", 1)); err != nil {
					return fmt.Errorf("rebuild cache: drop %s: %w", obj, err)
				}
			}
		}
	}
	if _, err := conn.Exec(cacheSchema); err != nil {
		return fmt.Errorf("apply cache schema: %w", err)
	}
	for _, t := range droppedCacheTables {
		if _, err := conn.Exec(`DROP TABLE IF EXISTS ` + t); err != nil {
			return fmt.Errorf("drop moved table %s: %w", t, err)
		}
	}
	_, err := conn.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, cacheSchemaVersion))
	return err
}

// Repository types — each will be expanded with query methods.

type MessageRepo struct{ db *sql.DB }
type SenderRepo struct{ db *sql.DB }
type KGRepo struct{ db *sql.DB }
type VectorRepo struct{ db *sql.DB }
type FolderRepo struct{ db *sql.DB }
type AnomalyRepo struct{ db *sql.DB }
type SyncRepo struct{ db *sql.DB }
type WebhookRepo struct{ db *sql.DB }
type RuleRepo struct{ db *sql.DB }
type EnrichRepo struct{ db *sql.DB }
