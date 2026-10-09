// Package httpauth enforces named, scoped bearer-token authentication on the
// HTTP server (REST at /api, MCP at /mcp). See AGENT.md D13a / D13a-2.
//
// A request authenticates with `Authorization: Bearer <token>`. The matching
// token's name and scopes become the request's Principal, carried in the
// context so REST routes (RequireScope) and MCP tools (ToolMiddleware) can each
// check the one scope they need. Token values are never logged; names are.
package httpauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// Scope is a permission granted to a token.
type Scope string

const (
	// ScopeRead covers read-only access to accounts, folders, messages, search,
	// analytics, rules listing, the event stream and the output sandbox.
	ScopeRead Scope = "read"
	// ScopeWrite covers mailbox changes, rules changes and sandbox writes.
	ScopeWrite Scope = "write"
	// ScopeSend covers outbound mail (SMTP).
	ScopeSend Scope = "send"
	// ScopeAdmin covers webhooks, the query DSL, sync and enrichment triggers.
	ScopeAdmin Scope = "admin"
)

// AllScopes lists every valid scope.
var AllScopes = []Scope{ScopeRead, ScopeWrite, ScopeSend, ScopeAdmin}

// ParseScope validates a scope name.
func ParseScope(s string) (Scope, error) {
	for _, sc := range AllScopes {
		if string(sc) == s {
			return sc, nil
		}
	}
	return "", fmt.Errorf("unknown scope %q (valid: read, write, send, admin)", s)
}

// Token is one configured credential.
type Token struct {
	Name   string
	Value  string
	Scopes []Scope
}

// Principal is the authenticated caller of a request.
type Principal struct {
	Name   string
	scopes map[Scope]bool
}

// Has reports whether the principal holds scope.
func (p *Principal) Has(s Scope) bool { return p != nil && p.scopes[s] }

// Scopes returns the principal's scopes, sorted.
func (p *Principal) Scopes() []string {
	out := make([]string, 0, len(p.scopes))
	for s := range p.scopes {
		out = append(out, string(s))
	}
	sort.Strings(out)
	return out
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the request's principal, or nil when the request did not
// pass through an enforcing Authenticator (stdio MCP, or auth disabled).
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

type entry struct {
	hash      [sha256.Size]byte
	principal *Principal
}

// Authenticator validates bearer tokens. A nil *Authenticator means auth is
// disabled: Middleware and RequireScope pass every request through.
type Authenticator struct {
	entries []entry
	open    map[string]bool // exact paths reachable without a token
	log     *slog.Logger
}

// New builds an Authenticator. openPaths are exact URL paths that skip
// authentication (e.g. /api/health). Tokens must have unique names and values.
func New(tokens []Token, openPaths []string, log *slog.Logger) (*Authenticator, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("no tokens configured")
	}
	a := &Authenticator{open: map[string]bool{}, log: log}
	for _, p := range openPaths {
		a.open[p] = true
	}
	names := map[string]bool{}
	for _, t := range tokens {
		if names[t.Name] {
			return nil, fmt.Errorf("duplicate token name %q", t.Name)
		}
		names[t.Name] = true
		h := sha256.Sum256([]byte(t.Value))
		for _, e := range a.entries {
			if e.hash == h {
				return nil, fmt.Errorf("token %q reuses the value of token %q", t.Name, e.principal.Name)
			}
		}
		p := &Principal{Name: t.Name, scopes: map[Scope]bool{}}
		for _, s := range t.Scopes {
			p.scopes[s] = true
		}
		a.entries = append(a.entries, entry{hash: h, principal: p})
	}
	return a, nil
}

// lookup returns the principal for a presented token. It hashes the input and
// compares against every configured token in constant time, so neither length
// nor position of a match leaks through timing.
func (a *Authenticator) lookup(presented string) *Principal {
	h := sha256.Sum256([]byte(presented))
	var found *Principal
	for _, e := range a.entries {
		if subtle.ConstantTimeCompare(h[:], e.hash[:]) == 1 {
			found = e.principal
		}
	}
	return found
}

// bearer extracts the token from an Authorization header.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// Middleware authenticates every request except open paths. Missing or
// invalid tokens get 401; a valid token attaches its Principal to the context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.open[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		tok, ok := bearer(r)
		var p *Principal
		if ok {
			p = a.lookup(tok)
		}
		if p == nil {
			if a.log != nil {
				a.log.Warn("auth: rejected request", "path", r.URL.Path, "method", r.Method,
					"remote", r.RemoteAddr, "reason", map[bool]string{true: "invalid token", false: "missing bearer token"}[ok])
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="imap-mcp"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized: valid bearer token required")
			return
		}
		if a.log != nil {
			a.log.Debug("auth: request", "token", p.Name, "path", r.URL.Path, "method", r.Method)
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireScope is per-route middleware that answers 403 unless the request's
// principal holds scope. With auth disabled (nil Authenticator) it is a no-op.
func (a *Authenticator) RequireScope(scope Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if a == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := FromContext(r.Context())
			if !p.Has(scope) {
				name := ""
				if p != nil {
					name = p.Name
				}
				if a.log != nil {
					a.log.Warn("auth: scope denied", "token", name, "path", r.URL.Path, "required", string(scope))
				}
				writeErr(w, http.StatusForbidden, "forbidden: token lacks scope "+string(scope))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"error\":%q}\n", msg)
}
