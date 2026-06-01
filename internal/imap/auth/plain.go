package auth

import (
	"context"
	"fmt"

	imaplib "github.com/emersion/go-imap/v2/imapclient"
)

type Plain struct {
	username string
	password string
}

func NewPlain(username, password string) *Plain {
	return &Plain{username: username, password: password}
}

func (p *Plain) Type() string { return "plain" }

func (p *Plain) Authenticate(_ context.Context, c *imaplib.Client) error {
	if err := c.Login(p.username, p.password).Wait(); err != nil {
		return fmt.Errorf("plain login %s: %w", p.username, err)
	}
	return nil
}
