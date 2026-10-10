package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/service"
)

// CreateRule persists an automation rule (match criteria + action).
func (h *Handlers) CreateRule(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rule := &db.Rule{
		Name:        req.GetString("name", ""),
		Description: req.GetString("description", ""),
		Active:      req.GetBool("active", true),
		Conditions: db.RuleConditions{
			Account:       req.GetString("account", ""),
			Folder:        req.GetString("folder", ""),
			From:          req.GetString("from", ""),
			Subject:       req.GetString("subject", ""),
			Text:          req.GetString("text", ""),
			OlderThanDays: int(req.GetFloat("older_than_days", 0)),
			NewSender:     req.GetBool("new_sender", false),
			NewSenderDays: int(req.GetFloat("new_sender_days", 0)),
		},
	}
	if action := req.GetString("action", ""); action != "" {
		rule.Actions = []db.RuleAction{{Type: action, Dest: req.GetString("dest", ""), Flags: req.GetString("flags", "")}}
	}
	id, err := h.svc.CreateRule(ctx, rule)
	return text(fmt.Sprintf("rule %q created (id=%d)", rule.Name, id), err)
}

// ListRules returns all persisted rules.
func (h *Handlers) ListRules(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rules, err := h.svc.ListRules(ctx)
	return result(map[string]any{"count": len(rules), "rules": rules}, err)
}

// DeleteRule removes a rule by id.
func (h *Handlers) DeleteRule(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := int64(req.GetFloat("id", 0))
	return text(fmt.Sprintf("rule %d deleted", id), h.svc.DeleteRule(ctx, id))
}

// RunRules applies all active rules (or one rule by id) to their target folders
// and reports how many messages each matched/acted on.
func (h *Handlers) RunRules(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dryRun := req.GetBool("dry_run", false)
	results, err := h.svc.RunActiveRules(int64(req.GetFloat("id", 0)), dryRun)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("run rules: %v", err)), nil
	}
	return result(map[string]any{"dry_run": dryRun, "results": results}, nil)
}

// RuleRunResult is the outcome of applying one rule.
type RuleRunResult = service.RuleRunResult

// RunActiveRules applies all active rules (or one by id); see service.RunActiveRules.
func (h *Handlers) RunActiveRules(onlyID int64, dryRun bool) ([]RuleRunResult, error) {
	return h.svc.RunActiveRules(onlyID, dryRun)
}
