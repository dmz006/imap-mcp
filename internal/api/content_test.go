package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

const (
	readToken  = "read-token-0123456789-0123456789-abcdef"
	writeToken = "write-token-0123456789-0123456789-abcdef"
)

// contentServer runs the REST API (with token auth) over a test IMAP server
// holding a message with one attachment, and a reply to it.
func contentServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := imaptest.Start(t, nil)
	srv.Append(t, "INBOX", "From: alice@example.com\r\nSubject: Report\r\nMessage-ID: <root@example.com>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=XX\r\n\r\n--XX\r\nContent-Type: text/plain\r\n\r\nbody\r\n"+
		"--XX\r\nContent-Type: text/html\r\nContent-Disposition: attachment; filename=\"../report.html\"\r\nContent-Transfer-Encoding: base64\r\n\r\n"+
		base64.StdEncoding.EncodeToString([]byte("<script>x</script>"))+"\r\n--XX--\r\n", time.Now().Add(-time.Hour))
	srv.Append(t, "INBOX", "From: bob@example.com\r\nSubject: Re: Report\r\nMessage-ID: <r1@example.com>\r\nIn-Reply-To: <root@example.com>\r\n"+
		"References: <root@example.com>\r\n\r\nthanks\r\n", time.Now())
	cfg := &config.Config{
		Accounts: []config.AccountConfig{srv.Account("work")},
		Tools:    config.ToolsConfig{AttachmentInlineKB: 64, AttachmentMaxMB: 25, ExportMaxMessages: 500, ExportMaxMB: 100},
	}
	d, err := db.Open(db.Options{Path: t.TempDir() + "/imap.db"}, db.Options{Path: t.TempDir() + "/cache.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn, err := httpauth.New([]httpauth.Token{
		{Name: "reader", Value: readToken, Scopes: []httpauth.Scope{httpauth.ScopeRead}},
		{Name: "writer", Value: writeToken, Scopes: []httpauth.Scope{httpauth.ScopeRead, httpauth.ScopeWrite}},
	}, []string{"/api/health"}, log)
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(cfg, imaptest.Pool(t, cfg, nil), d, nil, nil, nil, log, authn)
	hs := httptest.NewServer(authn.Middleware(api.Router()))
	t.Cleanup(hs.Close)
	return hs
}

func do(t *testing.T, method, u, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRESTContentRoutes(t *testing.T) {
	hs := contentServer(t)
	msg := hs.URL + "/api/accounts/work/folders/INBOX/messages/1"

	// Reads: attachment list, thread, cross-account search.
	resp := do(t, "GET", msg+"/attachments", readToken, "")
	var list struct {
		Count       int
		Attachments []struct{ Part, Filename string }
	}
	json.NewDecoder(resp.Body).Decode(&list) //nolint:errcheck
	if resp.StatusCode != 200 || list.Count != 1 || list.Attachments[0].Part != "2" {
		t.Fatalf("attachments: %d %+v", resp.StatusCode, list)
	}
	resp = do(t, "GET", hs.URL+"/api/threads/"+url.PathEscape("<root@example.com>"), readToken, "")
	var th struct{ Count int }
	json.NewDecoder(resp.Body).Decode(&th) //nolint:errcheck
	if resp.StatusCode != 200 || th.Count != 2 {
		t.Fatalf("thread: %d %+v", resp.StatusCode, th)
	}
	resp = do(t, "GET", hs.URL+"/api/search/cross?from=bob&live=true", readToken, "")
	var xs struct{ Count int }
	json.NewDecoder(resp.Body).Decode(&xs) //nolint:errcheck
	if resp.StatusCode != 200 || xs.Count != 1 {
		t.Fatalf("cross search: %d %+v", resp.StatusCode, xs)
	}

	// Downloads need write.
	for _, r := range []struct{ method, url, body string }{
		{"GET", msg + "/attachments/2", ""},
		{"GET", msg + "/export.eml", ""},
		{"POST", hs.URL + "/api/export", `{"folder":"INBOX","uids":[1,2]}`},
	} {
		if resp := do(t, r.method, r.url, readToken, r.body); resp.StatusCode != http.StatusForbidden {
			t.Errorf("read token %s %s: %d, want 403", r.method, r.url, resp.StatusCode)
		}
	}

	resp = do(t, "GET", msg+"/attachments/2", writeToken, "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "<script>x</script>" {
		t.Fatalf("download: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("download served as %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename=report.html` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	resp = do(t, "GET", msg+"/export.eml", writeToken, "")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(body), "From: alice@example.com\r\n") {
		t.Fatalf("eml: %d %q", resp.StatusCode, body)
	}

	resp = do(t, "POST", hs.URL+"/api/export", writeToken, `{"thread_id":"root@example.com"}`)
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Export-Count") != "2" || !strings.HasPrefix(string(body), "From alice@example.com ") {
		t.Fatalf("mbox: %d %s", resp.StatusCode, resp.Header.Get("X-Export-Count"))
	}

	for _, bad := range []struct {
		method, url, body string
		code              int
	}{
		{"POST", hs.URL + "/api/export", `{}`, 400},
		{"GET", msg + "/attachments/0", "", 400},
		{"GET", msg + "/attachments/1", "", 404}, // the body text, not an attachment
		{"GET", hs.URL + "/api/search/cross", "", 400},
	} {
		if resp := do(t, bad.method, bad.url, writeToken, bad.body); resp.StatusCode != bad.code {
			t.Errorf("%s %s: %d, want %d", bad.method, bad.url, resp.StatusCode, bad.code)
		}
	}
}

func TestRESTResolveAnomaly(t *testing.T) {
	hs := contentServer(t)
	if resp := do(t, "POST", hs.URL+"/api/anomalies/1/resolve", readToken, "{}"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read token: %d, want 403", resp.StatusCode)
	}
	if resp := do(t, "POST", hs.URL+"/api/anomalies/1/resolve", writeToken, "{}"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", resp.StatusCode)
	}
	if resp := do(t, "POST", hs.URL+"/api/anomalies/x/resolve", writeToken, "{}"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: %d, want 400", resp.StatusCode)
	}
}

// TestIntelProgressNamesOnlyBehindAuth (D29): health is open and must not name
// accounts; /api/intelligence/status names them and needs a token.
func TestIntelProgressNamesOnlyBehindAuth(t *testing.T) {
	hs := contentServer(t)
	resp, err := http.Get(hs.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(body), `"work"`) || !strings.Contains(string(body), `"index":1`) ||
		!strings.Contains(string(body), `"hold_digest_hour"`) {
		t.Errorf("health %d: %s", resp.StatusCode, body)
	}
	if resp, _ := http.Get(hs.URL + "/api/intelligence/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status without token: %d", resp.StatusCode)
	}
	resp = do(t, "GET", hs.URL+"/api/intelligence/status", readToken, "")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"account":"work"`) {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
}
