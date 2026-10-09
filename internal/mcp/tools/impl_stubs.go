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
