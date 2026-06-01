// Package server combines the MCP streamable HTTP transport and REST API
// onto a single HTTP server. MCP lives at /mcp, REST at /api.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/dmz006/imap-mcp/internal/api"
	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/imap"
	mcpserver "github.com/dmz006/imap-mcp/internal/mcp"
	"github.com/dmz006/imap-mcp/internal/sync"
	"github.com/go-chi/chi/v5"
	mcpgo "github.com/mark3labs/mcp-go/server"
)

// Server is the combined HTTP + MCP server.
type Server struct {
	cfg      *config.Config
	pool     *imap.Pool
	db       *db.DB
	bus      *bus.Bus
	syncer   *sync.Syncer
	pipeline *enrichment.Pipeline
	log      *slog.Logger
	http     *http.Server
}

func New(
	cfg *config.Config,
	pool *imap.Pool,
	database *db.DB,
	b *bus.Bus,
	syncer *sync.Syncer,
	pipeline *enrichment.Pipeline,
	log *slog.Logger,
) *Server {
	return &Server{
		cfg:      cfg,
		pool:     pool,
		db:       database,
		bus:      b,
		syncer:   syncer,
		pipeline: pipeline,
		log:      log,
	}
}

func (s *Server) Start(ctx context.Context) error {
	// Build MCP server
	mcpSrv := mcpserver.NewServer(s.cfg, s.pool, s.db, s.syncer)
	streamable := mcpgo.NewStreamableHTTPServer(mcpSrv)

	// Build REST API router
	apiSrv := api.NewServer(s.cfg, s.pool, s.db, s.bus, s.syncer, s.log)

	// Combine onto a single chi router
	r := chi.NewRouter()
	r.Mount("/mcp", streamable)
	r.Mount("/", apiSrv.Router())

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.http = &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	s.log.Info("server listening",
		"addr", addr,
		"mcp", fmt.Sprintf("http://%s/mcp", addr),
		"api", fmt.Sprintf("http://%s/api", addr),
	)

	errCh := make(chan error, 1)
	go func() { errCh <- s.http.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.http.Shutdown(shutCtx)
	case err := <-errCh:
		if err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}
