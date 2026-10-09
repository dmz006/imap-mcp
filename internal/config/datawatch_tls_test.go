package config

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCert(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cert.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDatawatchCAFilePinsSelfSignedCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"mail_pw","value":"s3cr3t"}`))
	}))
	defer srv.Close()

	// Without the pinned cert the self-signed server is rejected.
	cfg := cfgWithPassword("${secret:mail_pw}", &DatawatchConfig{APIURL: srv.URL, Token: "t"})
	if err := resolveSecrets(cfg); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("unpinned self-signed cert: %v", err)
	}

	// With ca_file it verifies and resolves.
	cfg = cfgWithPassword("${secret:mail_pw}", &DatawatchConfig{APIURL: srv.URL, Token: "t", CAFile: writeCert(t, srv)})
	if err := resolveSecrets(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Accounts[0].Auth.Password != "s3cr3t" {
		t.Fatalf("password = %q", cfg.Accounts[0].Auth.Password)
	}
}

func TestDatawatchCAFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(notPEM, []byte("not a certificate"), 0o644)
	for name, f := range map[string]string{"missing": filepath.Join(dir, "nope.pem"), "junk": notPEM} {
		d := &DatawatchConfig{APIURL: "https://127.0.0.1:1", Token: "t", CAFile: f}
		if _, err := d.Transport(); err == nil || !strings.Contains(err.Error(), "ca_file") {
			t.Errorf("%s: %v", name, err)
		}
		cfg := cfgWithPassword("plain", d)
		if err := validate(cfg); err == nil || !strings.Contains(err.Error(), "ca_file") {
			t.Errorf("%s: validate should fail closed, got %v", name, err)
		}
	}
}
