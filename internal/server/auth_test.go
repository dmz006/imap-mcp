package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/imap"
)

const (
	e2eAll  = "e2e-claude-token-0123456789abcdef0123"
	e2eDW   = "e2e-datawatch-token-0123456789abcdef0"
	e2eHost = "127.0.0.1"
)

// newTestServer builds the real handler stack (browserGuard → auth → MCP/REST)
// with no IMAP accounts connected.
func newTestServer(t *testing.T, enforce bool) *httptest.Server {
	t.Helper()
	cfg := &config.Config{Server: config.ServerConfig{Host: e2eHost}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var authn *httpauth.Authenticator
	if enforce {
		var err error
		authn, err = httpauth.New([]httpauth.Token{
			{Name: "claude", Value: e2eAll, Scopes: httpauth.AllScopes},
			{Name: "datawatch", Value: e2eDW, Scopes: []httpauth.Scope{httpauth.ScopeRead, httpauth.ScopeSend}},
		}, []string{"/api/health"}, log)
		if err != nil {
			t.Fatal(err)
		}
	}
	s := New(cfg, imap.NewPool(cfg, nil, log), nil, nil, nil, nil, nil, log, authn)
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestRESTAuthEndToEnd(t *testing.T) {
	ts := newTestServer(t, true)
	cases := []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"health open", "GET", "/api/health", "", "", 200},
		{"accounts needs token", "GET", "/api/accounts", "", "", 401},
		{"accounts bad token", "GET", "/api/accounts", "wrong", "", 401},
		{"accounts with datawatch token", "GET", "/api/accounts", e2eDW, "", 200},
		{"events needs token", "GET", "/api/events", "", "", 401},
		{"send with datawatch token reaches handler", "POST", "/api/accounts/_default/messages/send", e2eDW, `{}`, 400},
		{"rules write denied for datawatch", "POST", "/api/rules", e2eDW, `{}`, 403},
		{"webhooks admin denied for datawatch", "GET", "/api/webhooks", e2eDW, "", 403},
		{"query admin denied for datawatch", "POST", "/api/query", e2eDW, `{}`, 403},
		{"delete message denied for datawatch", "DELETE", "/api/accounts/a/folders/INBOX/messages/1", e2eDW, `{}`, 403},
		{"rules write allowed for claude reaches validation", "POST", "/api/rules", e2eAll, `{}`, 400},
		{"query allowed for claude (still a stub)", "POST", "/api/query", e2eAll, `{}`, 501},
		{"cache sweep admin denied for datawatch", "POST", "/api/cache/sweep", e2eDW, `{"all":true}`, 403},
		{"cache sweep allowed for claude (no syncer in test)", "POST", "/api/cache/sweep", e2eAll, `{"all":true}`, 503},
		{"unknown path still needs token", "GET", "/api/nope", "", "", 401},
		{"mcp needs token", "POST", "/mcp", "", `{}`, 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := do(t, ts, c.method, c.path, c.token, c.body, nil)
			if resp.StatusCode != c.want {
				t.Fatalf("status %d, want %d (body %s)", resp.StatusCode, c.want, body)
			}
		})
	}

	_, body := do(t, ts, "GET", "/api/health", "", "", nil)
	if !strings.Contains(string(body), `"auth":"enabled"`) {
		t.Errorf("health should report auth enabled: %s", body)
	}
}

func TestAuthDisabledEndToEnd(t *testing.T) {
	ts := newTestServer(t, false)
	if resp, _ := do(t, ts, "GET", "/api/accounts", "", "", nil); resp.StatusCode != 200 {
		t.Fatalf("disabled auth: status %d", resp.StatusCode)
	}
	_, body := do(t, ts, "GET", "/api/health", "", "", nil)
	if !strings.Contains(string(body), `"auth":"disabled"`) {
		t.Errorf("health should report auth disabled: %s", body)
	}
}

// mcpCall runs initialize + one JSON-RPC request over streamable HTTP.
func mcpCall(t *testing.T, ts *httptest.Server, token, method string, params any) map[string]any {
	t.Helper()
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	resp, b := do(t, ts, "POST", "/mcp", token, initBody, map[string]string{"Accept": "application/json, text/event-stream"})
	if resp.StatusCode != 200 {
		t.Fatalf("initialize: %d %s", resp.StatusCode, b)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	p, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": method, "params": params})
	resp, b = do(t, ts, "POST", "/mcp", token, string(p), map[string]string{"Accept": "application/json, text/event-stream", "Mcp-Session-Id": sid})
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d %s", method, resp.StatusCode, b)
	}
	// Response may be SSE-framed; take the JSON after "data:".
	if i := bytes.Index(b, []byte("data:")); i >= 0 {
		b = bytes.TrimSpace(b[i+5:])
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", method, err, b)
	}
	return out
}

func toolNames(res map[string]any) []string {
	var names []string
	for _, t := range res["result"].(map[string]any)["tools"].([]any) {
		names = append(names, t.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	return names
}

func TestMCPScopesEndToEnd(t *testing.T) {
	ts := newTestServer(t, true)

	dw := toolNames(mcpCall(t, ts, e2eDW, "tools/list", map[string]any{}))
	all := toolNames(mcpCall(t, ts, e2eAll, "tools/list", map[string]any{}))
	if len(all) <= len(dw) {
		t.Fatalf("claude sees %d tools, datawatch %d; want strictly more", len(all), len(dw))
	}
	for _, n := range dw {
		if n == "delete_message" || n == "purge_sender" || n == "sync_account" {
			t.Errorf("datawatch token must not list %s", n)
		}
	}
	has := func(list []string, n string) bool {
		for _, x := range list {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has(dw, "send_message") || !has(dw, "list_messages") || !has(all, "purge_sender") {
		t.Errorf("unexpected tool lists: datawatch=%v", dw)
	}

	res := mcpCall(t, ts, e2eDW, "tools/call", map[string]any{"name": "delete_message", "arguments": map[string]any{}})
	r := res["result"].(map[string]any)
	if r["isError"] != true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), "requires scope") {
		t.Fatalf("delete_message with datawatch token should be forbidden: %v", res)
	}
}
