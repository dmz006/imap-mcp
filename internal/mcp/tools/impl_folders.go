package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (h *Handlers) ListFolders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}

	conn.Lock()
	defer conn.Unlock()

	client := conn.Client()
	mailboxes, err := client.List("", "*", nil).Collect()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list folders: %v", err)), nil
	}

	type folder struct {
		Path  string `json:"path"`
		Delim string `json:"delimiter"`
	}
	result := make([]folder, 0, len(mailboxes))
	for _, mb := range mailboxes {
		result = append(result, folder{
			Path:  mb.Mailbox,
			Delim: string(mb.Delim),
		})
	}
	return mcp.NewToolResultJSON(result)
}

func (h *Handlers) CreateFolder(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	path := req.GetString("path", "")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}

	conn.Lock()
	defer conn.Unlock()

	if err := conn.Client().Create(path, nil).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create folder: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("folder %q created", path)), nil
}

func (h *Handlers) DeleteFolder(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	path := req.GetString("path", "")
	if path == "" {
		return mcp.NewToolResultError("path is required"), nil
	}

	conn, err := h.pool.Resolve(account)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
	}

	conn.Lock()
	defer conn.Unlock()

	if err := conn.Client().Delete(path).Wait(); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete folder: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("folder %q deleted", path)), nil
}
