// Package tools contains MCP tool definitions and their handler implementations.
package tools

import (
	"bytes"
	"encoding/json"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/output"
	"github.com/dmz006/imap-mcp/internal/service"
	"github.com/dmz006/imap-mcp/internal/sync"
	"github.com/mark3labs/mcp-go/mcp"
)

// Handlers holds all dependencies shared by MCP tool handlers.
type Handlers struct {
	cfg    *config.Config
	pool   *imap.Pool
	db     *db.DB
	syncer *sync.Syncer
	out    *output.Writer
	svc    *service.Service // shared with the REST API (D13)
}

// SetPipeline attaches the enrichment pipeline for enrichment_status and
// trigger_enrichment.
func (h *Handlers) SetPipeline(p *enrichment.Pipeline) { h.svc.SetPipeline(p) }

// Service exposes the shared operation layer.
func (h *Handlers) Service() *service.Service { return h.svc }

// result renders a service result as JSON, or its error as a tool error.
// structuredContent must be a JSON object, so lists (list_folders) are
// returned as text only; clients reject an array there.
func result(v any, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) {
		return mcp.NewToolResultText(string(b)), nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{mcp.NewTextContent(string(b))},
		StructuredContent: v,
	}, nil
}

// text renders a confirmation message, or the error as a tool error.
func text(msg string, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(msg), nil
}

func NewHandlers(cfg *config.Config, pool *imap.Pool, database *db.DB, syncer *sync.Syncer, out *output.Writer) *Handlers {
	return &Handlers{
		cfg:    cfg,
		pool:   pool,
		db:     database,
		syncer: syncer,
		out:    out,
		svc:    service.New(cfg, pool, database, syncer, nil),
	}
}
