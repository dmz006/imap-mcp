// Package smtp implements per-account outbound mail. Each account sends through
// its own configured server (its domain), never a shared relay.
package smtp

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
)

// Message is an outbound email.
type Message struct {
	To      []string
	Cc      []string
	Subject string
	Body    string // plain text
}

// Sender sends mail for a single account's SMTP config (already credential-resolved).
type Sender struct {
	cfg *config.SMTPConfig
	now func() time.Time
}

// NewSender builds a Sender from a resolved SMTP config (see AccountConfig.ResolvedSMTP).
func NewSender(cfg *config.SMTPConfig) (*Sender, error) {
	if cfg == nil {
		return nil, fmt.Errorf("smtp not configured for this account")
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("smtp.host is required")
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("smtp.from (or auth.username) is required")
	}
	return &Sender{cfg: cfg, now: time.Now}, nil
}

// Send delivers msg. It uses implicit TLS when cfg.TLS is set (typically :465),
// otherwise STARTTLS (typically :587). Auth is PLAIN when a password is set.
func (s *Sender) Send(msg Message) error {
	if len(msg.To) == 0 {
		return fmt.Errorf("at least one recipient is required")
	}
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprint(s.cfg.Port))
	raw := s.build(msg)
	rcpts := append(append([]string{}, msg.To...), msg.Cc...)

	var auth smtp.Auth
	if s.cfg.Password != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}

	if s.cfg.TLS {
		return s.sendImplicitTLS(addr, auth, rcpts, raw)
	}
	// STARTTLS path: smtp.SendMail issues STARTTLS automatically when the
	// server advertises it.
	return smtp.SendMail(addr, auth, fromAddr(s.cfg.From), rcpts, raw)
}

func (s *Sender) sendImplicitTLS(addr string, auth smtp.Auth, rcpts []string, raw []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: s.cfg.Host})
	if err != nil {
		return fmt.Errorf("smtp tls dial %s: %w", addr, err)
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(fromAddr(s.cfg.From)); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	for _, rcpt := range rcpts {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return c.Quit()
}

// build renders an RFC 5322 message. Body is sent as UTF-8 text/plain.
func (s *Sender) build(msg Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + s.cfg.From + "\r\n")
	b.WriteString("To: " + strings.Join(msg.To, ", ") + "\r\n")
	if len(msg.Cc) > 0 {
		b.WriteString("Cc: " + strings.Join(msg.Cc, ", ") + "\r\n")
	}
	b.WriteString("Subject: " + sanitizeHeader(msg.Subject) + "\r\n")
	b.WriteString("Date: " + s.now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(msg.Body, "\n", "\r\n"))
	return []byte(b.String())
}

// fromAddr returns the bare address from a possibly "Name <addr>" From value.
func fromAddr(from string) string {
	if lt := strings.LastIndex(from, "<"); lt >= 0 {
		if gt := strings.Index(from[lt:], ">"); gt >= 0 {
			return from[lt+1 : lt+gt]
		}
	}
	return strings.TrimSpace(from)
}

// sanitizeHeader strips CR/LF to prevent header injection from a crafted subject.
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}
