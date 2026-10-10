package tools

// impl_content.go — threads, attachments, export and cross-account search
// (AGENT.md D23–D26). Content is written into the working-dir sandbox, never
// returned raw, except small text attachments (D24).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/service"
)

// GetThread returns a conversation, cache first with a live fallback.
func (h *Handlers) GetThread(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.GetThread(ctx, service.ThreadParams{
		Account:  req.GetString("account", ""),
		ThreadID: req.GetString("thread_id", ""),
		Live:     req.GetBool("live", false),
		Folders:  splitList(req.GetString("folders", "")),
		Limit:    int(req.GetFloat("limit", 100)),
	}))
}

// GetAttachments lists a message's attachments, or saves one into the
// working directory. The scope middleware requires write for a download.
func (h *Handlers) GetAttachments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account, folder := req.GetString("account", ""), req.GetString("folder", "")
	uid := uint32(req.GetFloat("uid", 0))
	part := req.GetString("part", "")
	if part == "" {
		list, err := h.svc.ListAttachments(ctx, account, folder, uid)
		return result(map[string]any{"folder": folder, "uid": uid, "count": len(list), "attachments": list}, err)
	}
	if h.out == nil {
		return mcp.NewToolResultError("working_dir is not configured — set working_dir in config.yaml"), nil
	}
	att, err := h.svc.FetchAttachment(ctx, account, folder, uid, part, int64(h.cfg.Tools.AttachmentMaxMB)<<20)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	name := req.GetString("filename", "")
	if name == "" {
		acct := account
		if acct == "" {
			acct = h.cfg.DefaultAccount().Name
		}
		name = fmt.Sprintf("attachments/%s/%d-%s-%s", service.SafeFilename(acct, "account"), uid,
			strings.ReplaceAll(part, ".", "_"), service.SafeFilename(att.Filename, "part.bin"))
	}
	if _, err := h.out.Write(name, string(att.Data)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get_attachments: %v", err)), nil
	}
	out := map[string]any{
		"saved_to": name, "bytes": len(att.Data), "part": att.Part,
		"filename": att.Filename, "mime": att.MIME,
	}
	if limit := h.cfg.Tools.AttachmentInlineKB << 10; att.IsText() && limit > 0 && len(att.Data) <= limit {
		out["text"] = string(att.Data)
	}
	return result(out, nil)
}

// ExportMessage writes one .eml or one .mbox into the working directory.
func (h *Handlers) ExportMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.out == nil {
		return mcp.NewToolResultError("working_dir is not configured — set working_dir in config.yaml"), nil
	}
	uids, err := parseUIDs(req.GetString("uids", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	p := service.ExportParams{
		Account:     req.GetString("account", ""),
		Folder:      req.GetString("folder", ""),
		UID:         uint32(req.GetFloat("uid", 0)),
		UIDs:        uids,
		ThreadID:    req.GetString("thread_id", ""),
		From:        req.GetString("from", ""),
		MaxMessages: h.cfg.Tools.ExportMaxMessages,
		MaxBytes:    int64(h.cfg.Tools.ExportMaxMB) << 20,
	}
	res, err := h.svc.Export(ctx, p)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	name := req.GetString("filename", "")
	if name == "" {
		name = h.exportName(p, res.Format)
	}
	if _, err := h.out.Write(name, string(res.Data)); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("export_message: %v", err)), nil
	}
	out := map[string]any{"saved_to": name, "format": res.Format, "count": res.Count, "bytes": res.Bytes}
	if res.Missing > 0 {
		out["missing"] = res.Missing // thread messages moved or deleted since they were found
	}
	return result(out, nil)
}

// exportName is the default export file name, built only from safe parts.
func (h *Handlers) exportName(p service.ExportParams, format string) string {
	acct := p.Account
	if acct == "" {
		acct = h.cfg.DefaultAccount().Name
	}
	acct = service.SafeFilename(acct, "account")
	folder := service.SafeFilename(p.Folder, "INBOX")
	stamp := time.Now().UTC().Format("20060102-150405")
	switch {
	case p.UID != 0:
		return fmt.Sprintf("exports/%s-%s-%d.eml", acct, folder, p.UID)
	case p.ThreadID != "":
		id := service.SafeFilename(p.ThreadID, "thread")
		if len(id) > 40 {
			id = strings.ToValidUTF8(id[:40], "")
		}
		return fmt.Sprintf("exports/thread-%s-%s.%s", id, stamp, format)
	case p.From != "":
		return fmt.Sprintf("exports/%s-from-%s-%s.%s", acct, service.SafeFilename(p.From, "sender"), stamp, format)
	default:
		return fmt.Sprintf("exports/%s-%s-%s.%s", acct, folder, stamp, format)
	}
}

// CrossAccountSearch searches every account at once.
func (h *Handlers) CrossAccountSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.CrossAccountSearch(ctx, service.CrossSearchParams{
		From:    req.GetString("from", ""),
		Subject: req.GetString("subject", ""),
		Text:    req.GetString("text", ""),
		Since:   req.GetString("since", ""),
		Before:  req.GetString("before", ""),
		Limit:   int(req.GetFloat("limit", 20)),
		Live:    req.GetBool("live", false),
		Folder:  req.GetString("folder", ""),
	}))
}

// splitList splits a comma-separated list, dropping empty entries.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// parseUIDs parses a comma-separated UID list.
func parseUIDs(s string) ([]uint32, error) {
	var out []uint32
	for _, f := range splitList(s) {
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("uids: %q is not a UID", f)
		}
		out = append(out, uint32(n))
	}
	return out, nil
}
