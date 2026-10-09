package mcp

import (
	"context"
	"testing"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestEveryToolHasScope(t *testing.T) {
	s := NewServer(&config.Config{}, nil, nil, nil, nil, nil, true)
	registered := s.ListTools()
	for name := range registered {
		if _, ok := toolScopes[name]; !ok {
			t.Errorf("tool %q has no entry in toolScopes", name)
		}
	}
	for name := range toolScopes {
		if _, ok := registered[name]; !ok {
			t.Errorf("toolScopes has stale entry %q (no such tool)", name)
		}
	}
}

func TestScopeMiddleware(t *testing.T) {
	called := false
	next := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("ok"), nil
	}
	h := scopeMiddleware(next)
	call := func(ctx context.Context, tool string) *mcp.CallToolResult {
		called = false
		req := mcp.CallToolRequest{}
		req.Params.Name = tool
		res, err := h(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	ro := httpauth.WithPrincipal(context.Background(), principal(t, "ro-token", httpauth.ScopeRead))
	if res := call(ro, "list_messages"); res.IsError || !called {
		t.Error("read token should call list_messages")
	}
	if res := call(ro, "delete_message"); !res.IsError || called {
		t.Error("read token must not call delete_message")
	}
	if res := call(ro, "send_message"); !res.IsError || called {
		t.Error("read token must not call send_message")
	}
	if res := call(ro, "no_such_tool"); !res.IsError || called {
		t.Error("unmapped tool must be denied")
	}
	if res := call(context.Background(), "list_accounts"); !res.IsError || called {
		t.Error("missing principal must be denied when enforcing")
	}
}

func TestScopeFilter(t *testing.T) {
	all := []mcp.Tool{{Name: "list_messages"}, {Name: "delete_message"}, {Name: "send_message"}, {Name: "unmapped"}}
	ctx := httpauth.WithPrincipal(context.Background(), principal(t, "x", httpauth.ScopeRead, httpauth.ScopeSend))
	got := scopeFilter(ctx, all)
	if len(got) != 2 || got[0].Name != "list_messages" || got[1].Name != "send_message" {
		t.Fatalf("filtered = %v", got)
	}
	if len(scopeFilter(context.Background(), all)) != 0 {
		t.Error("no principal must see no tools")
	}
}

// principal builds a Principal through the public Authenticator API.
func principal(t *testing.T, value string, scopes ...httpauth.Scope) *httpauth.Principal {
	t.Helper()
	a, err := httpauth.New([]httpauth.Token{{Name: "t", Value: value, Scopes: scopes}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var p *httpauth.Principal
	h := a.Middleware(httpHandlerFunc(func(ctx context.Context) { p = httpauth.FromContext(ctx) }))
	serveWithToken(h, value)
	return p
}

// TestAttachmentDownloadNeedsWrite: listing attachments is a read, but a
// download writes into working_dir, so it needs the write scope (D24).
func TestAttachmentDownloadNeedsWrite(t *testing.T) {
	called := false
	h := scopeMiddleware(func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("ok"), nil
	})
	call := func(ctx context.Context, args map[string]any) bool {
		called = false
		req := mcp.CallToolRequest{}
		req.Params.Name = "get_attachments"
		req.Params.Arguments = args
		res, err := h(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return !res.IsError && called
	}
	ro := httpauth.WithPrincipal(context.Background(), principal(t, "ro-token-attachments", httpauth.ScopeRead))
	rw := httpauth.WithPrincipal(context.Background(), principal(t, "rw-token-attachments", httpauth.ScopeRead, httpauth.ScopeWrite))
	list := map[string]any{"folder": "INBOX", "uid": float64(1)}
	download := map[string]any{"folder": "INBOX", "uid": float64(1), "part": "2"}
	if !call(ro, list) {
		t.Error("read token should list attachments")
	}
	if call(ro, download) {
		t.Error("read token must not download an attachment")
	}
	if !call(rw, download) {
		t.Error("read+write token should download an attachment")
	}
}
