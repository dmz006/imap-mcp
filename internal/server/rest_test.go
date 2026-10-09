package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// newRESTServer wires the real handler stack to an in-memory IMAP account.
func newRESTServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := imaptest.Start(t, []string{"Trash", "Archive/2026"}, imaplib.CapMove, imaplib.CapUIDPlus)
	now := time.Now()
	srv.Append(t, "INBOX", imaptest.Plain("a@example.com", "alice@example.com", "Invoice", "pay"), now.Add(-2*time.Hour))
	srv.Append(t, "INBOX", imaptest.Plain("b@example.com", "bob@example.com", "Hi", "hello"), now.Add(-time.Hour))
	cfg := &config.Config{Server: config.ServerConfig{Host: e2eHost}, Accounts: []config.AccountConfig{srv.Account("test")}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := bus.New()
	pool := imaptest.Pool(t, cfg, b)
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	authn, _ := httpauth.New([]httpauth.Token{
		{Name: "claude", Value: e2eAll, Scopes: httpauth.AllScopes},
		{Name: "datawatch", Value: e2eDW, Scopes: []httpauth.Scope{httpauth.ScopeRead, httpauth.ScopeSend}},
	}, []string{"/api/health"}, log)
	s := New(cfg, pool, d, b, nil, nil, nil, log, authn)
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return ts
}

func jsonOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	return m
}

func TestRESTEndpointsEndToEnd(t *testing.T) {
	ts := newRESTServer(t)
	get := func(path, tok string) (int, []byte) {
		resp, b := do(t, ts, "GET", path, tok, "", nil)
		return resp.StatusCode, b
	}
	send := func(method, path, tok, body string) (int, []byte) {
		resp, b := do(t, ts, method, path, tok, body, nil)
		return resp.StatusCode, b
	}

	code, b := get("/api/accounts/test/folders", e2eDW)
	if code != 200 || !strings.Contains(string(b), "Archive/2026") {
		t.Fatalf("folders %d %s", code, b)
	}
	code, b = get("/api/accounts/_default/folders/INBOX/messages?limit=1", e2eDW)
	page := jsonOf(t, b)
	if code != 200 || page["total"].(float64) != 2 || page["count"].(float64) != 1 {
		t.Fatalf("messages %d %s", code, b)
	}
	code, b = get("/api/accounts/test/folders/INBOX/messages/2", e2eDW)
	if code != 200 || !strings.Contains(string(b), `"subject":"Hi"`) {
		t.Fatalf("get message %d %s", code, b)
	}
	if code, _ = get("/api/accounts/test/folders/INBOX/messages/0", e2eDW); code != 400 {
		t.Errorf("uid 0 → %d", code)
	}
	if code, _ = get("/api/accounts/test/folders/INBOX/messages/99", e2eDW); code != 404 {
		t.Errorf("missing uid → %d", code)
	}
	if code, _ = get("/api/accounts/nope/folders", e2eDW); code != 404 {
		t.Errorf("unknown account → %d", code)
	}
	code, b = get("/api/search?from=alice", e2eDW)
	if code != 200 || jsonOf(t, b)["total_matches"].(float64) != 1 {
		t.Fatalf("search %d %s", code, b)
	}
	if code, _ = get("/api/search?since=bad", e2eDW); code != 400 {
		t.Errorf("bad since → %d", code)
	}
	code, b = get("/api/accounts/test/stats", e2eDW)
	if code != 200 || jsonOf(t, b)["connected"] != true {
		t.Fatalf("stats %d %s", code, b)
	}

	// Writes need the write scope.
	if code, _ = send("PUT", "/api/accounts/test/folders/INBOX/messages/1/flags", e2eDW, `{"add":"flagged"}`); code != 403 {
		t.Errorf("flags with read token → %d", code)
	}
	if code, b = send("PUT", "/api/accounts/test/folders/INBOX/messages/1/flags", e2eAll, `{"add":"flagged"}`); code != 200 {
		t.Fatalf("flags %d %s", code, b)
	}
	// Folder with "/" travels as %2F.
	if code, b = send("POST", "/api/accounts/test/folders/INBOX/messages/1/move", e2eAll, `{"destination":"Archive/2026"}`); code != 200 {
		t.Fatalf("move %d %s", code, b)
	}
	code, b = get("/api/accounts/test/folders/Archive%2F2026/messages", e2eDW)
	if code != 200 || jsonOf(t, b)["total"].(float64) != 1 {
		t.Fatalf("encoded folder listing %d %s", code, b)
	}
	code, b = send("DELETE", "/api/accounts/test/folders/INBOX/messages/2", e2eAll, `{}`)
	if code != 200 || jsonOf(t, b)["trash"] != "Trash" {
		t.Fatalf("delete %d %s", code, b)
	}
	if code, _ = send("POST", "/api/accounts/test/folders/INBOX/messages/1/move", e2eAll, `{}`); code != 400 {
		t.Errorf("move without destination → %d", code)
	}

	// Rules CRUD + test.
	code, b = send("POST", "/api/rules", e2eAll, `{"name":"bob","conditions":{"from":"bob"},"actions":[{"type":"seen"}]}`)
	if code != 201 {
		t.Fatalf("create rule %d %s", code, b)
	}
	id := int(jsonOf(t, b)["id"].(float64))
	rulePath := "/api/rules/" + itoa(id)
	if code, _ = send("POST", "/api/rules", e2eDW, `{}`); code != 403 {
		t.Errorf("create rule with read token → %d", code)
	}
	code, b = get("/api/rules", e2eDW)
	if code != 200 || jsonOf(t, b)["count"].(float64) != 1 {
		t.Fatalf("list rules %d %s", code, b)
	}
	code, b = send("PUT", rulePath, e2eAll, `{"name":"bob2","conditions":{"from":"bob","folder":"Trash"},"actions":[{"type":"seen"}],"active":false}`)
	if code != 200 || jsonOf(t, b)["active"] != false || jsonOf(t, b)["name"] != "bob2" {
		t.Fatalf("update rule %d %s", code, b)
	}
	code, b = send("POST", rulePath+"/test", e2eAll, `{}`)
	if code != 200 || jsonOf(t, b)["matched"].(float64) != 1 {
		t.Fatalf("test rule %d %s", code, b)
	}
	if code, _ = send("PUT", "/api/rules/999", e2eAll, `{"name":"x","conditions":{"from":"a"},"actions":[{"type":"seen"}]}`); code != 404 {
		t.Errorf("update unknown → %d", code)
	}
	if code, _ = send("DELETE", rulePath, e2eAll, `{}`); code != 200 {
		t.Errorf("delete rule → %d", code)
	}
	if code, _ = send("DELETE", rulePath, e2eAll, `{}`); code != 404 {
		t.Errorf("delete again → %d", code)
	}

	// Send keeps its contract: 422 for a receive-only account, 400 for missing fields.
	if code, _ = send("POST", "/api/accounts/_default/messages/send", e2eDW, `{"to":"x@example.com","subject":"s","body":"b"}`); code != 422 {
		t.Errorf("send receive-only → %d", code)
	}
	if code, _ = send("POST", "/api/accounts/_default/messages/send", e2eDW, `{"to":"x@example.com"}`); code != 400 {
		t.Errorf("send missing fields → %d", code)
	}
	if code, _ = send("POST", "/api/accounts/test/sync", e2eAll, `{}`); code != 503 {
		t.Errorf("sync without syncer → %d", code)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
