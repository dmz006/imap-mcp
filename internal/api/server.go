// Package api provides the REST API server alongside the MCP server.
// Both run on the same port: MCP at /mcp, REST at /api.
package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/imap"
	imapsmtp "github.com/dmz006/imap-mcp/internal/smtp"
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

	// SSE fan-out: one bus subscription drives N connected clients.
	sseMu    sync.RWMutex
	sseChans []chan bus.Event
}

func NewServer(cfg *config.Config, pool *imap.Pool, database *db.DB, b *bus.Bus, syncer *imapsync.Syncer, log *slog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		pool:   pool,
		db:     database,
		bus:    b,
		syncer: syncer,
		log:    log,
	}
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

	// ── Event stream ────────────────────────────────────────────────────────
	r.Get("/api/events", s.handleEventStream)

	// ── Send message ─────────────────────────────────────────────────────────
	r.Post("/api/accounts/{account}/messages/send", s.handleSendMessage)

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
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) { notImplemented(w, r) }

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
// Body: {"to":"addr","subject":"s","body":"b","cc":"optional"}
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	accountName := chi.URLParam(r, "account")
	var req struct {
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		Cc      string `json:"cc,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.To == "" || req.Subject == "" || req.Body == "" {
		http.Error(w, "to, subject, and body are required", http.StatusBadRequest)
		return
	}

	acct := s.cfg.DefaultAccount()
	if accountName != "" && accountName != "_default" {
		a, err := s.cfg.Account(accountName)
		if err != nil {
			http.Error(w, fmt.Sprintf("account %q not found", accountName), http.StatusNotFound)
			return
		}
		acct = a
	}

	smtpCfg := acct.ResolvedSMTP()
	if smtpCfg == nil {
		http.Error(w, fmt.Sprintf("account %q has no smtp config (receive-only)", acct.Name), http.StatusUnprocessableEntity)
		return
	}
	sender, err := imapsmtp.NewSender(smtpCfg)
	if err != nil {
		http.Error(w, "smtp setup: "+err.Error(), http.StatusInternalServerError)
		return
	}

	splitAddrs := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	msg := imapsmtp.Message{
		To:      splitAddrs(req.To),
		Cc:      splitAddrs(req.Cc),
		Subject: req.Subject,
		Body:    req.Body,
	}
	if err := sender.Send(msg); err != nil {
		http.Error(w, "send failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"status": "sent", "from": smtpCfg.From, "account": acct.Name})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
