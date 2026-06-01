// Package api provides the REST API server alongside the MCP server.
// Both run on the same port: MCP at /mcp, REST at /api.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/sync"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	cfg    *config.Config
	pool   *imap.Pool
	db     *db.DB
	bus    *bus.Bus
	syncer *sync.Syncer
	log    *slog.Logger
}

func NewServer(cfg *config.Config, pool *imap.Pool, database *db.DB, b *bus.Bus, syncer *sync.Syncer, log *slog.Logger) *Server {
	return &Server{
		cfg:    cfg,
		pool:   pool,
		db:     database,
		bus:    b,
		syncer: syncer,
		log:    log,
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	// ── Health ────────────────────────────────────────────────────────────────
	r.Get("/api/health", s.handleHealth)

	// ── Accounts ─────────────────────────────────────────────────────────────
	r.Get("/api/accounts", s.handleListAccounts)
	r.Post("/api/accounts/{account}/sync", s.handleSyncAccount)

	// ── Folders ──────────────────────────────────────────────────────────────
	r.Get("/api/accounts/{account}/folders", s.handleListFolders)

	// ── Messages ─────────────────────────────────────────────────────────────
	r.Get("/api/accounts/{account}/folders/{folder}/messages", s.handleListMessages)
	r.Get("/api/accounts/{account}/folders/{folder}/messages/{uid}", s.handleGetMessage)
	r.Delete("/api/accounts/{account}/folders/{folder}/messages/{uid}", s.handleDeleteMessage)
	r.Put("/api/accounts/{account}/folders/{folder}/messages/{uid}/flags", s.handleSetFlags)
	r.Post("/api/accounts/{account}/folders/{folder}/messages/{uid}/move", s.handleMoveMessage)

	// ── Search ───────────────────────────────────────────────────────────────
	r.Get("/api/search", s.handleSearch)
	r.Post("/api/search/semantic", s.handleSemanticSearch)

	// ── Analytics (cache-based, fast) ────────────────────────────────────────
	r.Get("/api/accounts/{account}/stats", s.handleAccountStats)
	r.Get("/api/senders", s.handleListSenders)
	r.Get("/api/senders/{address}", s.handleGetSender)
	r.Get("/api/kg", s.handleKGQuery)
	r.Get("/api/anomalies", s.handleGetAnomalies)
	r.Get("/api/enrichment/status", s.handleEnrichmentStatus)
	r.Post("/api/enrichment/trigger", s.handleTriggerEnrichment)

	// ── Webhooks ─────────────────────────────────────────────────────────────
	r.Get("/api/webhooks", s.handleListWebhooks)
	r.Post("/api/webhooks", s.handleCreateWebhook)
	r.Delete("/api/webhooks/{id}", s.handleDeleteWebhook)

	// ── Rules ────────────────────────────────────────────────────────────────
	r.Get("/api/rules", s.handleListRules)
	r.Post("/api/rules", s.handleCreateRule)
	r.Put("/api/rules/{id}", s.handleUpdateRule)
	r.Delete("/api/rules/{id}", s.handleDeleteRule)
	r.Post("/api/rules/{id}/test", s.handleTestRule)

	// ── Query DSL (algorithmic layer entry point) ─────────────────────────────
	r.Post("/api/query", s.handleQuery)

	// ── Event stream (future: streaming API / federation) ────────────────────
	r.Get("/api/events", s.handleEventStream)

	return r
}

// ── Implemented handlers ──────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts := s.pool.AccountNames()
	writeJSON(w, map[string]any{
		"status":   "ok",
		"version":  config.Version,
		"accounts": len(accounts),
	})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	type info struct {
		Name      string `json:"name"`
		Default   bool   `json:"default"`
		Connected bool   `json:"connected"`
	}
	names := s.pool.AccountNames()
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	result := make([]info, 0, len(s.cfg.Accounts))
	for _, a := range s.cfg.Accounts {
		result = append(result, info{Name: a.Name, Default: a.Default, Connected: nameSet[a.Name]})
	}
	writeJSON(w, result)
}

// ── Stub handlers (iteration 2) ───────────────────────────────────────────────

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
	writeJSON(w, map[string]string{"error": "not yet implemented — coming in iteration 2"})
}

func (s *Server) handleSyncAccount(w http.ResponseWriter, r *http.Request)     { notImplemented(w, r) }
func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request)     { notImplemented(w, r) }
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request)    { notImplemented(w, r) }
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request)      { notImplemented(w, r) }
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request)   { notImplemented(w, r) }
func (s *Server) handleSetFlags(w http.ResponseWriter, r *http.Request)        { notImplemented(w, r) }
func (s *Server) handleMoveMessage(w http.ResponseWriter, r *http.Request)     { notImplemented(w, r) }
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request)          { notImplemented(w, r) }
func (s *Server) handleSemanticSearch(w http.ResponseWriter, r *http.Request)  { notImplemented(w, r) }
func (s *Server) handleAccountStats(w http.ResponseWriter, r *http.Request)    { notImplemented(w, r) }
func (s *Server) handleListSenders(w http.ResponseWriter, r *http.Request)     { notImplemented(w, r) }
func (s *Server) handleGetSender(w http.ResponseWriter, r *http.Request)       { notImplemented(w, r) }
func (s *Server) handleKGQuery(w http.ResponseWriter, r *http.Request)         { notImplemented(w, r) }
func (s *Server) handleGetAnomalies(w http.ResponseWriter, r *http.Request)    { notImplemented(w, r) }
func (s *Server) handleEnrichmentStatus(w http.ResponseWriter, r *http.Request){ notImplemented(w, r) }
func (s *Server) handleTriggerEnrichment(w http.ResponseWriter, r *http.Request){ notImplemented(w, r)}
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request)    { notImplemented(w, r) }
func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request)   { notImplemented(w, r) }
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request)   { notImplemented(w, r) }
func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request)       { notImplemented(w, r) }
func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request)      { notImplemented(w, r) }
func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request)      { notImplemented(w, r) }
func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request)      { notImplemented(w, r) }
func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request)        { notImplemented(w, r) }
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request)           { notImplemented(w, r) }
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request)     { notImplemented(w, r) }

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
