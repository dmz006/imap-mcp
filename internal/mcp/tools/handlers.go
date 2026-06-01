// Package tools contains MCP tool definitions and their handler implementations.
package tools

import (
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/sync"
)

// Handlers holds all dependencies shared by MCP tool handlers.
type Handlers struct {
	cfg    *config.Config
	pool   *imap.Pool
	db     *db.DB
	syncer *sync.Syncer
}

func NewHandlers(cfg *config.Config, pool *imap.Pool, database *db.DB, syncer *sync.Syncer) *Handlers {
	return &Handlers{
		cfg:    cfg,
		pool:   pool,
		db:     database,
		syncer: syncer,
	}
}
