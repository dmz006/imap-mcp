package tools

import (
	"context"

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
