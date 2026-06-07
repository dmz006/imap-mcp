package config

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func cfgWithPassword(pw string, dw *DatawatchConfig) *Config {
	return &Config{
		Accounts: []AccountConfig{{
			Name: "acct",
			Auth: AuthConfig{Type: "plain", Username: "u", Password: pw},
		}},
		Datawatch: dw,
	}
}

func TestResolveSecrets_StandaloneNoRefs(t *testing.T) {
	cfg := cfgWithPassword("plain-pw", nil)
	if err := resolveSecrets(cfg); err != nil {
		t.Fatalf("standalone config should not error: %v", err)
	}
	if cfg.Accounts[0].Auth.Password != "plain-pw" {
		t.Fatalf("password mutated: %q", cfg.Accounts[0].Auth.Password)
	}
}

func TestResolveSecrets_RefWithoutDatawatchBlock(t *testing.T) {
	cfg := cfgWithPassword("${secret:mail_pw}", nil)
	err := resolveSecrets(cfg)
	if err == nil {
		t.Fatal("expected error when ${secret:} used without datawatch block")
	}
	if !strings.Contains(err.Error(), "no `datawatch:` block") {
		t.Fatalf("error should explain the missing block, got: %v", err)
	}
}

func TestResolveSecrets_FetchesFromDatawatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing/wrong bearer token: %q", got)
		}
		if r.URL.Path != "/api/agents/secrets/mail_pw" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"mail_pw","value":"s3cr3t"}`))
	}))
	defer srv.Close()

	cfg := cfgWithPassword("${secret:mail_pw}", &DatawatchConfig{APIURL: srv.URL, Token: "test-token"})
	if err := resolveSecrets(cfg); err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if cfg.Accounts[0].Auth.Password != "s3cr3t" {
		t.Fatalf("password not resolved, got %q", cfg.Accounts[0].Auth.Password)
	}
}

func TestResolveSecrets_DatawatchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := cfgWithPassword("${secret:missing}", &DatawatchConfig{APIURL: srv.URL, Token: "t"})
	err := resolveSecrets(cfg)
	if err == nil {
		t.Fatal("expected error when datawatch returns non-200")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should surface status code, got: %v", err)
	}
}
