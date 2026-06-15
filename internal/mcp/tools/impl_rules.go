package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/dmz006/imap-mcp/internal/db"
	imaplib "github.com/emersion/go-imap/v2"
	"github.com/mark3labs/mcp-go/mcp"
)

// CreateRule persists an automation rule (match criteria + action).
func (h *Handlers) CreateRule(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("name", "")
	action := req.GetString("action", "")
	if name == "" || action == "" {
		return mcp.NewToolResultError("name and action (trash|move|flag|seen) are required"), nil
	}
	if action == "move" && req.GetString("dest", "") == "" {
		return mcp.NewToolResultError("action=move requires dest"), nil
	}
	rule := &db.Rule{
		Name:        name,
		Description: req.GetString("description", ""),
		Active:      req.GetBool("active", true),
		Conditions: db.RuleConditions{
			Account:       req.GetString("account", ""),
			Folder:        req.GetString("folder", ""),
			From:          req.GetString("from", ""),
			Subject:       req.GetString("subject", ""),
			Text:          req.GetString("text", ""),
			OlderThanDays: int(req.GetFloat("older_than_days", 0)),
		},
		Actions: []db.RuleAction{{
			Type:  action,
			Dest:  req.GetString("dest", ""),
			Flags: req.GetString("flags", ""),
		}},
	}
	if rule.Conditions.From == "" && rule.Conditions.Subject == "" && rule.Conditions.Text == "" && rule.Conditions.OlderThanDays == 0 {
		return mcp.NewToolResultError("a rule needs at least one condition (from, subject, text, or older_than_days)"), nil
	}
	id, err := h.db.Rules.Create(rule)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create rule: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("rule %q created (id=%d)", name, id)), nil
}

// ListRules returns all persisted rules.
func (h *Handlers) ListRules(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rules, err := h.db.Rules.List()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list rules: %v", err)), nil
	}
	result, err := mcp.NewToolResultJSON(map[string]any{"count": len(rules), "rules": rules})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

// DeleteRule removes a rule by id.
func (h *Handlers) DeleteRule(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := int64(req.GetFloat("id", 0))
	if id == 0 {
		return mcp.NewToolResultError("id is required"), nil
	}
	if err := h.db.Rules.Delete(id); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete rule: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("rule %d deleted", id)), nil
}

// RunRules applies all active rules (or one rule by id) to their target folders
// and reports how many messages each matched/acted on.
func (h *Handlers) RunRules(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	onlyID := int64(req.GetFloat("id", 0))
	dryRun := req.GetBool("dry_run", false)

	rules, err := h.db.Rules.List()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list rules: %v", err)), nil
	}

	type ruleResult struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Matched int    `json:"matched"`
		Action  string `json:"action"`
		Error   string `json:"error,omitempty"`
	}
	var results []ruleResult

	for _, rule := range rules {
		if !rule.Active && onlyID == 0 {
			continue
		}
		if onlyID != 0 && rule.ID != onlyID {
			continue
		}
		res := ruleResult{ID: rule.ID, Name: rule.Name}
		if len(rule.Actions) > 0 {
			res.Action = rule.Actions[0].Type
		}
		n, err := h.applyRule(rule, dryRun)
		res.Matched = n
		if err != nil {
			res.Error = err.Error()
		} else if !dryRun && n > 0 {
			_ = h.db.Rules.IncrementRun(rule.ID, n)
		}
		results = append(results, res)
	}

	result, err := mcp.NewToolResultJSON(map[string]any{"dry_run": dryRun, "results": results})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}

// applyRule searches a rule's folder by its conditions and applies its actions.
// Returns the number of matched messages. With dryRun it only counts.
func (h *Handlers) applyRule(rule db.Rule, dryRun bool) (int, error) {
	folder := rule.Conditions.Folder
	if folder == "" {
		folder = "INBOX"
	}
	conn, err := h.pool.Resolve(rule.Conditions.Account)
	if err != nil {
		return 0, err
	}
	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return 0, fmt.Errorf("select %s: %w", folder, err)
	}

	criteria := &imaplib.SearchCriteria{}
	c := rule.Conditions
	if c.From != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "From", Value: c.From})
	}
	if c.Subject != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "Subject", Value: c.Subject})
	}
	if c.Text != "" {
		criteria.Body = append(criteria.Body, c.Text)
	}
	if c.OlderThanDays > 0 {
		criteria.Before = time.Now().AddDate(0, 0, -c.OlderThanDays)
	}

	sd, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return 0, fmt.Errorf("search: %w", err)
	}
	uids := sd.AllUIDs()
	if len(uids) == 0 || dryRun {
		return len(uids), nil
	}

	set := imaplib.UIDSetNum(uids...)
	for _, act := range rule.Actions {
		switch act.Type {
		case "trash":
			trash, err := resolveTrash(client)
			if err != nil {
				return len(uids), fmt.Errorf("resolve trash: %w", err)
			}
			if _, err := client.Move(set, trash).Wait(); err != nil {
				return len(uids), fmt.Errorf("move to trash: %w", err)
			}
		case "move":
			if _, err := client.Move(set, act.Dest).Wait(); err != nil {
				return len(uids), fmt.Errorf("move to %s: %w", act.Dest, err)
			}
		case "flag":
			if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: parseFlags(act.Flags)}, nil).Close(); err != nil {
				return len(uids), fmt.Errorf("flag: %w", err)
			}
		case "seen":
			if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagSeen}}, nil).Close(); err != nil {
				return len(uids), fmt.Errorf("mark seen: %w", err)
			}
		default:
			return len(uids), fmt.Errorf("unknown action %q", act.Type)
		}
	}
	return len(uids), nil
}
