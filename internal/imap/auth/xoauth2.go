package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	provider     string // "google" | "microsoft"; empty = auto-detect
	cfg          *oauth2.Config
}

func NewXOAuth2(username, clientID, clientSecret, tokenFile, provider string) *XOAuth2 {
	return &XOAuth2{
		username:     username,
		clientID:     clientID,
		clientSecret: clientSecret,
		tokenFile:    tokenFile,
		provider:     provider,
	}
}

func (x *XOAuth2) Type() string { return "xoauth2" }

func (x *XOAuth2) oauthConfig() *oauth2.Config {
	if x.cfg != nil {
		return x.cfg
	}
	// Endpoint selection: an explicit provider wins (required for enterprise
	// Gmail on a custom domain, which can't be auto-detected from the address);
	// otherwise fall back to a domain heuristic.
	var isGoogle bool
	switch strings.ToLower(x.provider) {
	case "google", "gmail", "workspace":
		isGoogle = true
	case "microsoft", "outlook", "office365", "azure":
	default:
		isGoogle = isGmailAddress(x.username)
	}
	endpoint, scopes := microsoft.AzureADEndpoint("common"), microsoftScopes
	if isGoogle {
		endpoint, scopes = google.Endpoint, googleScopes
	}
	x.cfg = &oauth2.Config{
		ClientID:     x.clientID,
		ClientSecret: x.clientSecret,
		Endpoint:     endpoint,
		Scopes:       scopes,
		RedirectURL:  "http://localhost:" + callbackPort + callbackPath,
	}
	return x.cfg
}

// Scopes per provider. Google's full-mailbox scope covers IMAP and SMTP.
// Microsoft needs the Exchange Online IMAP scope, and offline_access for a
// refresh token (it ignores Google's access_type=offline).
var (
	googleScopes    = []string{"https://mail.google.com/"}
	microsoftScopes = []string{"https://outlook.office.com/IMAP.AccessAsUser.All", "offline_access"}
)

const (
	callbackPort = "8766"
	callbackPath = "/oauth/callback"
)

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
// This is invoked by the auth-setup subcommand.
func RunAuthSetup(ctx context.Context, username, clientID, clientSecret, tokenFile, provider string) error {
	a := NewXOAuth2(username, clientID, clientSecret, tokenFile, provider)
	cfg := a.oauthConfig()

	state, err := randomState()
	if err != nil {
		return err
	}
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)

	// Loopback only: the redirect URI is http://localhost, and nothing else
	// should be able to deliver an authorization code.
	// "localhost" may resolve to either loopback address, so listen on both.
	var lns []net.Listener
	for _, host := range []string{"127.0.0.1", "::1"} {
		if ln, err := net.Listen("tcp", net.JoinHostPort(host, callbackPort)); err == nil {
			lns = append(lns, ln)
		} else if host == "127.0.0.1" {
			return fmt.Errorf("listen for OAuth callback: %w", err)
		}
	}
	fmt.Printf("\nOpen this URL in your browser to authorize %s:\n\n  %s\n\n", username, authURL)
	fmt.Println("Waiting for OAuth callback on " + cfg.RedirectURL + " ...")

	results := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.Handle(callbackPath, callbackHandler(state, results))
	srv := &http.Server{Handler: mux, ReadTimeout: 2 * time.Minute}
	for _, ln := range lns {
		go srv.Serve(ln) //nolint:errcheck
	}
	defer srv.Close()

	var code string
	select {
	case res := <-results:
		if res.err != nil {
			return res.err
		}
		code = res.code
	case <-ctx.Done():
		return ctx.Err()
	}

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

type callbackResult struct {
	code string
	err  error
}

// callbackHandler accepts the first callback carrying the expected state and
// reports its code (or the provider's error). Requests with a wrong or
// missing state are rejected and do not end the flow.
func callbackHandler(state string, results chan<- callbackResult) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		res := callbackResult{code: q.Get("code")}
		switch {
		case q.Get("error") != "":
			res = callbackResult{err: fmt.Errorf("authorization denied: %s %s", q.Get("error"), q.Get("error_description"))}
			fmt.Fprint(w, "<html><body><h2>Authorization failed. See the terminal.</h2></body></html>")
		case res.code == "":
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		default:
			fmt.Fprint(w, "<html><body><h2>Authorization complete. You can close this tab.</h2></body></html>")
		}
		once.Do(func() { results <- res })
	})
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func isGmailAddress(addr string) bool {
	a := strings.ToLower(addr)
	return strings.HasSuffix(a, "@gmail.com") || strings.HasSuffix(a, "@googlemail.com")
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
