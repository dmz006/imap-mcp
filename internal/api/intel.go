package api

import (
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/dmz006/imap-mcp/internal/service"
)

// POST /api/search/semantic
// Body: {"query" | "reference_uid"+"folder", "account", "folder", "limit", "threshold"}
func (s *Server) handleSemanticSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account      string  `json:"account"`
		Folder       string  `json:"folder"`
		Query        string  `json:"query"`
		ReferenceUID uint32  `json:"reference_uid"`
		Limit        int     `json:"limit"`
		Threshold    float64 `json:"threshold"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w)(s.svc.SemanticSearch(r.Context(), service.SemanticParams{
		Account: req.Account, Folder: req.Folder, Query: req.Query, ReferenceUID: req.ReferenceUID,
		Limit: req.Limit, Threshold: req.Threshold,
	}))
}

// GET /api/senders?role=&domain=&limit=
func (s *Server) handleListSenders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	senders, err := s.svc.ListSenders(r.Context(), q.Get("role"), q.Get("domain"), queryInt(r, "limit", 50))
	respond(w)(map[string]any{"count": len(senders), "senders": senders}, err)
}

// GET /api/senders/{address}
func (s *Server) handleGetSender(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.GetSenderProfile(r.Context(), pathParam(r, "address")))
}

// GET /api/kg?entity=&predicate=&entity_type=&limit=
func (s *Server) handleKGQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	edges, err := s.svc.KGQuery(r.Context(), service.KGParams{
		Entity: q.Get("entity"), Predicate: q.Get("predicate"), EntityType: q.Get("entity_type"), Limit: queryInt(r, "limit", 50),
	})
	respond(w)(map[string]any{"count": len(edges), "relationships": edges}, err)
}

// GET /api/anomalies?account=&severity=&include_resolved=&limit=
func (s *Server) handleGetAnomalies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	resolved, _ := strconv.ParseBool(q.Get("include_resolved"))
	list, err := s.svc.Anomalies(r.Context(), service.AnomalyParams{
		Account: q.Get("account"), Severity: q.Get("severity"), Type: q.Get("type"), Sender: q.Get("sender"),
		IncludeResolved: resolved, Limit: queryInt(r, "limit", 20),
	})
	respond(w)(map[string]any{"count": len(list), "anomalies": list}, err)
}

// POST /api/anomalies/{id}/resolve
func (s *Server) handleResolveAnomaly(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "id must be a positive integer", http.StatusBadRequest)
		return
	}
	respond(w)(s.svc.ResolveAnomaly(r.Context(), id))
}

// replyParams reads ?account=&older_than_days=&within_days=&limit=
func replyParams(r *http.Request) service.ReplyParams {
	return service.ReplyParams{
		Account:       r.URL.Query().Get("account"),
		OlderThanDays: queryInt(r, "older_than_days", 2),
		WithinDays:    queryInt(r, "within_days", 90),
		Limit:         queryInt(r, "limit", 20),
	}
}

// GET /api/replies/needed?account=&older_than_days=&within_days=&limit=
func (s *Server) handleNeedsReply(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.NeedsReply(r.Context(), replyParams(r)))
}

// GET /api/replies/awaiting?account=&older_than_days=&within_days=&limit=
func (s *Server) handleAwaitingReply(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.AwaitingReply(r.Context(), replyParams(r)))
}

// POST /api/replies/dismiss
// Body: {"account", "thread_id"}
func (s *Server) handleDismissReply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
		Thread  string `json:"thread_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w)(s.svc.DismissReply(r.Context(), req.Account, req.Thread))
}

// GET /api/rules/suggestions?account=&limit=
func (s *Server) handleSuggestRules(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.SuggestRules(r.Context(), service.SuggestParams{
		Account: r.URL.Query().Get("account"), Limit: queryInt(r, "limit", 50),
	}))
}

// POST /api/rules/suggestions/dismiss
// Body: {"account", "target"}
func (s *Server) handleDismissSuggestion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
		Target  string `json:"target"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w)(s.svc.DismissSuggestion(r.Context(), req.Account, req.Target))
}

// GET /api/identities
func (s *Server) handleSuggestIdentities(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.SuggestIdentities(r.Context()))
}

// POST /api/identities/confirm  Body: {"address"}
func (s *Server) handleConfirmIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w)(s.svc.ConfirmIdentity(r.Context(), req.Address))
}

// POST /api/identities/reject  Body: {"address"}
func (s *Server) handleRejectIdentity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w)(s.svc.RejectIdentity(r.Context(), req.Address))
}

// GET /api/setup-check
func (s *Server) handleSetupCheck(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.SetupCheck(r.Context()))
}

// GET /api/rule-packs
func (s *Server) handleListRulePacks(w http.ResponseWriter, r *http.Request) {
	packs, err := s.svc.ListRulePacks()
	respond(w)(map[string]any{"count": len(packs), "packs": packs}, err)
}

// POST /api/rule-packs/{name}/import  Body: {"account", "dest"}
func (s *Server) handleImportRulePack(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
		Dest    string `json:"dest"`
	}
	if !decode(w, r, &req) {
		return
	}
	respond(w)(s.svc.ImportRulePack(r.Context(), req.Account, chi.URLParam(r, "name"), req.Dest))
}

// POST /api/verdicts/eval  Body: {"url", "models": [...], "per_class"}
func (s *Server) handleStartVerdictEval(w http.ResponseWriter, r *http.Request) {
	var req service.EvalParams
	if !decode(w, r, &req) {
		return
	}
	respond(w)(s.svc.StartVerdictEval(r.Context(), req))
}

// GET /api/verdicts/eval?id=  (latest when no id)
func (s *Server) handleVerdictEval(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.VerdictEval(r.Context(), int64(queryInt(r, "id", 0))))
}

// POST /api/unsubscribe (unsubscribe scope; archive_to also needs write, D57;
// a mailto unsubscribe also needs send, D40)
// Body: {"account", "sender" | "uid", "folder", "confirm", "archive_to"}
func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account   string `json:"account"`
		Sender    string `json:"sender"`
		UID       uint32 `json:"uid"`
		Folder    string `json:"folder"`
		Confirm   bool   `json:"confirm"`
		ArchiveTo string `json:"archive_to"`
	}
	if !decode(w, r, &req) {
		return
	}
	p := httpauth.FromContext(r.Context())
	if s.authn != nil && req.ArchiveTo != "" && !p.Has(httpauth.ScopeWrite) {
		http.Error(w, "forbidden: archive_to also needs scope write", http.StatusForbidden)
		return
	}
	respond(w)(s.svc.Unsubscribe(r.Context(), service.UnsubscribeParams{
		Account: req.Account, Sender: req.Sender, UID: req.UID, Folder: req.Folder,
		Confirm: req.Confirm, ArchiveTo: req.ArchiveTo, CanSend: s.authn == nil || p.Has(httpauth.ScopeSend),
	}))
}

// GET /api/unsubscribes?account=&dry_runs=&limit=
func (s *Server) handleListUnsubscribes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	respond(w)(s.svc.ListUnsubscribes(r.Context(), q.Get("account"), q.Get("dry_runs") == "true", queryInt(r, "limit", 100)))
}

// GET /api/held?account=
func (s *Server) handleHeldSenders(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.HeldSenders(r.Context(), r.URL.Query().Get("account")))
}

// POST /api/held/{account}/decide (write; a preview unless confirm, D62)
// Body: {"decisions": [{sender, action, dest}], "confirm"}
func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	var req service.ScreenParams
	if !decode(w, r, &req) {
		return
	}
	req.Account = chi.URLParam(r, "account")
	respond(w)(s.svc.ScreenVia(r.Context(), req, "rest"))
}
