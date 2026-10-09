package webhook

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/db"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestValidateURL(t *testing.T) {
	ok := []string{"https://hooks.example.com/x", "http://127.0.0.1:9000/h", "http://localhost/h", "http://[::1]:8080/"}
	bad := []string{"http://hooks.example.com/x", "ftp://example.com", "https://user:pw@example.com/", "/relative", "http://10.0.0.5/"}
	for _, u := range ok {
		if err := ValidateURL(u); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
	for _, u := range bad {
		if ValidateURL(u) == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestEventsAndSubscriptions(t *testing.T) {
	if ValidateEvents(nil) == nil || ValidateEvents([]string{"webhook.failed"}) == nil || ValidateEvents([]string{"inbound.command"}) == nil {
		t.Error("undeliverable events accepted")
	}
	if err := ValidateEvents([]string{"*"}); err != nil {
		t.Error(err)
	}
	if !Subscribed([]string{"*"}, "message.synced") || Subscribed([]string{"*"}, "webhook.delivered") || Subscribed([]string{"*"}, "inbound.command") {
		t.Error("wildcard must cover only deliverable events")
	}
	if Subscribed([]string{"rule.fired"}, "message.synced") || !Subscribed([]string{"rule.fired"}, "rule.fired") {
		t.Error("explicit subscription")
	}
}

func TestMetadataNeverCarriesContent(t *testing.T) {
	type folderStats struct {
		Account string `json:"account"`
		Folder  string `json:"folder"`
		New     int    `json:"new"`
		Error   string `json:"error,omitempty"`
	}
	cases := []struct {
		e    bus.Event
		want map[string]any
	}{
		{bus.Event{Type: bus.EventMessageSynced, Payload: map[string]any{"folder": "INBOX", "uid": uint32(7), "id": int64(3),
			"subject": "secret subject", "from": "boss@example.com", "body": "secret body"}},
			map[string]any{"folder": "INBOX", "uid": uint32(7), "id": int64(3)}},
		{bus.Event{Type: bus.EventSyncError, Payload: "imap: auth failed for user@example.com"}, nil},
		{bus.Event{Type: bus.EventFolderSynced, Payload: folderStats{Account: "a", Folder: "INBOX", New: 2, Error: "boom user@example.com"}},
			map[string]any{"folder": "INBOX", "new": float64(2), "failed": true}},
	}
	for _, c := range cases {
		got := Metadata(c.e)
		b, _ := json.Marshal(got)
		for _, leak := range []string{"secret", "example.com", "boom", "auth failed"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s: payload leaks %q: %s", c.e.Type, leak, b)
			}
		}
		wb, _ := json.Marshal(c.want)
		if string(b) != string(wb) {
			t.Errorf("%s: metadata = %s, want %s", c.e.Type, b, wb)
		}
	}
}

func TestBackoff(t *testing.T) {
	if Backoff(1) != 30*time.Second || Backoff(2) != time.Minute || Backoff(20) != time.Hour {
		t.Errorf("backoff: %v %v %v", Backoff(1), Backoff(2), Backoff(20))
	}
}

// receiver records requests and verifies signatures.
type receiver struct {
	t      *testing.T
	secret string
	mu     sync.Mutex
	status []int // responses to return in order; last repeats
	got    []Payload
	hdrs   []http.Header
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	sig := r.Header.Get("X-Imap-Mcp-Signature")
	parts := strings.SplitN(sig, ",", 2)
	ts := strings.TrimPrefix(parts[0], "t=")
	var tsv int64
	_ = json.Unmarshal([]byte(ts), &tsv)
	if want := Sign(rc.secret, time.Unix(tsv, 0), body); !hmac.Equal([]byte(want), []byte(sig)) {
		rc.t.Errorf("bad signature %q", sig)
	}
	var p Payload
	_ = json.Unmarshal(body, &p)
	rc.got = append(rc.got, p)
	rc.hdrs = append(rc.hdrs, r.Header.Clone())
	st := rc.status[0]
	if len(rc.status) > 1 {
		rc.status = rc.status[1:]
	}
	if st == http.StatusFound {
		w.Header().Set("Location", "https://elsewhere.example.com/")
	}
	w.WriteHeader(st)
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, statuses ...int) (*db.DB, string, *receiver, *Enqueuer, *Dispatcher, *clock, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "imap.db")
	d, err := db.OpenState(db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	rc := &receiver{t: t, status: statuses}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	rc.secret = NewSecret()
	id, err := d.Webhooks.Create(ctx, srv.URL+"/hook", []string{"message.synced", "rule.fired"}, rc.secret)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	q := NewEnqueuer(d.Webhooks, quiet, nil)
	q.now = clk.now
	disp := NewDispatcher(d.Webhooks, bus.New(), quiet)
	disp.now = clk.now
	return d, path, rc, q, disp, clk, id
}

func TestDeliverSignedMetadata(t *testing.T) {
	d, _, rc, q, disp, _, id := setup(t, 200)
	ctx := context.Background()
	q.Handle(bus.Event{Type: bus.EventMessageSynced, Account: "work", Payload: map[string]any{"folder": "INBOX", "uid": 9, "id": 4, "subject": "secret"}})
	q.Handle(bus.Event{Type: bus.EventSyncComplete, Account: "work"}) // not subscribed
	q.Handle(bus.Event{Type: bus.EventWebhookDelivered})              // never deliverable

	if n := disp.RunOnce(ctx); n != 1 || rc.count() != 1 {
		t.Fatalf("attempted %d, received %d", n, rc.count())
	}
	p, h := rc.got[0], rc.hdrs[0]
	if p.Event != "message.synced" || p.Account != "work" || p.DeliveryID == "" || p.Data["folder"] != "INBOX" || p.Data["subject"] != nil {
		t.Errorf("payload = %+v", p)
	}
	if h.Get("X-Imap-Mcp-Delivery") != p.DeliveryID || h.Get("X-Imap-Mcp-Event") != "message.synced" {
		t.Errorf("headers = %v", h)
	}
	ds, _ := d.Webhooks.Deliveries(ctx, id, 10)
	if len(ds) != 1 || ds[0].Status != "delivered" || ds[0].LastStatus != 200 {
		t.Errorf("outbox = %+v", ds)
	}
	w, _ := d.Webhooks.Get(ctx, id)
	if w.LastFired == 0 || w.FailCount != 0 || w.Pending != 0 {
		t.Errorf("webhook = %+v", w)
	}
}

func TestRetryWithBackoff(t *testing.T) {
	d, _, rc, q, disp, clk, id := setup(t, 500, 200)
	ctx := context.Background()
	q.Handle(bus.Event{Type: bus.EventRuleFired, Payload: map[string]any{"rule_id": 1, "matched": 3}})

	disp.RunOnce(ctx) // 500
	ds, _ := d.Webhooks.Deliveries(ctx, id, 10)
	if ds[0].Status != "pending" || ds[0].Attempts != 1 || ds[0].LastStatus != 500 || ds[0].NextAttempt != clk.t.Add(30*time.Second).Unix() {
		t.Fatalf("after failure: %+v", ds[0])
	}
	if n := disp.RunOnce(ctx); n != 0 {
		t.Fatal("retried before backoff elapsed")
	}
	clk.t = clk.t.Add(31 * time.Second)
	disp.RunOnce(ctx) // 200
	ds, _ = d.Webhooks.Deliveries(ctx, id, 10)
	if ds[0].Status != "delivered" || ds[0].Attempts != 2 || rc.count() != 2 {
		t.Fatalf("after retry: %+v (received %d)", ds[0], rc.count())
	}
	if rc.got[0].DeliveryID != rc.got[1].DeliveryID {
		t.Error("retry must reuse the delivery id (receiver dedup)")
	}
}

func TestOutboxSurvivesRestart(t *testing.T) {
	d, path, rc, q, _, _, _ := setup(t, 200)
	q.Handle(bus.Event{Type: bus.EventRuleFired, Payload: map[string]any{"rule_id": 2}})
	d.Close() // "crash" before any delivery

	d2, err := db.OpenState(db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	disp := NewDispatcher(d2.Webhooks, bus.New(), quiet)
	disp.now = func() time.Time { return time.Unix(1_800_000_100, 0) }
	if disp.RunOnce(context.Background()); rc.count() != 1 {
		t.Fatalf("delivered %d after restart", rc.count())
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	d, _, rc, q, disp, _, id := setup(t, http.StatusFound)
	q.Handle(bus.Event{Type: bus.EventRuleFired})
	disp.RunOnce(context.Background())
	ds, _ := d.Webhooks.Deliveries(context.Background(), id, 10)
	if rc.count() != 1 || ds[0].Status != "pending" || ds[0].LastStatus != http.StatusFound {
		t.Errorf("redirect: received %d, outbox %+v", rc.count(), ds[0])
	}
}

func TestGiveUpAfterMaxAttempts(t *testing.T) {
	d, _, _, q, disp, clk, id := setup(t, 503)
	failed := make(chan bus.Event, 1)
	disp.bus.Subscribe(bus.EventWebhookFailed, func(e bus.Event) { failed <- e })
	q.Handle(bus.Event{Type: bus.EventRuleFired})
	for i := 0; i < MaxAttempts; i++ {
		disp.RunOnce(context.Background())
		clk.t = clk.t.Add(MaxBackoff + time.Second)
	}
	ds, _ := d.Webhooks.Deliveries(context.Background(), id, 10)
	if ds[0].Status != "failed" || ds[0].Attempts != MaxAttempts {
		t.Fatalf("outbox = %+v", ds[0])
	}
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Error("no webhook.failed event")
	}
}

func TestAutoDisableAndEnable(t *testing.T) {
	d, _, _, q, _, _, id := setup(t, 200)
	ctx := context.Background()
	q.Handle(bus.Event{Type: bus.EventRuleFired})
	due, _ := d.Webhooks.Due(ctx, time.Unix(1_900_000_000, 0), 10)
	now := time.Unix(1_800_000_000, 0)
	var disabled bool
	for i := 0; i < 3; i++ {
		_, disabled, _ = d.Webhooks.Failed(ctx, due[0], 500, "HTTP 500", now.Add(time.Minute), now, 3)
	}
	if !disabled {
		t.Fatal("not disabled after 3 consecutive failures")
	}
	if due, _ := d.Webhooks.Due(ctx, time.Unix(1_900_000_000, 0), 10); len(due) != 0 {
		t.Error("disabled webhook still has due deliveries")
	}
	q.Invalidate()
	q.Handle(bus.Event{Type: bus.EventRuleFired}) // dropped while disabled
	if err := d.Webhooks.SetActive(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	due, _ = d.Webhooks.Due(ctx, time.Unix(1_900_000_000, 0), 10)
	w, _ := d.Webhooks.Get(ctx, id)
	if len(due) != 1 || w.FailCount != 0 {
		t.Errorf("after enable: due=%d fail_count=%d", len(due), w.FailCount)
	}
	if err := d.Webhooks.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	var n int
	d.StateSQL().QueryRow(`SELECT count(*) FROM webhook_deliveries`).Scan(&n)
	if n != 0 {
		t.Errorf("delete left %d outbox rows", n)
	}
}

func TestPrune(t *testing.T) {
	d, _, _, q, disp, clk, _ := setup(t, 200)
	ctx := context.Background()
	q.Handle(bus.Event{Type: bus.EventRuleFired})
	disp.RunOnce(ctx)
	q.Handle(bus.Event{Type: bus.EventRuleFired}) // stays pending
	n, err := d.Webhooks.Prune(ctx, clk.t.Add(time.Second), clk.t.Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	due, _ := d.Webhooks.Due(ctx, clk.t.Add(time.Hour), 10)
	if len(due) != 1 {
		t.Error("pending delivery pruned")
	}
}

// TestMetadataAnomaly: anomaly.detected keeps id, type and severity and
// nothing else, even if a payload ever carried the sender.
func TestMetadataAnomaly(t *testing.T) {
	m := Metadata(bus.Event{Type: bus.EventAnomalyDetected, Account: "work",
		Payload: map[string]any{"id": 7, "type": "auth_failure", "severity": "high", "sender": "eve@example.com", "description": "x"}})
	if len(m) != 3 || m["id"] != 7 || m["type"] != "auth_failure" || m["severity"] != "high" {
		t.Fatalf("metadata = %v", m)
	}
}
