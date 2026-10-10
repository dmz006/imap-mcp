package mcp

import (
	"context"
	"fmt"

	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// toolScopes maps every MCP tool to the scope a token needs to call it
// (AGENT.md D13a). A tool missing from this table is denied when auth is
// enforced, and TestEveryToolHasScope fails, so new tools must be added here.
var toolScopes = map[string]httpauth.Scope{
	// read
	"list_accounts":        httpauth.ScopeRead,
	"list_folders":         httpauth.ScopeRead,
	"list_messages":        httpauth.ScopeRead,
	"get_message":          httpauth.ScopeRead,
	"get_thread":           httpauth.ScopeRead,
	"get_headers":          httpauth.ScopeRead,
	"get_attachments":      httpauth.ScopeRead,
	"search_messages":      httpauth.ScopeRead,
	"cross_account_search": httpauth.ScopeRead,
	"semantic_search":      httpauth.ScopeRead,
	"summarize_folder":     httpauth.ScopeRead,
	"detect_subscriptions": httpauth.ScopeRead,
	"get_sender_history":   httpauth.ScopeRead,
	"get_sender_profile":   httpauth.ScopeRead,
	"kg_query":             httpauth.ScopeRead,
	"get_anomalies":        httpauth.ScopeRead,
	"needs_reply":          httpauth.ScopeRead,
	"awaiting_reply":       httpauth.ScopeRead,
	"enrichment_status":    httpauth.ScopeRead,
	"top_senders":          httpauth.ScopeRead,
	"list_rules":           httpauth.ScopeRead,
	"suggest_rules":        httpauth.ScopeRead,
	"suggest_identities":   httpauth.ScopeRead,
	"read_file":            httpauth.ScopeRead,
	"list_files":           httpauth.ScopeRead,

	// write — mailbox changes, rules changes, sandbox writes
	"create_folder":      httpauth.ScopeWrite,
	"delete_folder":      httpauth.ScopeWrite,
	"label_message":      httpauth.ScopeWrite,
	"label_bulk":         httpauth.ScopeWrite,
	"empty_trash":        httpauth.ScopeWrite,
	"move_message":       httpauth.ScopeWrite,
	"copy_message":       httpauth.ScopeWrite,
	"delete_message":     httpauth.ScopeWrite,
	"set_flags":          httpauth.ScopeWrite,
	"append_message":     httpauth.ScopeWrite,
	"move_bulk":          httpauth.ScopeWrite,
	"flag_bulk":          httpauth.ScopeWrite,
	"purge_sender":       httpauth.ScopeWrite,
	"create_rule":        httpauth.ScopeWrite,
	"delete_rule":        httpauth.ScopeWrite,
	"run_rules":          httpauth.ScopeWrite,
	"dismiss_suggestion": httpauth.ScopeWrite,
	"confirm_identity":   httpauth.ScopeWrite,
	"reject_identity":    httpauth.ScopeWrite,
	"resolve_anomaly":    httpauth.ScopeWrite,
	"dismiss_reply":      httpauth.ScopeWrite,
	"export_message":     httpauth.ScopeWrite,
	"write_file":         httpauth.ScopeWrite,
	"delete_file":        httpauth.ScopeWrite,

	// send
	"send_message": httpauth.ScopeSend,

	// admin — operational triggers
	"sync_account":       httpauth.ScopeAdmin,
	"trigger_enrichment": httpauth.ScopeAdmin,
	"cache_sweep":        httpauth.ScopeAdmin,
}

// argScopes adds a scope requirement that depends on a call's arguments: a
// tool listed in toolScopes for its read form needs more for its write form.
var argScopes = map[string]func(args map[string]any) httpauth.Scope{
	// Listing attachments is a read; downloading one writes into working_dir (D24).
	"get_attachments": func(args map[string]any) httpauth.Scope {
		if part, _ := args["part"].(string); part != "" {
			return httpauth.ScopeWrite
		}
		return ""
	},
}

// scopeMiddleware denies a tool call unless the caller's principal holds the
// tool's scope (and any argument-dependent scope from argScopes). It is installed only when HTTP auth is enforced, so a missing
// principal is itself a denial (fail closed).
func scopeMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.Params.Name
		scope, ok := toolScopes[name]
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("forbidden: tool %q has no scope mapping", name)), nil
		}
		p := httpauth.FromContext(ctx)
		if !p.Has(scope) {
			return mcp.NewToolResultError(fmt.Sprintf("forbidden: tool %q requires scope %q", name, scope)), nil
		}
		if extra, ok := argScopes[name]; ok {
			if need := extra(req.GetArguments()); need != "" && !p.Has(need) {
				return mcp.NewToolResultError(fmt.Sprintf("forbidden: tool %q with these arguments requires scope %q", name, need)), nil
			}
		}
		return next(ctx, req)
	}
}

// scopeFilter hides tools the caller cannot call from tools/list.
func scopeFilter(ctx context.Context, all []mcp.Tool) []mcp.Tool {
	p := httpauth.FromContext(ctx)
	out := make([]mcp.Tool, 0, len(all))
	for _, t := range all {
		if s, ok := toolScopes[t.Name]; ok && p.Has(s) {
			out = append(out, t)
		}
	}
	return out
}
