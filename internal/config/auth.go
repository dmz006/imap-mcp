package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dmz006/imap-mcp/internal/httpauth"
)

// MinTokenLength is the shortest accepted bearer token. Generate tokens with
// e.g. `openssl rand -hex 32` (64 characters).
const MinTokenLength = 32

var tokenNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// unresolvedRe matches a ${...} reference left in place because the
// environment variable was unset.
var unresolvedRe = regexp.MustCompile(`\$\{[^}]+\}`)

// ServeAuth resolves and validates server.auth for the HTTP server. It is
// called only by `serve`, so stdio and run-rules never need tokens or a
// reachable datawatch.
//
// Every configured token is resolved and validated even when auth is disabled:
// an unresolvable reference always fails closed (D13a-2). With auth enabled,
// at least one token is required.
func (c *Config) ServeAuth() (tokens []httpauth.Token, disabled bool, err error) {
	a := c.Server.Auth
	var resolver *secretResolver
	for i, t := range a.Tokens {
		label := t.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if !tokenNameRe.MatchString(t.Name) {
			return nil, false, fmt.Errorf("server.auth.tokens[%s]: name must match %s", label, tokenNameRe)
		}
		val := t.Token
		if hasSecretRef(val) {
			if resolver == nil {
				if c.Datawatch == nil || strings.TrimSpace(c.Datawatch.APIURL) == "" || strings.TrimSpace(c.Datawatch.Token) == "" {
					return nil, false, fmt.Errorf("server.auth.tokens[%s]: ${secret:...} needs a datawatch block with api_url and token", label)
				}
				var rerr error
				if resolver, rerr = c.Datawatch.newResolver(); rerr != nil {
					return nil, false, rerr
				}
			}
			if val, err = resolver.expand(val); err != nil {
				return nil, false, fmt.Errorf("server.auth.tokens[%s]: %w", label, err)
			}
		}
		if unresolvedRe.MatchString(val) {
			return nil, false, fmt.Errorf("server.auth.tokens[%s]: token reference is unresolved (environment variable unset?)", label)
		}
		if len(val) < MinTokenLength {
			return nil, false, fmt.Errorf("server.auth.tokens[%s]: token must be at least %d characters", label, MinTokenLength)
		}
		if len(t.Scopes) == 0 {
			return nil, false, fmt.Errorf("server.auth.tokens[%s]: at least one scope is required", label)
		}
		tok := httpauth.Token{Name: t.Name, Value: val}
		for _, s := range t.Scopes {
			sc, err := httpauth.ParseScope(s)
			if err != nil {
				return nil, false, fmt.Errorf("server.auth.tokens[%s]: %w", label, err)
			}
			tok.Scopes = append(tok.Scopes, sc)
		}
		tokens = append(tokens, tok)
	}
	if a.Disabled {
		return nil, true, nil
	}
	if len(tokens) == 0 {
		return nil, false, fmt.Errorf("server.auth: no tokens configured; add server.auth.tokens or set server.auth.disabled: true (insecure)")
	}
	return tokens, false, nil
}
