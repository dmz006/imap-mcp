package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/output"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

func contentHandlers(t *testing.T) (*Handlers, string) {
	t.Helper()
	srv := imaptest.Start(t, nil)
	srv.Append(t, "INBOX", "From: alice@example.com\r\nSubject: Files\r\nMessage-ID: <f@example.com>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=XX\r\n\r\n--XX\r\nContent-Type: text/plain\r\n\r\nbody\r\n"+
		"--XX\r\nContent-Type: text/calendar\r\nContent-Disposition: attachment; filename=\"invite.ics\"\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"+
		"--XX\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"../../../.ssh/authorized_keys\"\r\nContent-Transfer-Encoding: base64\r\n\r\n"+
		base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 255})+"\r\n--XX--\r\n", time.Now())
	srv.Append(t, "INBOX", imaptest.Plain("p@example.com", "alice@example.com", "Plain", "hi"), time.Now())
	dir := t.TempDir()
	cfg := &config.Config{
		Accounts: []config.AccountConfig{srv.Account("work")},
		Tools:    config.ToolsConfig{AttachmentInlineKB: 64, AttachmentMaxMB: 25, ExportMaxMessages: 500, ExportMaxMB: 100},
	}
	out, err := output.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandlers(cfg, imaptest.Pool(t, cfg, nil), nil, nil, out), dir
}

func call(t *testing.T, fn func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) (map[string]any, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := fn(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if res.IsError {
		return map[string]any{"error": text}, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("not JSON: %s", text)
	}
	return m, true
}

func TestGetAttachmentsSavesIntoSandbox(t *testing.T) {
	h, dir := contentHandlers(t)

	list, ok := call(t, h.GetAttachments, map[string]any{"folder": "INBOX", "uid": float64(1)})
	if !ok || list["count"].(float64) != 2 {
		t.Fatalf("list = %v", list)
	}

	ics, ok := call(t, h.GetAttachments, map[string]any{"folder": "INBOX", "uid": float64(1), "part": "2"})
	if !ok || ics["saved_to"] != "attachments/work/1-2-invite.ics" || !strings.Contains(ics["text"].(string), "VCALENDAR") {
		t.Fatalf("ics = %v", ics)
	}

	bin, ok := call(t, h.GetAttachments, map[string]any{"folder": "INBOX", "uid": float64(1), "part": "3"})
	if !ok {
		t.Fatalf("bin = %v", bin)
	}
	if _, inline := bin["text"]; inline {
		t.Error("binary attachment returned inline")
	}
	if bin["saved_to"] != "attachments/work/1-3-authorized_keys" {
		t.Errorf("hostile filename not contained: %v", bin["saved_to"])
	}
	data, err := os.ReadFile(filepath.Join(dir, bin["saved_to"].(string)))
	if err != nil || string(data) != "\x00\x01\x02\xff" {
		t.Errorf("saved bytes = %q, %v", data, err)
	}
	if res, ok := call(t, h.GetAttachments, map[string]any{"folder": "INBOX", "uid": float64(1), "part": "2", "filename": "../escape.ics"}); ok {
		t.Errorf("filename outside the sandbox accepted: %v", res)
	}
}

func TestExportMessageWritesFiles(t *testing.T) {
	h, dir := contentHandlers(t)
	eml, ok := call(t, h.ExportMessage, map[string]any{"folder": "INBOX", "uid": float64(2)})
	if !ok || eml["saved_to"] != "exports/work-INBOX-2.eml" || eml["format"] != "eml" {
		t.Fatalf("eml = %v", eml)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "exports/work-INBOX-2.eml")); err != nil || !strings.Contains(string(b), "Message-ID: <p@example.com>") {
		t.Errorf("eml file: %v", err)
	}
	mbox, ok := call(t, h.ExportMessage, map[string]any{"folder": "INBOX", "uids": "1, 2", "filename": "out/all.mbox"})
	if !ok || mbox["count"].(float64) != 2 || mbox["saved_to"] != "out/all.mbox" {
		t.Fatalf("mbox = %v", mbox)
	}
	if res, ok := call(t, h.ExportMessage, map[string]any{"folder": "INBOX", "uids": "1,x"}); ok {
		t.Errorf("bad uids accepted: %v", res)
	}
}

func TestParseUIDs(t *testing.T) {
	got, err := parseUIDs(" 3,4 ,,5")
	if err != nil || len(got) != 3 || got[2] != 5 {
		t.Fatalf("parseUIDs = %v, %v", got, err)
	}
	for _, bad := range []string{"0", "-1", "a", "99999999999"} {
		if _, err := parseUIDs(bad); err == nil {
			t.Errorf("parseUIDs(%q) accepted", bad)
		}
	}
}
