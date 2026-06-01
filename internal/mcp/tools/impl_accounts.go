package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (h *Handlers) ListAccounts(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	names := h.pool.AccountNames()
	type accountInfo struct {
		Name      string `json:"name"`
		Default   bool   `json:"default"`
		Connected bool   `json:"connected"`
	}
	infos := make([]accountInfo, 0, len(h.cfg.Accounts))
	for _, a := range h.cfg.Accounts {
		connected := false
		for _, n := range names {
			if n == a.Name {
				connected = true
				break
			}
		}
		infos = append(infos, accountInfo{
			Name:      a.Name,
			Default:   a.Default,
			Connected: connected,
		})
	}
	data, _ := json.Marshal(infos)
	return mcp.NewToolResultText(string(data)), nil
}

func (h *Handlers) SyncAccount(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	if account == "" {
		account = h.cfg.DefaultAccount().Name
	}
	if err := h.syncer.SyncAccount(ctx, account); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("sync failed: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("sync triggered for account %q", account)), nil
}
