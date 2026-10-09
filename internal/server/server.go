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
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/imap"
	mcpserver "github.com/dmz006/imap-mcp/internal/mcp"
	"github.com/dmz006/imap-mcp/internal/output"
	"github.com/dmz006/imap-mcp/internal/sync"
	"github.com/dmz006/imap-mcp/internal/webhook"
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
	out      *output.Writer
	log      *slog.Logger
	authn    *httpauth.Authenticator // nil = auth disabled
	webhooks *webhook.Enqueuer       // nil = webhook delivery not running
	http     *http.Server
}

// SetWebhooks attaches the webhook enqueuer used by the REST webhook routes.
func (s *Server) SetWebhooks(q *webhook.Enqueuer) { s.webhooks = q }

func New(
	cfg *config.Config,
	pool *imap.Pool,
	database *db.DB,
	b *bus.Bus,
	syncer *sync.Syncer,
	pipeline *enrichment.Pipeline,
	out *output.Writer,
	log *slog.Logger,
	authn *httpauth.Authenticator,
) *Server {
	return &Server{
		authn:    authn,
		cfg:      cfg,
		pool:     pool,
		db:       database,
		bus:      b,
		syncer:   syncer,
		pipeline: pipeline,
		out:      out,
		log:      log,
	}
}

// handler assembles the full HTTP handler: browserGuard → auth → MCP at /mcp
// and REST at /api.
func (s *Server) handler() http.Handler {
	mcpSrv := mcpserver.NewServer(s.cfg, s.pool, s.db, s.syncer, s.out, s.pipeline, s.authn != nil)
	streamable := mcpgo.NewStreamableHTTPServer(mcpSrv)
	apiSrv := api.NewServer(s.cfg, s.pool, s.db, s.bus, s.syncer, s.pipeline, s.log, s.authn)
	apiSrv.SetWebhooks(s.webhooks)

	r := chi.NewRouter()
	r.Mount("/mcp", streamable)
	r.Mount("/", apiSrv.Router())
	return browserGuard(s.cfg.Server.Host, s.authn.Middleware(r))
}

func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.http = &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	s.log.Info("server listening",
		"addr", addr,
		"mcp", fmt.Sprintf("http://%s/mcp", addr),
		"api", fmt.Sprintf("http://%s/api", addr),
		"auth", s.authn != nil,
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
