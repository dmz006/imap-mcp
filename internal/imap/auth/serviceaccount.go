package auth

import (
	"context"
	"fmt"
	"os"

	imaplib "github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/oauth2/google"
)

// ServiceAccount authenticates to Gmail/Workspace using a GCP service account
// with domain-wide delegation (2-legged JWT). The Workspace admin authorizes
// the service account's client ID for the https://mail.google.com/ scope; the
// service account then impersonates a user (Subject) to mint access tokens —
// no browser flow, no per-user refresh tokens, headless.
type ServiceAccount struct {
	subject string // Workspace user to impersonate (the mailbox owner)
	keyFile string // path to the service-account JSON key
}

func NewServiceAccount(subject, keyFile string) (*ServiceAccount, error) {
	if subject == "" {
		return nil, fmt.Errorf("service-account auth: subject (user to impersonate) is required")
	}
	if keyFile == "" {
		return nil, fmt.Errorf("service-account auth: service_account_file is required")
	}
	return &ServiceAccount{subject: subject, keyFile: keyFile}, nil
}

func (s *ServiceAccount) Type() string { return "xoauth2_service_account" }

func (s *ServiceAccount) Authenticate(ctx context.Context, c *imaplib.Client) error {
	key, err := os.ReadFile(s.keyFile)
	if err != nil {
		return fmt.Errorf("service-account: read key %s: %w", s.keyFile, err)
	}
	// JWTConfigFromJSON builds a 2-legged config; Subject enables domain-wide
	// delegation (impersonation of the target Workspace user).
	jwtCfg, err := google.JWTConfigFromJSON(key, "https://mail.google.com/")
	if err != nil {
		return fmt.Errorf("service-account: parse key: %w", err)
	}
	jwtCfg.Subject = s.subject

	tok, err := jwtCfg.TokenSource(ctx).Token()
	if err != nil {
		return fmt.Errorf("service-account: mint token for %s (check domain-wide delegation + scope): %w", s.subject, err)
	}

	saslClient := newXOAuth2SASL(s.subject, tok.AccessToken)
	if err := c.Authenticate(saslClient); err != nil {
		return fmt.Errorf("service-account xoauth2 authenticate %s: %w", s.subject, err)
	}
	return nil
}
