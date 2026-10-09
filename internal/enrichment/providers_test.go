package enrichment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
)

func TestOllamaProvidersAndErrorClasses(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "busy", status)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		switch r.URL.Path {
		case "/api/embeddings":
			json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1, 2}})
		case "/api/generate":
			if req["stream"] != false {
				t.Error("generate must not stream")
			}
			json.NewEncoder(w).Encode(map[string]any{"response": "ok:" + req["model"].(string)})
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	e := NewOllamaEmbedder(srv.URL+"/", "emb")
	c := NewOllamaClassifier(srv.URL, "cls")
	if v, err := e.Embed(ctx, "x"); err != nil || len(v) != 2 {
		t.Fatalf("embed: %v %v", v, err)
	}
	if out, err := c.Classify(ctx, "p"); err != nil || out != "ok:cls" {
		t.Fatalf("classify: %q %v", out, err)
	}
	for code, transient := range map[int]bool{503: true, 500: true, 429: true, 502: true, 400: false, 404: false} {
		status = code
		_, err := e.Embed(ctx, "x")
		if err == nil || IsTransient(err) != transient {
			t.Errorf("HTTP %d: err=%v transient=%v want %v", code, err, IsTransient(err), transient)
		}
	}
	srv.Close()
	if _, err := e.Embed(ctx, "x"); !IsTransient(err) {
		t.Errorf("connection refused must be transient: %v", err)
	}
}

func TestDatawatchClassifier(t *testing.T) {
	var gotAuth, gotPath, gotPrompt string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		gotPrompt = req["prompt"]
		if fail {
			http.Error(w, "no compute node", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"text": `{"hall":"personal"}`, "used_model": "m", "duration_ms": 5})
	}))
	defer srv.Close()
	c := NewDatawatchClassifier(srv.URL, "fast-small", "tok")
	out, err := c.Classify(context.Background(), "hello")
	if err != nil || out != `{"hall":"personal"}` {
		t.Fatalf("%q %v", out, err)
	}
	if gotAuth != "Bearer tok" || gotPath != "/api/proxy/llm/fast-small" || gotPrompt != "hello" {
		t.Errorf("auth=%q path=%q prompt=%q", gotAuth, gotPath, gotPrompt)
	}
	if c.Name() != "datawatch:fast-small" {
		t.Error(c.Name())
	}
	fail = true
	if _, err := c.Classify(context.Background(), "x"); !IsTransient(err) {
		t.Errorf("502 must be transient: %v", err)
	}
}

func TestGates(t *testing.T) {
	ctx := context.Background()
	at := func(h, m int) time.Time { return time.Date(2026, 10, 8, h, m, 0, 0, time.Local) }
	night := NewWindowGate(22*60, 6*60)
	for _, c := range []struct {
		t  time.Time
		ok bool
	}{{at(23, 0), true}, {at(2, 0), true}, {at(6, 0), false}, {at(12, 0), false}, {at(22, 0), true}} {
		if ok, _ := night.Allow(ctx, c.t); ok != c.ok {
			t.Errorf("wrapping window at %v = %v", c.t.Format("15:04"), ok)
		}
	}
	day := NewWindowGate(9*60, 17*60)
	if ok, _ := day.Allow(ctx, at(8, 59)); ok {
		t.Error("before window")
	}
	if ok, _ := day.Allow(ctx, at(9, 0)); !ok {
		t.Error("window start inclusive")
	}

	ps := `{"models":[{"name":"nomic-embed-text:latest","size_vram":300000000},{"name":"qwen3:1.7b","size_vram":2000000000}]}`
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(ps)) }))
	defer ollama.Close()
	g := NewOllamaGate(ollama.URL, []string{"nomic-embed-text", "qwen3:1.7b"}, 8)
	if ok, why := g.Allow(ctx, time.Now()); !ok {
		t.Errorf("only our models resident: %s", why)
	}
	ps = `{"models":[{"name":"qwen3:1.7b","size_vram":2000000000},{"name":"llama3:70b","size_vram":40000000000}]}`
	if ok, why := g.Allow(ctx, time.Now()); ok || why == "" {
		t.Error("large foreign model must pause backfill")
	}
	if ok, _ := NewOllamaGate("http://127.0.0.1:1", nil, 8).Allow(ctx, time.Now()); !ok {
		t.Error("unreachable ollama must not block")
	}

	capJSON := `{"pools":[{"name":"node:gpu1","limit":1,"external":0,"held":0}],"waiting":[]}`
	var auth string
	dw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(capJSON))
	}))
	defer dw.Close()
	dg := NewDatawatchGate(dw.URL, "t", []string{"node:gpu1"})
	if ok, _ := dg.Allow(ctx, time.Now()); !ok || auth != "Bearer t" {
		t.Errorf("free pool: ok=%v auth=%q", ok, auth)
	}
	capJSON = `{"pools":[{"name":"node:gpu1","limit":1,"external":0,"held":1}],"waiting":[]}`
	if ok, _ := dg.Allow(ctx, time.Now()); ok {
		t.Error("full pool must pause")
	}
	capJSON = `{"pools":[{"name":"node:gpu1","limit":0,"held":3}],"waiting":[{"pools":["node:gpu1"]}]}`
	if ok, _ := dg.Allow(ctx, time.Now()); ok {
		t.Error("waiters must pause")
	}
	capJSON = `{"pools":[{"name":"node:other","limit":1,"held":1}],"waiting":[{"pools":["llm:x"]}]}`
	if ok, _ := dg.Allow(ctx, time.Now()); !ok {
		t.Error("unwatched pools must not pause")
	}
	if ok, _ := NewDatawatchGate("http://127.0.0.1:1", "t", []string{"node:gpu1"}).Allow(ctx, time.Now()); !ok {
		t.Error("unreachable datawatch must not block")
	}

	calls := 0
	cg := cached(gateFunc(func() bool { calls++; return calls == 1 }), time.Minute)
	t0 := time.Now()
	cg.Allow(ctx, t0)
	if ok, _ := cg.Allow(ctx, t0.Add(30*time.Second)); !ok || calls != 1 {
		t.Error("cached verdict not reused within ttl")
	}
	if ok, _ := cg.Allow(ctx, t0.Add(2*time.Minute)); ok || calls != 2 {
		t.Error("cache not refreshed after ttl")
	}
}

type gateFunc func() bool

func (gateFunc) Name() string                                      { return "func" }
func (f gateFunc) Allow(context.Context, time.Time) (bool, string) { return f(), "no" }

func TestRateLimiter(t *testing.T) {
	r := newRateLimiter(60)
	t0 := time.Now()
	if n := r.take(t0, 100); n != 60 {
		t.Fatalf("burst = %d", n)
	}
	if n := r.take(t0, 5); n != 0 {
		t.Fatalf("empty bucket gave %d", n)
	}
	if n := r.take(t0.Add(10*time.Second), 100); n != 10 {
		t.Fatalf("refill after 10s = %d, want 10", n)
	}
	if n := newRateLimiter(0).take(t0, 7); n != 7 {
		t.Fatal("0 = unlimited")
	}
}

func TestDatawatchOverPinnedTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pools":[]}`))
	}))
	defer srv.Close()
	p := &Pipeline{cfg: config.EnrichmentConfig{Yield: config.YieldConfig{Enabled: true, DatawatchPools: []string{"node:gpu1"}}}}
	WithDatawatch(srv.URL, "t", srv.Client().Transport)(p)
	if len(p.gates) != 1 {
		t.Fatalf("gates = %d", len(p.gates))
	}
	// The gate fails open with a reason when it cannot read capacity; an empty
	// reason proves the TLS call succeeded.
	if ok, why := p.gates[0].Allow(context.Background(), time.Now()); !ok || why != "" {
		t.Errorf("gate over pinned TLS: ok=%v reason=%q", ok, why)
	}
}
