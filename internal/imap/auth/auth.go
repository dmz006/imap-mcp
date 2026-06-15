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

// Options carries everything an authenticator may need. Built from the
// account's AuthConfig by the caller, so the auth package stays decoupled.
type Options struct {
	Type         string
	Username     string
	Password     string
	ClientID     string
	ClientSecret string
	TokenFile    string
	Provider     string // "google" | "microsoft" (xoauth2 endpoint selection)
	// Service-account (domain-wide delegation) fields:
	ServiceAccountFile string
	Subject            string // user to impersonate; defaults to Username
}

// New constructs an Authenticator from options.
func New(o Options) (Authenticator, error) {
	switch o.Type {
	case "plain", "":
		return NewPlain(o.Username, o.Password), nil
	case "xoauth2":
		return NewXOAuth2(o.Username, o.ClientID, o.ClientSecret, o.TokenFile, o.Provider), nil
	case "xoauth2_service_account":
		subject := o.Subject
		if subject == "" {
			subject = o.Username
		}
		return NewServiceAccount(subject, o.ServiceAccountFile)
	default:
		return nil, fmt.Errorf("unknown auth type %q", o.Type)
	}
}
