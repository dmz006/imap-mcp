package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/service"
)

// writeError maps a service error kind to an HTTP status. Bodies stay plain
// text, as the send endpoint has always returned (datawatch relies on it).
func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch service.KindOf(err) {
	case service.KindInvalid:
		code = http.StatusBadRequest
	case service.KindNotFound:
		code = http.StatusNotFound
	case service.KindUnprocessable:
		code = http.StatusUnprocessableEntity
	case service.KindUnavailable:
		code = http.StatusServiceUnavailable
	case service.KindUpstream:
		code = http.StatusBadGateway
	}
	http.Error(w, err.Error(), code)
}

// respond returns a writer for a (value, error) result.
func respond(w http.ResponseWriter) func(any, error) {
	return func(v any, err error) {
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, v)
	}
}

// decode reads a JSON body, answering 400 on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// pathParam returns an unescaped route parameter, so folder names containing
// "/" (e.g. "[Gmail]/Sent Mail") can be sent as %2F.
func pathParam(r *http.Request, name string) string {
	v := chi.URLParam(r, name)
	if u, err := url.PathUnescape(v); err == nil {
		return u
	}
	return v
}

func uidParam(w http.ResponseWriter, r *http.Request) (uint32, bool) {
	n, err := strconv.ParseUint(chi.URLParam(r, "uid"), 10, 32)
	if err != nil || n == 0 {
		http.Error(w, "uid must be a positive integer", http.StatusBadRequest)
		return 0, false
	}
	return uint32(n), true
}

func queryInt(r *http.Request, key string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return n
	}
	return def
}

// POST /api/accounts/{account}/sync
func (s *Server) handleSyncAccount(w http.ResponseWriter, r *http.Request) {
	account := pathParam(r, "account")
	folders, err := s.svc.SyncAccount(r.Context(), account)
	respond(w)(map[string]any{"account": account, "folders": folders}, err)
}

// GET /api/accounts/{account}/stats
func (s *Server) handleAccountStats(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.AccountStats(r.Context(), pathParam(r, "account")))
}

// GET /api/accounts/{account}/folders
func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.ListFolders(r.Context(), pathParam(r, "account")))
}

// GET /api/accounts/{account}/folders/{folder}/messages?limit=&offset=&order=asc|desc
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	respond(w)(s.svc.ListMessages(r.Context(), service.ListMessagesParams{
		Account:   pathParam(r, "account"),
		Folder:    pathParam(r, "folder"),
		Limit:     queryInt(r, "limit", 50),
		Offset:    queryInt(r, "offset", 0),
		Ascending: r.URL.Query().Get("order") == "asc",
	}))
}

// GET /api/accounts/{account}/folders/{folder}/messages/{uid}
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	uid, ok := uidParam(w, r)
	if !ok {
		return
	}
	respond(w)(s.svc.GetMessage(r.Context(), pathParam(r, "account"), pathParam(r, "folder"), uid))
}

// DELETE /api/accounts/{account}/folders/{folder}/messages/{uid}?permanent=true
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	uid, ok := uidParam(w, r)
	if !ok {
		return
	}
	permanent, _ := strconv.ParseBool(r.URL.Query().Get("permanent"))
	respond(w)(s.svc.DeleteMessage(r.Context(), pathParam(r, "account"), pathParam(r, "folder"), uid, permanent))
}

// PUT /api/accounts/{account}/folders/{folder}/messages/{uid}/flags  Body: {"add":"seen,flagged","remove":"..."}
func (s *Server) handleSetFlags(w http.ResponseWriter, r *http.Request) {
	uid, ok := uidParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Add    string `json:"add"`
		Remove string `json:"remove"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.svc.SetFlags(r.Context(), pathParam(r, "account"), pathParam(r, "folder"), uid, req.Add, req.Remove)
	respond(w)(map[string]any{"uid": uid, "status": "updated"}, err)
}

// POST /api/accounts/{account}/folders/{folder}/messages/{uid}/move  Body: {"destination":"Archive"}
func (s *Server) handleMoveMessage(w http.ResponseWriter, r *http.Request) {
	uid, ok := uidParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Destination string `json:"destination"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.svc.MoveMessage(r.Context(), pathParam(r, "account"), pathParam(r, "folder"), uid, req.Destination)
	respond(w)(map[string]any{"uid": uid, "status": "moved", "destination": req.Destination}, err)
}

// GET /api/search?account=&folder=&from=&subject=&text=&since=&before=&flags=&limit=
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	respond(w)(s.svc.Search(r.Context(), service.SearchParams{
		Account: q.Get("account"), Folder: q.Get("folder"),
		From: q.Get("from"), Subject: q.Get("subject"), Text: q.Get("text"),
		Since: q.Get("since"), Before: q.Get("before"), Flags: q.Get("flags"),
		Limit: queryInt(r, "limit", 50),
	}))
}

// ruleBody is the JSON shape for creating/updating a rule. Active defaults
// to true when omitted.
type ruleBody struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Conditions  db.RuleConditions `json:"conditions"`
	Actions     []db.RuleAction   `json:"actions"`
	Active      *bool             `json:"active"`
	Priority    int               `json:"priority"`
}

func (b ruleBody) rule(id int64) *db.Rule {
	active := true
	if b.Active != nil {
		active = *b.Active
	}
	return &db.Rule{ID: id, Name: b.Name, Description: b.Description, Conditions: b.Conditions, Actions: b.Actions, Active: active, Priority: b.Priority}
}

func ruleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "id must be a positive integer", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// GET /api/rules
func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.svc.ListRules(r.Context())
	respond(w)(map[string]any{"count": len(rules), "rules": rules}, err)
}

// POST /api/rules
func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var b ruleBody
	if !decode(w, r, &b) {
		return
	}
	id, err := s.svc.CreateRule(r.Context(), b.rule(0))
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"id": id, "name": b.Name})
}

// PUT /api/rules/{id}  (full replacement)
func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(w, r)
	if !ok {
		return
	}
	var b ruleBody
	if !decode(w, r, &b) {
		return
	}
	err := s.svc.UpdateRule(r.Context(), b.rule(id))
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w)(s.svc.GetRule(r.Context(), id))
}

// DELETE /api/rules/{id}
func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(w, r)
	if !ok {
		return
	}
	respond(w)(map[string]any{"id": id, "status": "deleted"}, s.svc.DeleteRule(r.Context(), id))
}

// POST /api/rules/{id}/test — dry run: counts matches, changes nothing.
func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(w, r)
	if !ok {
		return
	}
	respond(w)(s.svc.TestRule(r.Context(), id))
}
