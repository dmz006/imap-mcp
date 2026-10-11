package tools

import (
	"context"
	"encoding/json"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/service"
)

// SemanticSearch ranks cached, enriched messages by similarity to a query or
// a reference message.
func (h *Handlers) SemanticSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.SemanticSearch(ctx, service.SemanticParams{
		Account:      req.GetString("account", ""),
		Folder:       req.GetString("folder", ""),
		Query:        req.GetString("query", ""),
		ReferenceUID: uint32(req.GetFloat("reference_uid", 0)),
		Limit:        int(req.GetFloat("limit", 10)),
		Threshold:    req.GetFloat("threshold", 0.7),
	}))
}

// GetSenderHistory lists cached messages from an address (sync window only).
func (h *Handlers) GetSenderHistory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	msgs, err := h.svc.SenderHistory(ctx, req.GetString("account", ""), req.GetString("address", ""), int(req.GetFloat("limit", 100)))
	return result(map[string]any{"address": req.GetString("address", ""), "count": len(msgs), "messages": msgs}, err)
}

// GetSenderProfile returns a sender profile with relationships and anomalies.
func (h *Handlers) GetSenderProfile(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.GetSenderProfile(ctx, req.GetString("address", "")))
}

// KGQuery returns knowledge-graph relationships.
func (h *Handlers) KGQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	edges, err := h.svc.KGQuery(ctx, service.KGParams{
		Entity: req.GetString("entity", ""), Predicate: req.GetString("predicate", ""),
		EntityType: req.GetString("entity_type", ""), Limit: int(req.GetFloat("limit", 50)),
	})
	return result(map[string]any{"count": len(edges), "relationships": edges}, err)
}

// GetAnomalies lists detected anomalies.
func (h *Handlers) GetAnomalies(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	list, err := h.svc.Anomalies(ctx, service.AnomalyParams{
		Account: req.GetString("account", ""), Severity: req.GetString("severity", ""),
		Type: req.GetString("type", ""), Sender: req.GetString("sender", ""),
		IncludeResolved: !req.GetBool("unresolved_only", true), Limit: int(req.GetFloat("limit", 20)),
	})
	return result(map[string]any{"count": len(list), "anomalies": list}, err)
}

// ResolveAnomaly marks an anomaly reviewed.
func (h *Handlers) ResolveAnomaly(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.ResolveAnomaly(ctx, int64(req.GetFloat("id", 0))))
}

// replyParams reads the needs_reply / awaiting_reply parameters.
func replyParams(req mcp.CallToolRequest) service.ReplyParams {
	return service.ReplyParams{
		Account:       req.GetString("account", ""),
		OlderThanDays: int(req.GetFloat("older_than_days", 2)),
		WithinDays:    int(req.GetFloat("within_days", 90)),
		Limit:         int(req.GetFloat("limit", 20)),
	}
}

// NeedsReply lists conversations waiting on the owner.
func (h *Handlers) NeedsReply(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.NeedsReply(ctx, replyParams(req)))
}

// AwaitingReply lists conversations where the owner wrote last.
func (h *Handlers) AwaitingReply(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.AwaitingReply(ctx, replyParams(req)))
}

// DismissReply removes a conversation from the reply lists.
func (h *Handlers) DismissReply(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.DismissReply(ctx, req.GetString("account", ""), req.GetString("thread_id", "")))
}

// SuggestRules lists rules learned from the owner's moves.
func (h *Handlers) SuggestRules(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.SuggestRules(ctx, service.SuggestParams{
		Account: req.GetString("account", ""), Limit: int(req.GetFloat("limit", 50)),
	}))
}

// DismissSuggestion marks a sender or domain never-suggest.
func (h *Handlers) DismissSuggestion(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.DismissSuggestion(ctx, req.GetString("account", ""), req.GetString("target", "")))
}

// SuggestIdentities lists addresses that look like the owner's.
func (h *Handlers) SuggestIdentities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.SuggestIdentities(ctx))
}

// ConfirmIdentity records an address as the owner's.
func (h *Handlers) ConfirmIdentity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.ConfirmIdentity(ctx, req.GetString("address", "")))
}

// RejectIdentity records that an address is not the owner's.
func (h *Handlers) RejectIdentity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.RejectIdentity(ctx, req.GetString("address", "")))
}

// SetupCheck reports setup gaps.
func (h *Handlers) SetupCheck(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.SetupCheck(ctx))
}

// ListRulePacks lists the built-in rule packs.
func (h *Handlers) ListRulePacks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	packs, err := h.svc.ListRulePacks()
	return result(map[string]any{"count": len(packs), "packs": packs}, err)
}

// ImportRulePack creates a pack's rules, inactive.
func (h *Handlers) ImportRulePack(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.ImportRulePack(ctx, req.GetString("account", ""), req.GetString("name", ""), req.GetString("dest", "")))
}

// VerdictEval starts a verdict model evaluation.
func (h *Handlers) VerdictEval(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p := service.EvalParams{URL: req.GetString("url", ""), PerClass: int(req.GetFloat("per_class", 50))}
	for _, m := range strings.Split(req.GetString("models", ""), ",") {
		if m = strings.TrimSpace(m); m != "" {
			p.Models = append(p.Models, m)
		}
	}
	return result(h.svc.StartVerdictEval(ctx, p))
}

// VerdictEvalResults returns an evaluation run.
func (h *Handlers) VerdictEvalResults(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.VerdictEval(ctx, int64(req.GetFloat("id", 0))))
}

// Unsubscribe unsubscribes from a sender (Q4). A mailto unsubscribe sends
// mail, so it needs the send scope as well (D40); with auth off (stdio or no
// tokens) there is no principal and the operator is the caller.
func (h *Handlers) Unsubscribe(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p := httpauth.FromContext(ctx)
	return result(h.svc.Unsubscribe(ctx, service.UnsubscribeParams{
		Account: req.GetString("account", ""), Sender: req.GetString("sender", ""), UID: uint32(req.GetFloat("uid", 0)),
		Folder: req.GetString("folder", ""), Confirm: req.GetBool("confirm", false), ArchiveTo: req.GetString("archive_to", ""),
		CanSend: p == nil || p.Has(httpauth.ScopeSend),
	}))
}

// ListUnsubscribes lists the unsubscribe log and senders still mailing.
func (h *Handlers) ListUnsubscribes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.ListUnsubscribes(ctx, req.GetString("account", ""), req.GetBool("dry_runs", false), int(req.GetFloat("limit", 100))))
}

// HeldSenders lists undecided held senders, numbered.
func (h *Handlers) HeldSenders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.HeldSenders(ctx, req.GetString("account", "")))
}

// ScreenSenders decides held senders (Q5); a preview unless confirm (D62).
func (h *Handlers) ScreenSenders(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var decisions []service.ScreenDecision
	if raw, ok := req.GetArguments()["decisions"]; ok {
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &decisions); err != nil {
			return mcp.NewToolResultError("decisions must be [{sender, action, dest}]"), nil
		}
	}
	return result(h.svc.Screen(ctx, service.ScreenParams{
		Account: req.GetString("account", ""), Decisions: decisions, Confirm: req.GetBool("confirm", false),
	}))
}
