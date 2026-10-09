package config

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDBKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/external/secrets/cache_key" {
			_, _ = w.Write([]byte(`{"name":"cache_key","value":"from-datawatch"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &Config{DB: DBConfig{EncryptionKey: "state-pass", Cache: CacheDBConfig{EncryptionKey: "${secret:cache_key}"}}}
	cfg.Datawatch = &DatawatchConfig{APIURL: srv.URL, Token: "t"}

	k, err := cfg.ResolveDBKeys(true, true)
	if err != nil || k.State != "state-pass" || k.Cache != "from-datawatch" {
		t.Fatalf("keys=%+v err=%v", k, err)
	}

	// State-only never resolves (or needs) the cache key.
	cfg.Datawatch = nil
	k, err = cfg.ResolveDBKeys(true, false)
	if err != nil || k.State != "state-pass" || k.Cache != "" {
		t.Fatalf("state-only: keys=%+v err=%v", k, err)
	}
	if _, err := cfg.ResolveDBKeys(true, true); err == nil || !strings.Contains(err.Error(), "datawatch block") {
		t.Fatalf("secret without datawatch: %v", err)
	}

	// Plaintext when unset.
	k, err = (&Config{}).ResolveDBKeys(true, true)
	if err != nil || k.State != "" || k.Cache != "" {
		t.Fatalf("plaintext: %+v %v", k, err)
	}

	// Unresolved ${ENV} fails closed.
	bad := &Config{DB: DBConfig{EncryptionKey: "${IMAP_MCP_TEST_UNSET_DB_KEY}"}}
	if _, err := bad.ResolveDBKeys(true, false); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("unresolved env: %v", err)
	}
}

func TestCachePathDefaultsNextToState(t *testing.T) {
	cfg := defaults()
	cfg.DB.Path = "/var/lib/imap-mcp/imap.db"
	expandPaths(cfg)
	if want := filepath.Join("/var/lib/imap-mcp", "cache.db"); cfg.DB.Cache.Path != want {
		t.Fatalf("cache path = %q, want %q", cfg.DB.Cache.Path, want)
	}
	t.Setenv("IMAP_MCP_DB_CACHE_PATH", "/tmp/c.db")
	t.Setenv("IMAP_MCP_DB_CACHE_ENCRYPTION_KEY", "k")
	cfg = defaults()
	applyEnvOverrides(cfg)
	if cfg.DB.Cache.Path != "/tmp/c.db" || cfg.DB.Cache.EncryptionKey != "k" {
		t.Fatalf("env overrides not applied: %+v", cfg.DB.Cache)
	}
}
