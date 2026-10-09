package config

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodTok = "0123456789abcdef0123456789abcdef"

func authCfg(a ServerAuthConfig) *Config {
	return &Config{Server: ServerConfig{Auth: a}}
}

func TestServeAuthValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"no tokens, not disabled", authCfg(ServerAuthConfig{}), "no tokens configured"},
		{"bad name", authCfg(ServerAuthConfig{Tokens: []TokenConfig{{Name: "a b", Token: goodTok, Scopes: []string{"read"}}}}), "name must match"},
		{"empty name", authCfg(ServerAuthConfig{Tokens: []TokenConfig{{Token: goodTok, Scopes: []string{"read"}}}}), "#1"},
		{"short token", authCfg(ServerAuthConfig{Tokens: []TokenConfig{{Name: "a", Token: "short", Scopes: []string{"read"}}}}), "at least 32"},
		{"unresolved env", authCfg(ServerAuthConfig{Tokens: []TokenConfig{{Name: "a", Token: "${IMAP_MCP_TEST_UNSET_TOKEN_VAR}", Scopes: []string{"read"}}}}), "unresolved"},
		{"no scopes", authCfg(ServerAuthConfig{Tokens: []TokenConfig{{Name: "a", Token: goodTok}}}), "at least one scope"},
		{"bad scope", authCfg(ServerAuthConfig{Tokens: []TokenConfig{{Name: "a", Token: goodTok, Scopes: []string{"root"}}}}), "unknown scope"},
		{"secret without datawatch", authCfg(ServerAuthConfig{Tokens: []TokenConfig{{Name: "a", Token: "${secret:x}", Scopes: []string{"read"}}}}), "datawatch block"},
		{"disabled still fails closed on bad ref", authCfg(ServerAuthConfig{Disabled: true, Tokens: []TokenConfig{{Name: "a", Token: "${secret:x}", Scopes: []string{"read"}}}}), "datawatch block"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := c.cfg.ServeAuth()
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestServeAuthDisabledAndValid(t *testing.T) {
	toks, disabled, err := authCfg(ServerAuthConfig{Disabled: true}).ServeAuth()
	if err != nil || !disabled || toks != nil {
		t.Fatalf("disabled: toks=%v disabled=%v err=%v", toks, disabled, err)
	}
	toks, disabled, err = authCfg(ServerAuthConfig{Tokens: []TokenConfig{
		{Name: "claude", Token: goodTok, Scopes: []string{"read", "write", "send", "admin"}},
	}}).ServeAuth()
	if err != nil || disabled || len(toks) != 1 || len(toks[0].Scopes) != 4 || toks[0].Value != goodTok {
		t.Fatalf("valid: toks=%+v disabled=%v err=%v", toks, disabled, err)
	}
}

func TestServeAuthResolvesSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dw-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/external/secrets/imap_mcp_token_datawatch":
			_, _ = w.Write([]byte(`{"name":"imap_mcp_token_datawatch","value":"` + goodTok + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := authCfg(ServerAuthConfig{Tokens: []TokenConfig{
		{Name: "datawatch", Token: "${secret:imap_mcp_token_datawatch}", Scopes: []string{"read", "send"}},
	}})
	cfg.Datawatch = &DatawatchConfig{APIURL: srv.URL, Token: "dw-token"}
	toks, _, err := cfg.ServeAuth()
	if err != nil || len(toks) != 1 || toks[0].Value != goodTok {
		t.Fatalf("toks=%+v err=%v", toks, err)
	}

	cfg.Server.Auth.Tokens[0].Token = "${secret:missing}"
	if _, _, err := cfg.ServeAuth(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing secret: err = %v, want 404 failure", err)
	}
}

func TestAuthDisabledEnvOverride(t *testing.T) {
	t.Setenv("IMAP_MCP_SERVER_AUTH_DISABLED", "true")
	cfg := defaults()
	applyEnvOverrides(cfg)
	if !cfg.Server.Auth.Disabled {
		t.Fatal("IMAP_MCP_SERVER_AUTH_DISABLED=true not applied")
	}
}
