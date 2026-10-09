package enrichment

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
)

type fakeEmbed struct {
	mu       gosync.Mutex
	order    []string
	err      error
	inflight atomic.Int32
	maxSeen  atomic.Int32
	delay    time.Duration
}

func (f *fakeEmbed) Name() string { return "fake" }
func (f *fakeEmbed) Embed(_ context.Context, text string) ([]float32, error) {
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.maxSeen.Load()
		if n <= m || f.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, text)
	if f.err != nil {
		return nil, f.err
	}
	return []float32{1}, nil
}

type fakeClass struct{}

func (fakeClass) Name() string                                     { return "fake" }
func (fakeClass) Classify(context.Context, string) (string, error) { return `{"hall":"personal"}`, nil }

type staticGate struct{ ok bool }

func (staticGate) Name() string                                       { return "static" }
func (g *staticGate) Allow(context.Context, time.Time) (bool, string) { return g.ok, "test pause" }

func newLaneHarness(t *testing.T, cfg config.EnrichmentConfig, gate *staticGate) (*Pipeline, *fakeEmbed, *db.DB, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	emb := &fakeEmbed{}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cfg.Enabled = true
	p := NewPipeline(cfg, d, bus.New(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithProviders(emb, fakeClass{}), WithGates(gate), WithClock(func() time.Time { return now }))
	return p, emb, d, &now
}

func queue(t *testing.T, d *db.DB, uid uint32, subject string, backfill bool) {
	t.Helper()
	if _, err := d.Messages.Insert(context.Background(), &db.CachedMessage{Account: "a", Folder: "INBOX", UID: uid,
		FromAddr: "x@example.com", Subject: subject, InternalDate: time.Now(), Backfill: backfill}); err != nil {
		t.Fatal(err)
	}
}

func TestNewMailFirstAndBackfillGating(t *testing.T) {
	gate := &staticGate{ok: false}
	p, emb, d, _ := newLaneHarness(t, config.EnrichmentConfig{BatchSize: 10, Concurrency: 1}, gate)
	queue(t, d, 1, "old1", true)
	queue(t, d, 2, "old2", true)
	queue(t, d, 3, "new1", false)
	ctx := context.Background()

	n, err := p.runOnce(ctx, 0)
	if err != nil || n != 1 || len(emb.order) != 1 || emb.order[0] != "new1" {
		t.Fatalf("gated run: n=%d order=%v err=%v (new mail only)", n, emb.order, err)
	}
	st, _ := p.Stats(ctx)
	if st.Pending["backfill"] != 2 || st.Pending["new"] != 0 || st.BackfillPaused == "" {
		t.Errorf("stats while paused: %+v", st)
	}

	gate.ok = true
	queue(t, d, 4, "new2", false)
	n, _ = p.runOnce(ctx, 0)
	if n != 3 || emb.order[1] != "new2" {
		t.Fatalf("after resume: n=%d order=%v (new2 must precede backfill)", n, emb.order)
	}
	st, _ = p.Stats(ctx)
	if st.Done != 4 || st.BackfillPaused != "" || st.DoneLastHour != 4 {
		t.Errorf("final stats: %+v", st)
	}
}

func TestBackfillRateLimitAndTrigger(t *testing.T) {
	p, emb, d, now := newLaneHarness(t, config.EnrichmentConfig{BatchSize: 10, Concurrency: 2, BackfillPerMinute: 2}, &staticGate{ok: true})
	for i := uint32(1); i <= 6; i++ {
		queue(t, d, i, "b", true)
	}
	ctx := context.Background()
	if n, _ := p.runOnce(ctx, 0); n != 2 {
		t.Fatalf("rate limit: processed %d, want 2", n)
	}
	if n, _ := p.runOnce(ctx, 0); n != 0 {
		t.Fatalf("bucket empty: processed %d", n)
	}
	*now = now.Add(30 * time.Second)
	if n, _ := p.runOnce(ctx, 0); n != 1 {
		t.Fatalf("after 30s refill: processed %d, want 1", n)
	}
	// A manual trigger bypasses the rate limit and gates.
	p.gates = []LoadGate{&staticGate{ok: false}}
	if n, _ := p.runOnce(ctx, 10); n != 3 {
		t.Fatalf("trigger processed %d, want the remaining 3", n)
	}
	if len(emb.order) != 6 {
		t.Fatal(emb.order)
	}
}

func TestBackoffRetryAndMaxAttempts(t *testing.T) {
	p, emb, d, now := newLaneHarness(t, config.EnrichmentConfig{BatchSize: 5, Concurrency: 1, MaxAttempts: 2, BackoffMaxSeconds: 60}, &staticGate{ok: true})
	queue(t, d, 1, "m", false)
	ctx := context.Background()
	emb.err = &ProviderError{Provider: "fake", Status: 503, Transient: true, Err: errors.New("busy")}

	p.runOnce(ctx, 0)
	st, _ := p.Stats(ctx)
	if st.Pending["new"] != 1 || st.BackoffSeconds != 5 || st.ConsecutiveFailures != 1 {
		t.Fatalf("after first failure: %+v", st)
	}
	if n, _ := p.runOnce(ctx, 0); n != 0 || len(emb.order) != 1 {
		t.Fatal("must not call providers during backoff")
	}
	*now = now.Add(6 * time.Second)
	p.runOnce(ctx, 0)
	st, _ = p.Stats(ctx)
	if st.Errors != 1 || st.Pending["new"] != 0 || st.BackoffSeconds != 10 {
		t.Fatalf("after max attempts: %+v", st)
	}
	var status string
	d.SQL().QueryRow(`SELECT enrichment_status FROM messages WHERE uid=1`).Scan(&status)
	if status != "error" {
		t.Errorf("message status = %s", status)
	}

	// Success resets the backoff.
	emb.err = nil
	queue(t, d, 2, "ok", false)
	*now = now.Add(time.Minute)
	if n, _ := p.runOnce(ctx, 0); n != 1 {
		t.Fatal("recovery run failed")
	}
	st, _ = p.Stats(ctx)
	if st.BackoffSeconds != 0 || st.ConsecutiveFailures != 0 {
		t.Errorf("backoff not reset: %+v", st)
	}
}

func TestConcurrencyCap(t *testing.T) {
	p, emb, d, _ := newLaneHarness(t, config.EnrichmentConfig{BatchSize: 8, Concurrency: 2}, &staticGate{ok: true})
	emb.delay = 20 * time.Millisecond
	for i := uint32(1); i <= 8; i++ {
		queue(t, d, i, "m", false)
	}
	if n, _ := p.runOnce(context.Background(), 0); n != 8 {
		t.Fatalf("processed %d", n)
	}
	if m := emb.maxSeen.Load(); m != 2 {
		t.Errorf("max in-flight embeds = %d, want cap 2", m)
	}
}

func TestStartupRequeuesProcessing(t *testing.T) {
	p, _, d, _ := newLaneHarness(t, config.EnrichmentConfig{BatchSize: 5}, &staticGate{ok: true})
	queue(t, d, 1, "m", false)
	d.SQL().Exec(`UPDATE enrichment_queue SET status='processing'`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	st, _ := p.Stats(context.Background())
	if st.Processing != 0 || st.Pending["new"] != 1 {
		t.Errorf("interrupted row not requeued: %+v", st)
	}
	if !p.Trigger(5) || p.Trigger(5) {
		t.Error("Trigger should accept one pending request and refuse a second")
	}
}
