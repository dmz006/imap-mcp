package tools

import (
	"context"

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
	f, dryRun := SweepFilterFromArgs(req.GetArguments())
	return result(h.svc.SweepCache(ctx, f, dryRun))
}
