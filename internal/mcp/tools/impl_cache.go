package tools

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/db"
)

// SweepFilterFromArgs builds a cache sweep filter from tool/REST arguments.
// dry_run defaults to true (D7).
func SweepFilterFromArgs(args map[string]any) (db.SweepFilter, bool) {
	str := func(k string) string { v, _ := args[k].(string); return v }
	boolean := func(k string, def bool) bool {
		if v, ok := args[k].(bool); ok {
			return v
		}
		return def
	}
	f := db.SweepFilter{
		Account:    str("account"),
		Folder:     str("folder"),
		ErrorsOnly: boolean("errors_only", false),
		All:        boolean("all", false),
	}
	if n, ok := args["older_than_days"].(float64); ok && n > 0 {
		f.OlderThanDays = int(n)
	}
	return f, boolean("dry_run", true)
}

// CacheSweep implements cache_sweep.
func (h *Handlers) CacheSweep(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.syncer == nil {
		return mcp.NewToolResultError("cache sync is not running in this mode"), nil
	}
	f, dryRun := SweepFilterFromArgs(req.GetArguments())
	if f.Empty() && !f.All {
		return mcp.NewToolResultError("refusing to sweep without a filter: set account, folder, older_than_days, errors_only, or all=true"), nil
	}
	res, err := h.syncer.Sweep(ctx, f, dryRun)
	if err != nil {
		return mcp.NewToolResultError("cache sweep failed: " + err.Error()), nil
	}
	data, _ := json.Marshal(res)
	return mcp.NewToolResultText(string(data)), nil
}
