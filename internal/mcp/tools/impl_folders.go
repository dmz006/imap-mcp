package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (h *Handlers) ListFolders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.ListFolders(ctx, req.GetString("account", "")))
}

// folderPath accepts either `path` or `folder` so the folder tools are
// consistent with the rest of the API (which uses `folder`).
func folderPath(req mcp.CallToolRequest) string {
	if p := req.GetString("path", ""); p != "" {
		return p
	}
	return req.GetString("folder", "")
}

func (h *Handlers) CreateFolder(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := folderPath(req)
	return text(fmt.Sprintf("folder %q created", path), h.svc.CreateFolder(ctx, req.GetString("account", ""), path))
}

func (h *Handlers) DeleteFolder(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path := folderPath(req)
	return text(fmt.Sprintf("folder %q deleted", path), h.svc.DeleteFolder(ctx, req.GetString("account", ""), path))
}
