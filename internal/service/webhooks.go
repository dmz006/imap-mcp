package service

import (
	"context"
	"errors"
	"strings"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/webhook"
)

// CreatedWebhook is returned once at registration: the only time the signing
// secret is shown.
type CreatedWebhook struct {
	db.Webhook
	Secret string `json:"secret"`
}

// ListWebhooks returns all webhooks (no secrets).
func (s *Service) ListWebhooks(ctx context.Context) ([]db.Webhook, error) {
	return s.db.Webhooks.List(ctx)
}

// CreateWebhook validates and registers a webhook with a generated secret.
// payload is metadata, full or a comma-separated field list; empty picks
// the D46 default.
func (s *Service) CreateWebhook(ctx context.Context, url string, events []string, payload string) (*CreatedWebhook, error) {
	url = strings.TrimSpace(url)
	if err := webhook.ValidateURL(url); err != nil {
		return nil, invalid("%v", err)
	}
	if err := webhook.ValidateEvents(events); err != nil {
		return nil, invalid("%v", err)
	}
	payload, err := webhook.NormalizePayload(payload, events)
	if err != nil {
		return nil, invalid("%v", err)
	}
	secret := webhook.NewSecret()
	id, err := s.db.Webhooks.Create(ctx, url, events, secret, payload)
	if err != nil {
		return nil, err
	}
	s.invalidateWebhooks()
	w, err := s.db.Webhooks.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	w.Secret = ""
	return &CreatedWebhook{Webhook: *w, Secret: secret}, nil
}

// DeleteWebhook removes a webhook and its outbox rows.
func (s *Service) DeleteWebhook(ctx context.Context, id int64) error {
	if err := s.db.Webhooks.Delete(ctx, id); err != nil {
		return webhookErr(id, err)
	}
	s.invalidateWebhooks()
	return nil
}

// EnableWebhook re-activates a (possibly auto-disabled) webhook and resets its
// failure count. Its pending deliveries resume.
func (s *Service) EnableWebhook(ctx context.Context, id int64) (*db.Webhook, error) {
	if err := s.db.Webhooks.SetActive(ctx, id, true); err != nil {
		return nil, webhookErr(id, err)
	}
	s.invalidateWebhooks()
	return s.getWebhook(ctx, id)
}

// TestWebhook queues a webhook.test ping for one active webhook.
func (s *Service) TestWebhook(ctx context.Context, id int64) (*db.Webhook, error) {
	w, err := s.getWebhook(ctx, id)
	if err != nil {
		return nil, err
	}
	if !w.Active {
		return nil, &Error{Kind: KindUnprocessable, Msg: "webhook is disabled; enable it first"}
	}
	if s.webhooks == nil {
		return nil, unavailable("webhook delivery is not running in this mode")
	}
	if err := s.webhooks.EnqueueFor(ctx, id, bus.Event{Type: webhook.EventTest}); err != nil {
		return nil, err
	}
	return w, nil
}

// WebhookDeliveries returns a webhook's most recent outbox rows.
func (s *Service) WebhookDeliveries(ctx context.Context, id int64, limit int) ([]db.Delivery, error) {
	if _, err := s.getWebhook(ctx, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	return s.db.Webhooks.Deliveries(ctx, id, limit)
}

func (s *Service) getWebhook(ctx context.Context, id int64) (*db.Webhook, error) {
	w, err := s.db.Webhooks.Get(ctx, id)
	if err != nil {
		return nil, webhookErr(id, err)
	}
	w.Secret = ""
	return w, nil
}

func (s *Service) invalidateWebhooks() {
	if s.webhooks != nil {
		s.webhooks.Invalidate()
	}
}

func webhookErr(id int64, err error) error {
	if errors.Is(err, db.ErrWebhookNotFound) {
		return notFound("webhook %d not found", id)
	}
	return err
}
