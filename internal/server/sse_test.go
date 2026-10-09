package server

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/imap"
)

// TestEventStreamSurvivesTimeouts guards the datawatch SSE consumer: headers
// must arrive immediately (not at the first heartbeat), and the stream must
// outlive the server WriteTimeout and the per-request route timeout.
func TestEventStreamSurvivesTimeouts(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Host: e2eHost}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn, err := httpauth.New([]httpauth.Token{
		{Name: "datawatch", Value: e2eDW, Scopes: []httpauth.Scope{httpauth.ScopeRead, httpauth.ScopeSend}},
	}, []string{"/api/health"}, log)
	if err != nil {
		t.Fatal(err)
	}
	b := bus.New()
	s := New(cfg, imap.NewPool(cfg, b, log), nil, b, nil, nil, nil, log, authn)
	ts := httptest.NewUnstartedServer(s.handler())
	ts.Config.WriteTimeout = 300 * time.Millisecond
	ts.Start()
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+e2eDW)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("headers took %v; must not wait for first event", d)
	}

	// Publish after the WriteTimeout has passed; the event must still arrive.
	time.AfterFunc(700*time.Millisecond, func() {
		b.Publish(bus.Event{Type: bus.EventType("inbound.command"), Account: "personal"})
	})
	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before event (timeout killed it)")
			}
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, "inbound.command") {
				return
			}
		case <-deadline:
			t.Fatal("no event received")
		}
	}
}
