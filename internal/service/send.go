package service

import (
	"context"
	"strings"

	"github.com/dmz006/imap-mcp/internal/smtp"
)

// SendParams is an outbound message.
type SendParams struct {
	Account       string // "" or "_default" = default account
	To, Cc        string // comma-separated
	Subject, Body string
}

// SendResult reports what was sent.
type SendResult struct {
	Status  string   `json:"status"`
	From    string   `json:"from"`
	Account string   `json:"account"`
	To      []string `json:"to"`
}

// Send delivers mail through the account's own SMTP server.
func (s *Service) Send(ctx context.Context, p SendParams) (SendResult, error) {
	if p.To == "" || p.Subject == "" || p.Body == "" {
		return SendResult{}, invalid("to, subject, and body are required")
	}
	acct := s.cfg.DefaultAccount()
	if p.Account != "" && p.Account != "_default" {
		a, err := s.cfg.Account(p.Account)
		if err != nil {
			return SendResult{}, notFound("account %q not found", p.Account)
		}
		acct = a
	}
	smtpCfg := acct.ResolvedSMTP()
	if smtpCfg == nil {
		return SendResult{}, &Error{Kind: KindUnprocessable, Msg: "account " + quote(acct.Name) + " has no smtp config (receive-only)"}
	}
	sender, err := smtp.NewSender(smtpCfg)
	if err != nil {
		return SendResult{}, &Error{Kind: KindInternal, Msg: "smtp setup", Err: err}
	}
	msg := smtp.Message{To: splitAddrs(p.To), Cc: splitAddrs(p.Cc), Subject: p.Subject, Body: p.Body}
	if err := sender.Send(msg); err != nil {
		return SendResult{}, upstream("send failed", err)
	}
	return SendResult{Status: "sent", From: smtpCfg.From, Account: acct.Name, To: msg.To}, nil
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

func quote(s string) string { return `"` + s + `"` }
