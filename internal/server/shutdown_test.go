package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/imap"
)

// TestShutdownWithOpenEventStream: a connected SSE client (datawatch keeps one
// open permanently) must not stall a requested stop or turn it into a
// non-zero exit.
func TestShutdownWithOpenEventStream(t *testing.T) {
	l, err := net.Listen("tcp", e2eHost+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cfg := &config.Config{Server: config.ServerConfig{Host: e2eHost, Port: port}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn, err := httpauth.New([]httpauth.Token{
		{Name: "datawatch", Value: e2eDW, Scopes: []httpauth.Scope{httpauth.ScopeRead}},
	}, []string{"/api/health"}, log)
	if err != nil {
		t.Fatal(err)
	}
	b := bus.New()
	s := New(cfg, imap.NewPool(cfg, b, log), nil, b, nil, nil, nil, log, authn)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	url := "http://" + net.JoinHostPort(e2eHost, strconv.Itoa(port)) + "/api/events"
	var resp *http.Response
	for i := 0; i < 50; i++ {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+e2eDW)
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v on a requested stop", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("shutdown took %v; the open stream held it", d)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Start did not return")
	}
}
