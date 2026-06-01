package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	imaplib "github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/microsoft"
)

type XOAuth2 struct {
	username     string
	clientID     string
	clientSecret string
	tokenFile    string
	cfg          *oauth2.Config
}

func NewXOAuth2(username, clientID, clientSecret, tokenFile string) *XOAuth2 {
	return &XOAuth2{
		username:     username,
		clientID:     clientID,
		clientSecret: clientSecret,
		tokenFile:    tokenFile,
	}
}

func (x *XOAuth2) Type() string { return "xoauth2" }

func (x *XOAuth2) oauthConfig() *oauth2.Config {
	if x.cfg != nil {
		return x.cfg
	}
	// Detect provider from username domain
	var endpoint oauth2.Endpoint
	switch {
	case isGmailAddress(x.username):
		endpoint = google.Endpoint
	default:
		endpoint = microsoft.AzureADEndpoint("common")
	}
	x.cfg = &oauth2.Config{
		ClientID:     x.clientID,
		ClientSecret: x.clientSecret,
		Endpoint:     endpoint,
		Scopes:       []string{"https://mail.google.com/"},
		RedirectURL:  "http://localhost:8766/oauth/callback",
	}
	return x.cfg
}

func (x *XOAuth2) Authenticate(ctx context.Context, c *imaplib.Client) error {
	token, err := x.loadToken()
	if err != nil {
		return fmt.Errorf("xoauth2 load token: %w", err)
	}

	cfg := x.oauthConfig()
	ts := cfg.TokenSource(ctx, token)
	refreshed, err := ts.Token()
	if err != nil {
		return fmt.Errorf("xoauth2 refresh token: %w", err)
	}

	if refreshed.AccessToken != token.AccessToken {
		if err := x.saveToken(refreshed); err != nil {
			return fmt.Errorf("xoauth2 save refreshed token: %w", err)
		}
	}

	saslClient := newXOAuth2SASL(x.username, refreshed.AccessToken)
	if err := c.Authenticate(saslClient); err != nil {
		return fmt.Errorf("xoauth2 authenticate %s: %w", x.username, err)
	}
	return nil
}

func (x *XOAuth2) loadToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(x.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token file %s: %w", x.tokenFile, err)
	}
	var t oauth2.Token
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}
	return &t, nil
}

func (x *XOAuth2) saveToken(t *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(x.tokenFile), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(x.tokenFile, data, 0600)
}

// RunAuthSetup runs the browser-based OAuth2 flow and saves the token.
// This is invoked by the --auth-setup subcommand.
func RunAuthSetup(ctx context.Context, username, clientID, clientSecret, tokenFile string) error {
	a := NewXOAuth2(username, clientID, clientSecret, tokenFile)
	cfg := a.oauthConfig()

	state := fmt.Sprintf("imap-mcp-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)

	fmt.Printf("\nOpen this URL in your browser to authorize %s:\n\n  %s\n\n", username, authURL)
	fmt.Println("Waiting for OAuth callback on http://localhost:8766/oauth/callback ...")

	codeCh := make(chan string, 1)
	srv := &http.Server{
		Addr:        ":8766",
		ReadTimeout: 2 * time.Minute,
	}
	http.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		fmt.Fprintf(w, "<html><body><h2>Authorization complete. You can close this tab.</h2></body></html>")
		codeCh <- code
	})
	go srv.ListenAndServe() //nolint:errcheck

	var code string
	select {
	case code = <-codeCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	srv.Shutdown(ctx) //nolint:errcheck

	token, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("exchange code: %w", err)
	}
	if err := a.saveToken(token); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Printf("Token saved to %s\n", tokenFile)
	return nil
}

func isGmailAddress(addr string) bool {
	return len(addr) > 10 && (addr[len(addr)-9:] == "gmail.com" || addr[len(addr)-14:] == "googlemail.com")
}

// xOAuth2SASL implements the XOAUTH2 SASL mechanism for go-imap.
type xOAuth2SASL struct {
	username    string
	accessToken string
	done        bool
}

func newXOAuth2SASL(username, accessToken string) *xOAuth2SASL {
	return &xOAuth2SASL{username: username, accessToken: accessToken}
}

func (s *xOAuth2SASL) Start() (mech string, ir []byte, err error) {
	s.done = true
	ir = []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", s.username, s.accessToken))
	return "XOAUTH2", ir, nil
}

func (s *xOAuth2SASL) Next(challenge []byte) ([]byte, error) {
	// XOAUTH2 is single-round; if the server sends a challenge it's an error JSON
	if len(challenge) > 0 {
		return []byte{}, fmt.Errorf("XOAUTH2 server error: %s", challenge)
	}
	return nil, nil
}
