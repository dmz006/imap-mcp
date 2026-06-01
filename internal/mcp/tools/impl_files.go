package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (h *Handlers) WriteFile(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.out == nil {
		return mcp.NewToolResultError("working_dir is not configured — set working_dir in config.yaml"), nil
	}
	filename := req.GetString("filename", "")
	content := req.GetString("content", "")
	if filename == "" {
		return mcp.NewToolResultError("filename is required"), nil
	}

	abs, err := h.out.Write(filename, content)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write_file: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("written: %s (%d bytes)", abs, len(content))), nil
}

func (h *Handlers) ReadFile(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.out == nil {
		return mcp.NewToolResultError("working_dir is not configured"), nil
	}
	filename := req.GetString("filename", "")
	if filename == "" {
		return mcp.NewToolResultError("filename is required"), nil
	}

	content, err := h.out.Read(filename)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read_file: %v", err)), nil
	}
	return mcp.NewToolResultText(content), nil
}

func (h *Handlers) DeleteFile(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.out == nil {
		return mcp.NewToolResultError("working_dir is not configured"), nil
	}
	filename := req.GetString("filename", "")
	if filename == "" {
		return mcp.NewToolResultError("filename is required"), nil
	}
	if err := h.out.Delete(filename); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("delete_file: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("deleted: %s", filename)), nil
}

func (h *Handlers) ListFiles(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.out == nil {
		return mcp.NewToolResultError("working_dir is not configured"), nil
	}
	subdir := req.GetString("subdir", "")

	files, err := h.out.List(subdir)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list_files: %v", err)), nil
	}

	result, err := mcp.NewToolResultJSON(map[string]any{
		"working_dir": h.out.Root(),
		"subdir":      subdir,
		"files":       files,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return result, nil
}
