package api

// handlers_content.go — threads, attachments, export and cross-account
// search (AGENT.md D23–D27). Downloads stream bytes back and write nothing on
// the server.

import (
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/dmz006/imap-mcp/internal/service"
)

// GET /api/threads/{thread_id}?account=&live=&folders=&limit=
func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	live, _ := strconv.ParseBool(q.Get("live"))
	var folders []string
	for _, f := range strings.Split(q.Get("folders"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			folders = append(folders, f)
		}
	}
	respond(w)(s.svc.GetThread(r.Context(), service.ThreadParams{
		Account: q.Get("account"), ThreadID: pathParam(r, "thread_id"),
		Live: live, Folders: folders, Limit: queryInt(r, "limit", 100),
	}))
}

// GET /api/accounts/{account}/folders/{folder}/messages/{uid}/attachments
func (s *Server) handleListAttachments(w http.ResponseWriter, r *http.Request) {
	uid, ok := uidParam(w, r)
	if !ok {
		return
	}
	list, err := s.svc.ListAttachments(r.Context(), pathParam(r, "account"), pathParam(r, "folder"), uid)
	respond(w)(map[string]any{"uid": uid, "count": len(list), "attachments": list}, err)
}

// GET /api/accounts/{account}/folders/{folder}/messages/{uid}/attachments/{part}
func (s *Server) handleDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	uid, ok := uidParam(w, r)
	if !ok {
		return
	}
	att, err := s.svc.FetchAttachment(r.Context(), pathParam(r, "account"), pathParam(r, "folder"), uid,
		pathParam(r, "part"), int64(s.cfg.Tools.AttachmentMaxMB)<<20)
	if err != nil {
		writeError(w, err)
		return
	}
	download(w, service.SafeFilename(att.Filename, fmt.Sprintf("%d-%s.bin", uid, att.Part)), att.Data)
}

// GET /api/accounts/{account}/folders/{folder}/messages/{uid}/export.eml
func (s *Server) handleExportEML(w http.ResponseWriter, r *http.Request) {
	uid, ok := uidParam(w, r)
	if !ok {
		return
	}
	res, err := s.svc.Export(r.Context(), service.ExportParams{
		Account: pathParam(r, "account"), Folder: pathParam(r, "folder"), UID: uid,
		MaxBytes: int64(s.cfg.Tools.ExportMaxMB) << 20,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	download(w, fmt.Sprintf("%d.eml", uid), res.Data)
}

// exportBody selects a batch export; exactly one of uids, thread_id, from.
type exportBody struct {
	Account  string   `json:"account"`
	Folder   string   `json:"folder"`
	UIDs     []uint32 `json:"uids"`
	ThreadID string   `json:"thread_id"`
	From     string   `json:"from"`
}

// POST /api/export → one .mbox
func (s *Server) handleExportMbox(w http.ResponseWriter, r *http.Request) {
	var b exportBody
	if !decode(w, r, &b) {
		return
	}
	if len(b.UIDs) == 0 && b.ThreadID == "" && b.From == "" {
		http.Error(w, "give exactly one of uids, thread_id or from", http.StatusBadRequest)
		return
	}
	res, err := s.svc.Export(r.Context(), service.ExportParams{
		Account: b.Account, Folder: b.Folder, UIDs: b.UIDs, ThreadID: b.ThreadID, From: b.From,
		MaxMessages: s.cfg.Tools.ExportMaxMessages, MaxBytes: int64(s.cfg.Tools.ExportMaxMB) << 20,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("X-Export-Count", strconv.Itoa(res.Count))
	if res.Missing > 0 {
		w.Header().Set("X-Export-Missing", strconv.Itoa(res.Missing)) // thread messages moved or deleted since lookup
	}
	download(w, "export.mbox", res.Data)
}

// GET /api/search/cross?from=&subject=&text=&since=&before=&limit=&live=&folder=
func (s *Server) handleCrossSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	live, _ := strconv.ParseBool(q.Get("live"))
	respond(w)(s.svc.CrossAccountSearch(r.Context(), service.CrossSearchParams{
		From: q.Get("from"), Subject: q.Get("subject"), Text: q.Get("text"),
		Since: q.Get("since"), Before: q.Get("before"), Limit: queryInt(r, "limit", 20),
		Live: live, Folder: q.Get("folder"),
	}))
}

// download sends bytes as an opaque file. The declared type of mail content is
// untrusted, so it is always application/octet-stream and never sniffed.
func download(w http.ResponseWriter, filename string, data []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data) //nolint:errcheck
}
