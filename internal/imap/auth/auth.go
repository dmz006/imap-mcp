// Package auth defines the authentication interface and implementations
// for IMAP connections. New auth mechanisms implement Authenticator.
package auth

import (
	"context"
	"fmt"

	imaplib "github.com/emersion/go-imap/v2/imapclient"
)

// Authenticator is the pluggable auth interface. Each auth type implements
// Authenticate against a live IMAP client connection.
type Authenticator interface {
	// Type returns the auth mechanism name for logging and config.
	Type() string
	// Authenticate performs the login sequence on an already-connected client.
	Authenticate(ctx context.Context, c *imaplib.Client) error
}

// New constructs an Authenticator from config values.
func New(authType, username, password, clientID, clientSecret, tokenFile string) (Authenticator, error) {
	switch authType {
	case "plain", "":
		return NewPlain(username, password), nil
	case "xoauth2":
		return NewXOAuth2(username, clientID, clientSecret, tokenFile), nil
	default:
		return nil, fmt.Errorf("unknown auth type %q", authType)
	}
}
