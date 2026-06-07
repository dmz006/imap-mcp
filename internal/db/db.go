// Package db manages the local SQLite database: message cache, vector store,
// sender profiles, temporal knowledge graph, and enrichment queue.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite connection and exposes typed repositories.
type DB struct {
	sql *sql.DB

	Messages  *MessageRepo
	Senders   *SenderRepo
	KG        *KGRepo
	Vectors   *VectorRepo
	Folders   *FolderRepo
	Anomalies *AnomalyRepo
	Sync      *SyncRepo
	Webhooks  *WebhookRepo
	Rules     *RuleRepo
	Enrich    *EnrichRepo
	Nonces    *NonceRepo
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_journal=WAL&_fk=on&_timeout=5000", path)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	conn.SetMaxOpenConns(1) // SQLite WAL: one writer, many readers via separate conns

	db := &DB{sql: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	db.Messages = &MessageRepo{db: conn}
	db.Senders = &SenderRepo{db: conn}
	db.KG = &KGRepo{db: conn}
	db.Vectors = &VectorRepo{db: conn}
	db.Folders = &FolderRepo{db: conn}
	db.Anomalies = &AnomalyRepo{db: conn}
	db.Sync = &SyncRepo{db: conn}
	db.Webhooks = &WebhookRepo{db: conn}
	db.Rules = &RuleRepo{db: conn}
	db.Enrich = &EnrichRepo{db: conn}
	db.Nonces = &NonceRepo{db: conn}

	return db, nil
}

func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) SQL() *sql.DB { return db.sql }

func (db *DB) migrate() error {
	_, err := db.sql.Exec(schema)
	return err
}

// Placeholder repository types — each will be expanded with query methods.

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
