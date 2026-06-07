package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/dmz006/imap-mcp/internal/smtp"
	"github.com/mark3labs/mcp-go/mcp"
)

// SendMessage sends outbound mail through the account's own SMTP server.
func (h *Handlers) SendMessage(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	account := req.GetString("account", "")
	to := req.GetString("to", "")
	subject := req.GetString("subject", "")
	body := req.GetString("body", "")
	if to == "" || subject == "" || body == "" {
		return mcp.NewToolResultError("to, subject, and body are required"), nil
	}

	acct := h.cfg.DefaultAccount()
	if account != "" {
		a, err := h.cfg.Account(account)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("account error: %v", err)), nil
		}
		acct = a
	}

	smtpCfg := acct.ResolvedSMTP()
	if smtpCfg == nil {
		return mcp.NewToolResultError(fmt.Sprintf("account %q has no smtp: config — it is receive-only", acct.Name)), nil
	}
	sender, err := smtp.NewSender(smtpCfg)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("smtp setup: %v", err)), nil
	}

	msg := smtp.Message{
		To:      splitAddrs(to),
		Cc:      splitAddrs(req.GetString("cc", "")),
		Subject: subject,
		Body:    body,
	}
	if err := sender.Send(msg); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("send failed: %v", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("sent from %q to %s (subject: %q)", acct.Name, strings.Join(msg.To, ", "), subject)), nil
}

func splitAddrs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
