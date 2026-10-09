package httpauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	tokAll  = "all-scopes-token-0123456789abcdef0123"
	tokRead = "read-send-token-0123456789abcdef01234"
)

func newTestAuth(t *testing.T) *Authenticator {
	t.Helper()
	a, err := New([]Token{
		{Name: "claude", Value: tokAll, Scopes: AllScopes},
		{Name: "datawatch", Value: tokRead, Scopes: []Scope{ScopeRead, ScopeSend}},
	}, []string{"/api/health"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRejectsBadTokenSets(t *testing.T) {
	if _, err := New(nil, nil, nil); err == nil {
		t.Error("no tokens: want error")
	}
	if _, err := New([]Token{{Name: "a", Value: "x"}, {Name: "a", Value: "y"}}, nil, nil); err == nil || !strings.Contains(err.Error(), "duplicate token name") {
		t.Errorf("duplicate name: got %v", err)
	}
	if _, err := New([]Token{{Name: "a", Value: "x"}, {Name: "b", Value: "x"}}, nil, nil); err == nil || !strings.Contains(err.Error(), "reuses the value") {
		t.Errorf("duplicate value: got %v", err)
	}
}

func TestParseScope(t *testing.T) {
	for _, s := range AllScopes {
		if got, err := ParseScope(string(s)); err != nil || got != s {
			t.Errorf("ParseScope(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := ParseScope("root"); err == nil {
		t.Error("unknown scope: want error")
	}
}

func TestMiddleware(t *testing.T) {
	a := newTestAuth(t)
	var seen *Principal
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name, path, header string
		wantCode           int
		wantPrincipal      string
	}{
		{"open path, no token", "/api/health", "", 200, ""},
		{"missing token", "/api/accounts", "", 401, ""},
		{"wrong scheme", "/api/accounts", "Basic " + tokAll, 401, ""},
		{"invalid token", "/api/accounts", "Bearer nope", 401, ""},
		{"token prefix only", "/api/accounts", "Bearer " + tokAll[:10], 401, ""},
		{"valid token", "/api/accounts", "Bearer " + tokAll, 200, "claude"},
		{"case-insensitive scheme", "/mcp", "bearer " + tokRead, 200, "datawatch"},
		{"open path is exact, not prefix", "/api/health/x", "", 401, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen = nil
			req := httptest.NewRequest("GET", c.path, nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, c.wantCode)
			}
			if c.wantCode == 401 && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
			got := ""
			if seen != nil {
				got = seen.Name
			}
			if got != c.wantPrincipal {
				t.Errorf("principal = %q, want %q", got, c.wantPrincipal)
			}
		})
	}
}

func TestRequireScope(t *testing.T) {
	a := newTestAuth(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	chain := func(s Scope) http.Handler { return a.Middleware(a.RequireScope(s)(ok)) }

	cases := []struct {
		token string
		scope Scope
		want  int
	}{
		{tokAll, ScopeWrite, 200},
		{tokAll, ScopeAdmin, 200},
		{tokRead, ScopeRead, 200},
		{tokRead, ScopeSend, 200},
		{tokRead, ScopeWrite, 403},
		{tokRead, ScopeAdmin, 403},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		rec := httptest.NewRecorder()
		chain(c.scope).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("token %s.. scope %s: code %d, want %d", c.token[:4], c.scope, rec.Code, c.want)
		}
	}

	// RequireScope without a principal in context (middleware bypassed) denies.
	rec := httptest.NewRecorder()
	a.RequireScope(ScopeRead)(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
	if rec.Code != 403 {
		t.Errorf("no principal: code %d, want 403", rec.Code)
	}
}

func TestNilAuthenticatorPassesThrough(t *testing.T) {
	var a *Authenticator
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	a.Middleware(a.RequireScope(ScopeAdmin)(ok)).ServeHTTP(rec, httptest.NewRequest("POST", "/api/query", nil))
	if rec.Code != 200 {
		t.Errorf("disabled auth: code %d, want 200", rec.Code)
	}
}

func TestPrincipalScopes(t *testing.T) {
	a := newTestAuth(t)
	p := a.lookup(tokRead)
	if p == nil || strings.Join(p.Scopes(), ",") != "read,send" {
		t.Fatalf("scopes = %v", p.Scopes())
	}
	var nilP *Principal
	if nilP.Has(ScopeRead) {
		t.Error("nil principal must have no scopes")
	}
}
