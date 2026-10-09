package auth

import (
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2/google"
)

func authURL(username, provider string) string {
	return NewXOAuth2(username, "id", "secret", "/tmp/tok.json", provider).oauthConfig().Endpoint.AuthURL
}

func TestEndpointSelection(t *testing.T) {
	googleURL := google.Endpoint.AuthURL

	// Explicit provider=google wins even for a custom enterprise domain — the fix.
	if got := authURL("user@company.com", "google"); got != googleURL {
		t.Errorf("enterprise domain + provider=google: got %q, want Google endpoint", got)
	}
	// Personal gmail still auto-detects Google with no provider.
	if got := authURL("user@gmail.com", ""); got != googleURL {
		t.Errorf("gmail.com auto-detect: got %q, want Google endpoint", got)
	}
	// Custom domain with NO provider falls back to Microsoft (documents why
	// enterprise Gmail REQUIRES provider: google).
	if got := authURL("user@company.com", ""); got == googleURL {
		t.Errorf("custom domain w/o provider should NOT be Google (got Google) — provider is required")
	}
	if !strings.Contains(authURL("user@company.com", ""), "microsoftonline.com") {
		t.Errorf("custom domain w/o provider should fall back to Microsoft endpoint")
	}
	// Provider aliases.
	for _, p := range []string{"workspace", "gmail", "Google"} {
		if got := authURL("user@company.com", p); got != googleURL {
			t.Errorf("provider %q should map to Google, got %q", p, got)
		}
	}
}

func TestServiceAccountValidation(t *testing.T) {
	if _, err := NewServiceAccount("", "/k.json"); err == nil {
		t.Error("missing subject should error")
	}
	if _, err := NewServiceAccount("u@x.com", ""); err == nil {
		t.Error("missing key file should error")
	}
	if _, err := NewServiceAccount("u@x.com", "/k.json"); err != nil {
		t.Errorf("valid args should not error: %v", err)
	}
}

func TestNewDispatch(t *testing.T) {
	// service-account: subject defaults to username
	a, err := New(Options{Type: "xoauth2_service_account", Username: "u@company.com", ServiceAccountFile: "/k.json"})
	if err != nil {
		t.Fatalf("dispatch service account: %v", err)
	}
	if a.Type() != "xoauth2_service_account" {
		t.Errorf("type = %q", a.Type())
	}
	// service-account without key file → error surfaced from constructor
	if _, err := New(Options{Type: "xoauth2_service_account", Username: "u@company.com"}); err == nil {
		t.Error("service account without key file should error")
	}
	// unknown type
	if _, err := New(Options{Type: "bogus"}); err == nil {
		t.Error("unknown type should error")
	}
	// plain + xoauth2 construct fine
	if _, err := New(Options{Type: "plain", Username: "u", Password: "p"}); err != nil {
		t.Errorf("plain: %v", err)
	}
	if _, err := New(Options{Type: "xoauth2", Username: "u@gmail.com", Provider: "google"}); err != nil {
		t.Errorf("xoauth2: %v", err)
	}
}

func TestProviderScopes(t *testing.T) {
	scopes := func(user, provider string) string {
		return strings.Join(NewXOAuth2(user, "id", "secret", "/tmp/tok.json", provider).oauthConfig().Scopes, " ")
	}
	for _, c := range []struct{ user, provider string }{
		{"user@company.com", "microsoft"}, {"user@company.com", "outlook"}, {"user@outlook.example.com", ""},
	} {
		got := scopes(c.user, c.provider)
		if !strings.Contains(got, "https://outlook.office.com/IMAP.AccessAsUser.All") || !strings.Contains(got, "offline_access") {
			t.Errorf("%s/%q: Microsoft scopes missing: %q", c.user, c.provider, got)
		}
		if strings.Contains(got, "mail.google.com") {
			t.Errorf("%s/%q: Microsoft endpoint must not request the Google scope: %q", c.user, c.provider, got)
		}
	}
	for _, c := range []struct{ user, provider string }{{"user@company.com", "google"}, {"user@gmail.com", ""}} {
		if got := scopes(c.user, c.provider); got != "https://mail.google.com/" {
			t.Errorf("%s/%q: Google scopes = %q", c.user, c.provider, got)
		}
	}
}

func TestIsGmailAddress(t *testing.T) {
	cases := map[string]bool{
		"user@gmail.com": true, "User@GoogleMail.com": true, "u@gmail.com": true,
		"x@yahoo.com": false, // 11 chars: used to panic (negative slice index)
		"ab@aol.com":  false, "a@b.co": false, "": false,
		"user@notgmail.com": false,
	}
	for addr, want := range cases {
		if got := isGmailAddress(addr); got != want {
			t.Errorf("isGmailAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestCallbackHandlerChecksState(t *testing.T) {
	results := make(chan callbackResult, 1)
	h := callbackHandler("s3cret-state", results)
	call := func(query string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/callback?"+query, nil))
		return rec.Code
	}
	if c := call("code=evil"); c != 400 {
		t.Errorf("missing state: status %d", c)
	}
	if c := call("state=wrong&code=evil"); c != 400 {
		t.Errorf("wrong state: status %d", c)
	}
	if c := call("state=s3cret-state"); c != 400 {
		t.Errorf("missing code: status %d", c)
	}
	select {
	case r := <-results:
		t.Fatalf("rejected callbacks must not end the flow, got %+v", r)
	default:
	}
	if c := call("state=s3cret-state&code=good"); c != 200 {
		t.Errorf("valid callback: status %d", c)
	}
	if r := <-results; r.err != nil || r.code != "good" {
		t.Errorf("result = %+v", r)
	}

	results = make(chan callbackResult, 1)
	h = callbackHandler("st", results)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/callback?state=st&error=access_denied", nil))
	if r := <-results; r.err == nil || !strings.Contains(r.err.Error(), "access_denied") {
		t.Errorf("provider error not reported: %+v", r)
	}
}
