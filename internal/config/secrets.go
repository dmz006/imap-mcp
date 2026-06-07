// Package config — datawatch secrets resolution.
//
// imap-mcp supports two modes of operation, decided entirely by configuration:
//
//   - Standalone: credentials come from plain values or ${ENV_VAR} references.
//     No datawatch dependency. This is the default and always works.
//
//   - datawatch-integrated: credentials may use ${secret:name} references that
//     resolve against a running datawatch secrets service. Activated only when a
//     `datawatch:` block is present in the config. If a ${secret:name} reference
//     appears without a datawatch block, loading fails with a clear error rather
//     than silently leaving a placeholder in place.
//
// Resolution order for any credential string:
//  1. ${ENV_VAR}      → expanded from the environment before YAML parse
//  2. ${secret:name}  → fetched from datawatch (this file), after YAML parse
//  3. plain value     → used as-is
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// DatawatchConfig configures optional integration with a datawatch secrets
// service. When absent, imap-mcp runs fully standalone and ${secret:...}
// references are an error.
type DatawatchConfig struct {
	// APIURL is the base URL of the datawatch HTTP API, e.g. http://localhost:7777.
	APIURL string `yaml:"api_url"`
	// Token is a datawatch scoped secrets token (Bearer). Prefer an
	// agent-scoped token over the operator token — least privilege.
	Token string `yaml:"token"`
}

// secretRefRe matches ${secret:NAME} references. NAME may contain letters,
// digits, dashes, underscores, and dots.
var secretRefRe = regexp.MustCompile(`\$\{secret:([A-Za-z0-9._-]+)\}`)

// hasSecretRef reports whether s contains any ${secret:name} reference.
func hasSecretRef(s string) bool {
	return secretRefRe.MatchString(s)
}

// resolveSecrets walks every credential-bearing field in the config and
// replaces ${secret:name} references with values fetched from datawatch.
//
// It returns an error if a reference is present but no datawatch block is
// configured, or if a referenced secret cannot be fetched.
func resolveSecrets(cfg *Config) error {
	// Collect whether any secret references exist at all.
	anyRef := false
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i].Auth
		if hasSecretRef(a.Password) || hasSecretRef(a.ClientID) ||
			hasSecretRef(a.ClientSecret) || hasSecretRef(a.Username) {
			anyRef = true
		}
		if s := cfg.Accounts[i].SMTP; s != nil && (hasSecretRef(s.Password) || hasSecretRef(s.Username)) {
			anyRef = true
		}
		if in := cfg.Accounts[i].Inbound; in != nil && hasSecretRef(in.Gates.HMACSecret) {
			anyRef = true
		}
	}
	if !anyRef {
		return nil // standalone path — nothing to do
	}

	if cfg.Datawatch == nil {
		return fmt.Errorf("config uses ${secret:...} references but no `datawatch:` block is configured; " +
			"add a datawatch block with api_url and token, or use ${ENV_VAR}/plain values to run standalone")
	}
	if strings.TrimSpace(cfg.Datawatch.APIURL) == "" {
		return fmt.Errorf("datawatch.api_url is required to resolve ${secret:...} references")
	}
	if strings.TrimSpace(cfg.Datawatch.Token) == "" {
		return fmt.Errorf("datawatch.token is required to resolve ${secret:...} references")
	}

	r := newSecretResolver(cfg.Datawatch.APIURL, cfg.Datawatch.Token)
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i].Auth
		acct := cfg.Accounts[i].Name
		fields := []*string{&a.Password, &a.ClientID, &a.ClientSecret, &a.Username}
		if s := cfg.Accounts[i].SMTP; s != nil {
			fields = append(fields, &s.Password, &s.Username)
		}
		if in := cfg.Accounts[i].Inbound; in != nil {
			fields = append(fields, &in.Gates.HMACSecret)
		}
		for _, f := range fields {
			resolved, err := r.expand(*f)
			if err != nil {
				return fmt.Errorf("account %q: %w", acct, err)
			}
			*f = resolved
		}
	}
	return nil
}

// secretResolver fetches and caches secret values from a datawatch instance.
type secretResolver struct {
	apiURL string
	token  string
	client *http.Client
	cache  map[string]string
}

func newSecretResolver(apiURL, token string) *secretResolver {
	return &secretResolver{
		apiURL: strings.TrimRight(apiURL, "/"),
		token:  token,
		client: &http.Client{Timeout: 10 * time.Second},
		cache:  map[string]string{},
	}
}

// expand replaces every ${secret:name} in s with its fetched value.
func (r *secretResolver) expand(s string) (string, error) {
	if !hasSecretRef(s) {
		return s, nil
	}
	var firstErr error
	out := secretRefRe.ReplaceAllStringFunc(s, func(m string) string {
		name := secretRefRe.FindStringSubmatch(m)[1]
		val, err := r.fetch(name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return m
		}
		return val
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// fetch retrieves a single secret value from datawatch, using the
// agent-scoped endpoint (least privilege). Results are cached per resolver.
func (r *secretResolver) fetch(name string) (string, error) {
	if v, ok := r.cache[name]; ok {
		return v, nil
	}
	url := fmt.Sprintf("%s/api/agents/secrets/%s", r.apiURL, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("secret %q: build request: %w", name, err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("secret %q: datawatch unreachable at %s: %w", name, r.apiURL, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secret %q: datawatch returned %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("secret %q: decode datawatch response: %w", name, err)
	}
	r.cache[name] = out.Value
	return out.Value, nil
}
