// Package api provides the REST API server alongside the MCP server.
// Both run on the same port: MCP at /mcp, REST at /api.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/mcp/tools"
	"github.com/dmz006/imap-mcp/internal/query"
	"github.com/dmz006/imap-mcp/internal/service"
	imapsync "github.com/dmz006/imap-mcp/internal/sync"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	cfg    *config.Config
	pool   *imap.Pool
	db     *db.DB
	bus    *bus.Bus
	syncer *imapsync.Syncer
	log    *slog.Logger
	authn  *httpauth.Authenticator // nil = auth disabled
	enrich *enrichment.Pipeline    // nil when not running
	svc    *service.Service        // shared operation layer (D13)

	// SSE fan-out: one bus subscription drives N connected clients.
	sseMu    sync.RWMutex
	sseChans []chan bus.Event
}

// NewServer builds the REST API. authn may be nil when auth is disabled; every
// route except /api/health declares the scope it requires (AGENT.md D13a).
func NewServer(cfg *config.Config, pool *imap.Pool, database *db.DB, b *bus.Bus, syncer *imapsync.Syncer, pipeline *enrichment.Pipeline, log *slog.Logger, authn *httpauth.Authenticator) *Server {
	s := &Server{
		enrich: pipeline,
		authn:  authn,
		cfg:    cfg,
		pool:   pool,
		db:     database,
		bus:    b,
		syncer: syncer,
		log:    log,
	}
	s.svc = service.New(cfg, pool, database, syncer, pipeline)
	if b != nil {
		b.SubscribeAll(s.broadcastSSE)
	}
	return s
}

// broadcastSSE fans out each bus event to all active SSE connections.
func (s *Server) broadcastSSE(e bus.Event) {
	s.sseMu.RLock()
	chans := make([]chan bus.Event, len(s.sseChans))
	copy(chans, s.sseChans)
	s.sseMu.RUnlock()
	for _, ch := range chans {
		select {
		case ch <- e:
		default: // drop for slow clients rather than block the bus
		}
	}
}

func (s *Server) addSSEClient(ch chan bus.Event) {
	s.sseMu.Lock()
	s.sseChans = append(s.sseChans, ch)
	s.sseMu.Unlock()
}

func (s *Server) removeSSEClient(ch chan bus.Event) {
	s.sseMu.Lock()
	out := s.sseChans[:0]
	for _, c := range s.sseChans {
		if c != ch {
			out = append(out, c)
		}
	}
	s.sseChans = out
	s.sseMu.Unlock()
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Per-route scope requirements (no-ops when auth is disabled).
	read := s.authn.RequireScope(httpauth.ScopeRead)
	write := s.authn.RequireScope(httpauth.ScopeWrite)
	send := s.authn.RequireScope(httpauth.ScopeSend)
	admin := s.authn.RequireScope(httpauth.ScopeAdmin)

	// ── Event stream (long-lived: no request timeout) ───────────────────────
	r.With(read).Get("/api/events", s.handleEventStream)

	// ── Content downloads (D27): write scope, a longer timeout for big exports.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(5 * time.Minute))
		r.With(write).Get("/api/accounts/{account}/folders/{folder}/messages/{uid}/attachments/{part}", s.handleDownloadAttachment)
		r.With(write).Get("/api/accounts/{account}/folders/{folder}/messages/{uid}/export.eml", s.handleExportEML)
		r.With(write).Post("/api/export", s.handleExportMbox)
	})

	// Every other route gets a 30 s request timeout.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))

		// ── Health ────────────────────────────────────────────────────────────────
		r.Get("/api/health", s.handleHealth)

		// ── Accounts ─────────────────────────────────────────────────────────────
		r.With(read).Get("/api/accounts", s.handleListAccounts)
		r.With(admin).Post("/api/accounts/{account}/sync", s.handleSyncAccount)

		// ── Folders ──────────────────────────────────────────────────────────────
		r.With(read).Get("/api/accounts/{account}/folders", s.handleListFolders)

		// ── Messages ─────────────────────────────────────────────────────────────
		r.With(read).Get("/api/accounts/{account}/folders/{folder}/messages", s.handleListMessages)
		r.With(read).Get("/api/accounts/{account}/folders/{folder}/messages/{uid}", s.handleGetMessage)
		r.With(write).Delete("/api/accounts/{account}/folders/{folder}/messages/{uid}", s.handleDeleteMessage)
		r.With(write).Put("/api/accounts/{account}/folders/{folder}/messages/{uid}/flags", s.handleSetFlags)
		r.With(write).Post("/api/accounts/{account}/folders/{folder}/messages/{uid}/move", s.handleMoveMessage)
		r.With(read).Get("/api/accounts/{account}/folders/{folder}/messages/{uid}/attachments", s.handleListAttachments)
		r.With(read).Get("/api/threads/{thread_id}", s.handleGetThread)

		// ── Search ───────────────────────────────────────────────────────────────
		r.With(read).Get("/api/search", s.handleSearch)
		r.With(read).Post("/api/search/semantic", s.handleSemanticSearch)
		r.With(read).Get("/api/search/cross", s.handleCrossSearch)

		// ── Analytics (cache-based, fast) ────────────────────────────────────────
		r.With(read).Get("/api/accounts/{account}/stats", s.handleAccountStats)
		r.With(read).Get("/api/senders", s.handleListSenders)
		r.With(read).Get("/api/senders/{address}", s.handleGetSender)
		r.With(read).Get("/api/kg", s.handleKGQuery)
		r.With(read).Get("/api/anomalies", s.handleGetAnomalies)
		r.With(write).Post("/api/anomalies/{id}/resolve", s.handleResolveAnomaly)
		r.With(read).Get("/api/replies/needed", s.handleNeedsReply)
		r.With(read).Get("/api/replies/awaiting", s.handleAwaitingReply)
		r.With(write).Post("/api/replies/dismiss", s.handleDismissReply)
		r.With(read).Get("/api/enrichment/status", s.handleEnrichmentStatus)
		r.With(read).Get("/api/intelligence/status", s.handleIntelStatus)
		r.With(admin).Post("/api/enrichment/trigger", s.handleTriggerEnrichment)

		// ── Cache maintenance (cache only, never the mailbox) ───────────────
		r.With(admin).Post("/api/cache/sweep", s.handleCacheSweep)

		// ── Webhooks ─────────────────────────────────────────────────────────────
		r.With(admin).Get("/api/webhooks", s.handleListWebhooks)
		r.With(admin).Post("/api/webhooks", s.handleCreateWebhook)
		r.With(admin).Delete("/api/webhooks/{id}", s.handleDeleteWebhook)
		r.With(admin).Post("/api/webhooks/{id}/enable", s.handleEnableWebhook)
		r.With(admin).Post("/api/webhooks/{id}/test", s.handleTestWebhook)
		r.With(admin).Get("/api/webhooks/{id}/deliveries", s.handleWebhookDeliveries)

		// ── Rules ────────────────────────────────────────────────────────────────
		r.With(read).Get("/api/rules", s.handleListRules)
		r.With(read).Get("/api/rules/suggestions", s.handleSuggestRules)
		r.With(read).Get("/api/identities", s.handleSuggestIdentities)
		r.With(write).Post("/api/identities/confirm", s.handleConfirmIdentity)
		r.With(write).Post("/api/identities/reject", s.handleRejectIdentity)
		r.With(write).Post("/api/rules/suggestions/dismiss", s.handleDismissSuggestion)
		r.With(write).Post("/api/rules", s.handleCreateRule)
		r.With(write).Put("/api/rules/{id}", s.handleUpdateRule)
		r.With(write).Delete("/api/rules/{id}", s.handleDeleteRule)
		r.With(write).Post("/api/rules/{id}/test", s.handleTestRule)

		// ── Query DSL (algorithmic layer entry point) ─────────────────────────────
		r.With(admin).Get("/api/query", s.handleQueryDescribe)
		r.With(admin).Post("/api/query", s.handleQuery)

		// ── Send message ─────────────────────────────────────────────────────────
		r.With(send).Post("/api/accounts/{account}/messages/send", s.handleSendMessage)
	})

	return r
}

// ── Implemented handlers ──────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts := s.pool.AccountNames()
	writeJSON(w, map[string]any{
		"status":   "ok",
		"version":  config.Version,
		"accounts": len(accounts),
		"auth":     map[bool]string{true: "enabled", false: "disabled"}[s.authn != nil],
		"sync": map[string]any{
			"interval_minutes":      s.cfg.Sync.IntervalMinutes,
			"window_days":           s.cfg.WindowDays(nil, ""),
			"max_message_mb":        s.cfg.Sync.MaxMessageMB,
			"keep_flagged":          s.cfg.Sync.KeepFlagged,
			"vacuum_interval_hours": s.cfg.Sync.VacuumIntervalHours,
			"folders":               s.cfg.SyncFolders(nil),
		},
		"enrichment":   s.enrichmentHealth(r),
		"intelligence": s.intelHealth(r),
		"rules":        s.rulesHealth(),
		"tools": map[string]int{
			"attachment_inline_kb": s.cfg.Tools.AttachmentInlineKB,
			"attachment_max_mb":    s.cfg.Tools.AttachmentMaxMB,
			"export_max_messages":  s.cfg.Tools.ExportMaxMessages,
			"export_max_mb":        s.cfg.Tools.ExportMaxMB,
		},
		"storage": map[string]bool{
			"state_encrypted": s.cfg.DB.EncryptionKey != "",
			"cache_encrypted": s.cfg.DB.Cache.EncryptionKey != "",
		},
	})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	type info struct {
		Name      string `json:"name"`
		Default   bool   `json:"default"`
		Connected bool   `json:"connected"`
	}
	result := make([]info, 0, len(s.cfg.Accounts))
	for _, a := range s.cfg.Accounts {
		// Probe live state (NOOP) rather than trusting pool membership, so a
		// silently-dropped connection is reported as disconnected.
		result = append(result, info{Name: a.Name, Default: a.Default, Connected: s.pool.Probe(a.Name)})
	}
	writeJSON(w, result)
}

// enrichmentHealth summarises enrichment config and queue state for /api/health.
func (s *Server) enrichmentHealth(r *http.Request) any {
	e := s.cfg.Enrichment
	out := map[string]any{
		"enabled":             e.Enabled,
		"concurrency":         e.Concurrency,
		"backfill_per_minute": e.BackfillPerMinute,
		"backfill_window":     e.BackfillWindow,
		"max_attempts":        e.MaxAttempts,
		"backoff_max_seconds": e.BackoffMaxSeconds,
		"yield":               e.Yield.Enabled,
	}
	if s.enrich != nil {
		if st, err := s.enrich.Stats(r.Context()); err == nil {
			out["embed_provider"], out["classify_provider"] = st.Embed, st.Classify
			out["pending"], out["done_last_hour"], out["errors"] = st.Pending, st.DoneLastHour, st.Errors
			out["oldest_pending_seconds"] = st.OldestPendingSecs
			if st.BackfillPaused != "" {
				out["backfill_paused"] = st.BackfillPaused
			}
			if st.BackoffSeconds > 0 {
				out["backoff_seconds"] = st.BackoffSeconds
			}
		}
	}
	return out
}

// handleEnrichmentStatus: GET /api/enrichment/status
func (s *Server) handleEnrichmentStatus(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.EnrichmentStats(r.Context()))
}

// handleTriggerEnrichment: POST /api/enrichment/trigger  Body: {"limit": 50}
func (s *Server) handleTriggerEnrichment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Limit int `json:"limit"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Limit <= 0 {
		req.Limit = 50
	}
	queued, err := s.svc.TriggerEnrichment(req.Limit)
	respond(w)(map[string]any{"triggered": queued, "limit": req.Limit}, err)
}

// handleCacheSweep is the REST form of cache_sweep (D7).
// POST /api/cache/sweep
// Body: {"account","folder","older_than_days","errors_only","all","dry_run"}; dry_run defaults to true.
func (s *Server) handleCacheSweep(w http.ResponseWriter, r *http.Request) {
	args := map[string]any{}
	if !decode(w, r, &args) {
		return
	}
	f, dryRun := tools.SweepFilterFromArgs(args)
	respond(w)(s.svc.SweepCache(r.Context(), f, dryRun))
}

// ── Query DSL ─────────────────────────────────────────────────────────────────

// POST /api/query — JSON DSL over fixed cache views (D17; docs/query.md).
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var q query.Query
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	respond(w)(s.svc.Query(r.Context(), q))
}

// GET /api/query — the views, fields, operators and aggregates available.
func (s *Server) handleQueryDescribe(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, query.Describe())
}

// handleEventStream streams bus events as SSE. Each event is one JSON line
// prefixed with "data: " per the SSE spec. Clients reconnect on disconnect;
// the server does not buffer missed events.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// The stream outlives the server's WriteTimeout; lift it for this response.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// Send headers now so clients see 200 without waiting for the first event.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := make(chan bus.Event, 32)
	s.addSSEClient(ch)
	defer s.removeSSEClient(ch)

	// Send a heartbeat comment every 15 s to keep the connection alive through
	// proxies that close idle HTTP responses.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case e := <-ch:
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleSendMessage sends an outbound email via the account's SMTP config.
// POST /api/accounts/{account}/messages/send
// Body: {"to":"addr","subject":"s","body":"b","cc":"optional"}; account may be _default.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Cc      string `json:"cc,omitempty"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := s.svc.Send(r.Context(), service.SendParams{
		Account: chi.URLParam(r, "account"), To: req.To, Cc: req.Cc, Subject: req.Subject, Body: req.Body,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": res.Status, "from": res.From, "account": res.Account})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// intelHealth is the header scanner's progress for /api/health (counts only).
func (s *Server) intelHealth(r *http.Request) any {
	st, err := s.svc.IntelStats(r.Context())
	if err != nil {
		return map[string]any{"enabled": s.cfg.Intel.On()}
	}
	return st.Anonymous() // health is unauthenticated: no account names (D29)
}

// rulesHealth reports the held-mail digest settings and counts (D30, D31).
// Health is unauthenticated: counts only.
func (s *Server) rulesHealth() any {
	st, err := s.svc.HoldStatus()
	if err != nil {
		return map[string]any{"hold_digest": s.cfg.Rules.HoldDigestOn(), "hold_digest_hour": s.cfg.Rules.HoldDigestHour}
	}
	return st
}

// GET /api/intelligence/status — scan progress with account names (D29).
func (s *Server) handleIntelStatus(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.IntelStats(r.Context()))
}
