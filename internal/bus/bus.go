// Package bus provides a lightweight in-process event bus. Every subsystem
// (sync, enrichment, rules, webhooks, API, plugins) publishes and subscribes
// through this bus — enabling autonomous agents, federation, and streaming
// without tight coupling between components.
package bus

import (
	"sync"
)

type EventType string

const (
	EventMessageSynced     EventType = "message.synced"
	EventMessageUpdated    EventType = "message.updated"
	EventMessageDeleted    EventType = "message.deleted"
	EventFolderSynced      EventType = "folder.synced"
	EventSyncComplete      EventType = "sync.complete"
	EventSyncError         EventType = "sync.error"
	EventEnrichmentDone    EventType = "enrichment.done"
	EventEnrichmentError   EventType = "enrichment.error"
	EventAnomalyDetected   EventType = "anomaly.detected"
	EventRuleFired         EventType = "rule.fired"
	EventAccountConnected  EventType = "account.connected"
	EventAccountError      EventType = "account.error"
	EventAccountDisconnect EventType = "account.disconnected"
	EventWebhookDelivered  EventType = "webhook.delivered"
	EventWebhookFailed     EventType = "webhook.failed"
)

type Event struct {
	Type    EventType `json:"type"`
	Account string    `json:"account,omitempty"`
	Payload any       `json:"payload,omitempty"`
}

type Handler func(Event)

type Bus struct {
	mu          sync.RWMutex
	subscribers map[EventType][]Handler
	wildcard    []Handler
}

func New() *Bus {
	return &Bus{
		subscribers: make(map[EventType][]Handler),
	}
}

// Subscribe registers a handler for a specific event type.
func (b *Bus) Subscribe(t EventType, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[t] = append(b.subscribers[t], h)
}

// SubscribeAll registers a handler for every event type.
func (b *Bus) SubscribeAll(h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wildcard = append(b.wildcard, h)
}

// Publish dispatches an event to all matching subscribers.
// Handlers are called synchronously in the calling goroutine.
// For fire-and-forget, wrap in a goroutine at the call site.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	handlers := make([]Handler, 0, len(b.subscribers[e.Type])+len(b.wildcard))
	handlers = append(handlers, b.subscribers[e.Type]...)
	handlers = append(handlers, b.wildcard...)
	b.mu.RUnlock()

	for _, h := range handlers {
		h(e)
	}
}

// PublishAsync dispatches an event in a new goroutine.
func (b *Bus) PublishAsync(e Event) {
	go b.Publish(e)
}
