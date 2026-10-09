package api

import (
	"net/http"
	"strconv"

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
		Account: q.Get("account"), Severity: q.Get("severity"), IncludeResolved: resolved, Limit: queryInt(r, "limit", 20),
	})
	respond(w)(map[string]any{"count": len(list), "anomalies": list}, err)
}
