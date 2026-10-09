package tools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/service"
)

func (h *Handlers) ListMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.ListMessages(ctx, service.ListMessagesParams{
		Account:   req.GetString("account", ""),
		Folder:    req.GetString("folder", "INBOX"),
		Limit:     int(req.GetFloat("limit", 50)),
		Offset:    int(req.GetFloat("offset", 0)),
		Ascending: req.GetString("order", "desc") == "asc",
	}))
}

func (h *Handlers) GetMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.GetMessage(ctx, req.GetString("account", ""), req.GetString("folder", ""), uint32(req.GetFloat("uid", 0))))
}

func (h *Handlers) GetHeaders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.GetHeaders(ctx, req.GetString("account", ""), req.GetString("folder", ""), uint32(req.GetFloat("uid", 0))))
}

func (h *Handlers) SearchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.Search(ctx, service.SearchParams{
		Account: req.GetString("account", ""),
		Folder:  req.GetString("folder", "INBOX"),
		From:    req.GetString("from", ""),
		Subject: req.GetString("subject", ""),
		Text:    req.GetString("text", ""),
		Since:   req.GetString("since", ""),
		Before:  req.GetString("before", ""),
		Flags:   req.GetString("flags", ""),
		Limit:   int(req.GetFloat("limit", 50)),
	}))
}
