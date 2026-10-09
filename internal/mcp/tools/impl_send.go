package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/service"
)

// SendMessage sends outbound mail through the account's own SMTP server.
func (h *Handlers) SendMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	subject := req.GetString("subject", "")
	res, err := h.svc.Send(ctx, service.SendParams{
		Account: req.GetString("account", ""),
		To:      req.GetString("to", ""),
		Cc:      req.GetString("cc", ""),
		Subject: subject,
		Body:    req.GetString("body", ""),
	})
	if err != nil {
		return text("", err)
	}
	return text(fmt.Sprintf("sent from %q to %s (subject: %q)", res.Account, strings.Join(res.To, ", "), subject), nil)
}
