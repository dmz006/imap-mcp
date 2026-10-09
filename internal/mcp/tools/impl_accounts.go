package tools

import (
	"context"
	"encoding/json"

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
	folders, err := h.svc.SyncAccount(ctx, account)
	return result(map[string]any{"account": account, "folders": folders}, err)
}
