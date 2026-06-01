package tools

// impl_stubs.go — remaining tools not yet implemented.

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func stub(name string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf("%s: not yet implemented", name)), nil
}

func (h *Handlers) GetThread(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("get_thread")
}
func (h *Handlers) GetAttachments(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("get_attachments")
}
func (h *Handlers) ExportMessage(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("export_message")
}
func (h *Handlers) CrossAccountSearch(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("cross_account_search")
}
func (h *Handlers) SemanticSearch(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("semantic_search")
}
func (h *Handlers) GetSenderHistory(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("get_sender_history")
}
func (h *Handlers) GetSenderProfile(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("get_sender_profile")
}
func (h *Handlers) KGQuery(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("kg_query")
}
func (h *Handlers) GetAnomalies(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("get_anomalies")
}
func (h *Handlers) EnrichmentStatus(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("enrichment_status")
}
func (h *Handlers) TriggerEnrichment(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return stub("trigger_enrichment")
}
