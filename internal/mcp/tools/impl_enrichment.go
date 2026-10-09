package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// EnrichmentStatus reports queue depth per lane (new mail / backfill),
// throughput, lag, backoff and whether backfill is paused (and why).
func (h *Handlers) EnrichmentStatus(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.enrich == nil {
		return mcp.NewToolResultError("enrichment pipeline is not running in this mode"), nil
	}
	st, err := h.enrich.Stats(ctx)
	if err != nil {
		return mcp.NewToolResultError("enrichment status: " + err.Error()), nil
	}
	data, _ := json.Marshal(st)
	return mcp.NewToolResultText(string(data)), nil
}

// TriggerEnrichment asks the pipeline to process up to limit messages now,
// bypassing the backfill window, yield and rate limit (not backoff or caps).
func (h *Handlers) TriggerEnrichment(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.enrich == nil {
		return mcp.NewToolResultError("enrichment pipeline is not running in this mode"), nil
	}
	if !h.cfg.Enrichment.Enabled {
		return mcp.NewToolResultError("enrichment is disabled (enrichment.enabled: false)"), nil
	}
	limit := req.GetInt("limit", 50)
	if !h.enrich.Trigger(limit) {
		return mcp.NewToolResultText("an enrichment run is already queued"), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("enrichment of up to %d queued messages triggered; check enrichment_status", limit)), nil
}
