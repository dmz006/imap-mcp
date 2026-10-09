package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/db"
)

// Delivery policy.
const (
	MaxAttempts      = 12               // per delivery, then status failed
	BaseBackoff      = 30 * time.Second // doubled per attempt
	MaxBackoff       = time.Hour
	DisableAfter     = 100 // consecutive failed attempts before auto-disable
	KeepDelivered    = 7 * 24 * time.Hour
	KeepFailed       = 30 * 24 * time.Hour
	requestTimeout   = 10 * time.Second
	pollInterval     = 2 * time.Second
	batchSize        = 50
	subscriberMaxAge = 10 * time.Second
)

// Backoff returns the delay before retry number attempt (1-based).
func Backoff(attempt int) time.Duration {
	d := BaseBackoff
	for i := 1; i < attempt && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// Enqueuer writes outbox rows for bus events.
type Enqueuer struct {
	repo *db.WebhookRepo
	log  *slog.Logger
	now  func() time.Time
	wake func()

	mu      sync.Mutex
	hooks   []db.Webhook
	fetched time.Time
}

// NewEnqueuer builds an Enqueuer. wake (optional) nudges a local Dispatcher.
func NewEnqueuer(repo *db.WebhookRepo, log *slog.Logger, wake func()) *Enqueuer {
	return &Enqueuer{repo: repo, log: log, now: time.Now, wake: wake}
}

// Attach subscribes the Enqueuer to every event on b.
func (q *Enqueuer) Attach(b *bus.Bus) { b.SubscribeAll(q.Handle) }

// Invalidate drops the cached subscriber list (after a webhook change).
func (q *Enqueuer) Invalidate() {
	q.mu.Lock()
	q.fetched = time.Time{}
	q.mu.Unlock()
}

func (q *Enqueuer) subscribers(ctx context.Context) []db.Webhook {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.now().Sub(q.fetched) < subscriberMaxAge {
		return q.hooks
	}
	hooks, err := q.repo.Active(ctx)
	if err != nil {
		q.log.Warn("webhooks: list subscribers", "err", err)
		return q.hooks
	}
	q.hooks, q.fetched = hooks, q.now()
	return hooks
}

// Handle enqueues e for every active webhook subscribed to it.
func (q *Enqueuer) Handle(e bus.Event) {
	ev := string(e.Type)
	ctx := context.Background()
	n := 0
	for _, w := range q.subscribers(ctx) {
		if ev == EventTest || !Subscribed(w.Events, ev) {
			continue
		}
		if err := q.EnqueueFor(ctx, w.ID, e); err != nil {
			q.log.Warn("webhooks: enqueue", "webhook", w.ID, "event", ev, "err", err)
			continue
		}
		n++
	}
	if n > 0 && q.wake != nil {
		q.wake()
	}
}

// EnqueueFor writes one delivery of e for webhook id.
func (q *Enqueuer) EnqueueFor(ctx context.Context, id int64, e bus.Event) error {
	p := Payload{DeliveryID: newDeliveryID(), Event: string(e.Type), Timestamp: q.now().UTC(), Account: e.Account, Data: Metadata(e)}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return q.repo.Enqueue(ctx, id, p.DeliveryID, p.Event, body)
}

// Dispatcher sends due outbox rows.
type Dispatcher struct {
	repo   *db.WebhookRepo
	bus    *bus.Bus
	log    *slog.Logger
	client *http.Client
	now    func() time.Time
	wakeC  chan struct{}
}

// NewDispatcher builds a Dispatcher. Redirects are never followed.
func NewDispatcher(repo *db.WebhookRepo, b *bus.Bus, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		repo: repo, bus: b, log: log, now: time.Now,
		wakeC: make(chan struct{}, 1),
		client: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Wake asks the dispatcher to look for due deliveries now.
func (d *Dispatcher) Wake() {
	select {
	case d.wakeC <- struct{}{}:
	default:
	}
}

// Run delivers until ctx is done, pruning old rows hourly.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		if d.now().Sub(lastPrune) > time.Hour {
			now := d.now()
			if n, err := d.repo.Prune(ctx, now.Add(-KeepDelivered), now.Add(-KeepFailed)); err != nil {
				d.log.Warn("webhooks: prune", "err", err)
			} else if n > 0 {
				d.log.Info("webhooks: pruned outbox", "rows", n)
			}
			lastPrune = now
		}
		d.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wakeC:
		}
	}
}

// RunOnce sends every currently due delivery (in batches) and returns how
// many were attempted.
func (d *Dispatcher) RunOnce(ctx context.Context) int {
	total := 0
	for ctx.Err() == nil {
		due, err := d.repo.Due(ctx, d.now(), batchSize)
		if err != nil {
			d.log.Warn("webhooks: load due deliveries", "err", err)
			return total
		}
		if len(due) == 0 {
			return total
		}
		hooks := map[int64]*db.Webhook{}
		for _, dl := range due {
			w, ok := hooks[dl.WebhookID]
			if !ok {
				if w, err = d.repo.Get(ctx, dl.WebhookID); err != nil {
					continue
				}
				hooks[dl.WebhookID] = w
			}
			if !w.Active {
				continue // disabled by an earlier failure in this batch
			}
			if disabled := d.deliver(ctx, w, dl); disabled {
				w.Active = false
			}
			total++
		}
		if len(due) < batchSize {
			return total
		}
	}
	return total
}

// deliver makes one attempt and records the outcome. It reports whether the
// webhook was auto-disabled.
func (d *Dispatcher) deliver(ctx context.Context, w *db.Webhook, dl db.Delivery) bool {
	now := d.now()
	status, err := d.post(ctx, w, dl, now)
	if err == nil {
		if e := d.repo.Delivered(ctx, dl, status, d.now()); e != nil {
			d.log.Warn("webhooks: record delivery", "err", e)
		}
		d.bus.PublishAsync(bus.Event{Type: bus.EventWebhookDelivered, Payload: map[string]any{"id": w.ID, "delivery_id": dl.DeliveryID}})
		return false
	}
	var next time.Time
	if dl.Attempts+1 < MaxAttempts {
		next = now.Add(Backoff(dl.Attempts + 1))
	}
	fails, disabled, e := d.repo.Failed(ctx, dl, status, err.Error(), next, now, DisableAfter)
	if e != nil {
		d.log.Warn("webhooks: record failure", "err", e)
	}
	d.log.Warn("webhook delivery failed", "webhook", w.ID, "delivery", dl.DeliveryID, "attempt", dl.Attempts+1, "err", err, "gave_up", next.IsZero())
	if next.IsZero() || disabled {
		d.bus.PublishAsync(bus.Event{Type: bus.EventWebhookFailed, Payload: map[string]any{
			"id": w.ID, "delivery_id": dl.DeliveryID, "consecutive_failures": fails, "disabled": disabled}})
	}
	if disabled {
		d.log.Error("webhook disabled after repeated failures; re-enable with POST /api/webhooks/{id}/enable", "webhook", w.ID, "failures", fails)
	}
	return disabled
}

func (d *Dispatcher) post(ctx context.Context, w *db.Webhook, dl db.Delivery, now time.Time) (int, error) {
	body := []byte(dl.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "imap-mcp-webhook")
	req.Header.Set("X-Imap-Mcp-Event", dl.Event)
	req.Header.Set("X-Imap-Mcp-Delivery", dl.DeliveryID)
	req.Header.Set("X-Imap-Mcp-Signature", Sign(w.Secret, now, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
