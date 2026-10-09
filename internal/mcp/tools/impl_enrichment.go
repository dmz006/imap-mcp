package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// EnrichmentStatus reports queue depth per lane (new mail / backfill),
// throughput, lag, backoff and whether backfill is paused (and why).
func (h *Handlers) EnrichmentStatus(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return result(h.svc.EnrichmentStats(ctx))
}

// TriggerEnrichment asks the pipeline to process up to limit messages now,
// bypassing the backfill window, yield and rate limit (not backoff or caps).
func (h *Handlers) TriggerEnrichment(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 50)
	queued, err := h.svc.TriggerEnrichment(limit)
	if err == nil && !queued {
		return text("an enrichment run is already queued", nil)
	}
	return text(fmt.Sprintf("enrichment of up to %d queued messages triggered; check enrichment_status", limit), err)
}
