package auth

import (
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
