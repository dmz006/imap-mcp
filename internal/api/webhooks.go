package api

import (
	"net/http"
	"strconv"

	"github.com/dmz006/imap-mcp/internal/webhook"
)

// SetWebhooks attaches the webhook enqueuer so test pings can be queued and
// subscriber changes take effect immediately.
func (s *Server) SetWebhooks(q *webhook.Enqueuer) { s.svc.SetWebhooks(q) }

// GET /api/webhooks — secrets are never listed.
func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.svc.ListWebhooks(r.Context())
	respond(w)(map[string]any{"count": len(hooks), "webhooks": hooks, "events": webhook.Events}, err)
}

// POST /api/webhooks {url, events, payload} → 201 with the signing secret (shown once).
func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var b struct {
		URL     string   `json:"url"`
		Events  []string `json:"events"`
		Payload string   `json:"payload"`
	}
	if !decode(w, r, &b) {
		return
	}
	created, err := s.svc.CreateWebhook(r.Context(), b.URL, b.Events, b.Payload)
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

// DELETE /api/webhooks/{id}
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(w, r)
	if !ok {
		return
	}
	respond(w)(map[string]any{"id": id, "status": "deleted"}, s.svc.DeleteWebhook(r.Context(), id))
}

// POST /api/webhooks/{id}/enable — re-activate after auto-disable.
func (s *Server) handleEnableWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(w, r)
	if !ok {
		return
	}
	respond(w)(s.svc.EnableWebhook(r.Context(), id))
}

// POST /api/webhooks/{id}/test — queue a webhook.test ping.
func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(w, r)
	if !ok {
		return
	}
	hook, err := s.svc.TestWebhook(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"id": hook.ID, "status": "queued", "event": webhook.EventTest})
}

// GET /api/webhooks/{id}/deliveries?limit=N — newest first.
func (s *Server) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ds, err := s.svc.WebhookDeliveries(r.Context(), id, limit)
	respond(w)(map[string]any{"count": len(ds), "deliveries": ds}, err)
}
