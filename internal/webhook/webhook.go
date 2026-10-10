// Package webhook delivers bus events to registered HTTP endpoints through a
// durable outbox in imap.db (AGENT.md D16).
//
//   - Enqueuer subscribes to the bus and writes one outbox row per matching
//     active webhook. It runs in every process that produces events (serve and
//     the run-rules CLI), so events from the hourly job are delivered too.
//   - Dispatcher (serve only) sends due rows, retrying with exponential backoff
//     across restarts: at-least-once, deduplicable by delivery id.
//
// Payloads are metadata only: identifiers, counts and flags. Subject, sender,
// addresses, bodies and error text never leave the process this way;
// receivers fetch details over REST with their own scoped token.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
)

// EventTest is the type of the ping sent by POST /api/webhooks/{id}/test.
const EventTest = "webhook.test"

// Events are the event types a webhook may subscribe to. webhook.* (would
// loop) and inbound.* (command channel) are deliberately not deliverable.
var Events = []string{
	string(bus.EventMessageSynced),
	string(bus.EventMessageUpdated),
	string(bus.EventMessageDeleted),
	string(bus.EventFolderSynced),
	string(bus.EventSyncComplete),
	string(bus.EventSyncError),
	string(bus.EventCacheCleaned),
	string(bus.EventEnrichmentDone),
	string(bus.EventEnrichmentError),
	string(bus.EventAnomalyDetected),
	string(bus.EventRuleFired),
	string(bus.EventHoldDigest),
	string(bus.EventAccountConnected),
	string(bus.EventAccountError),
	string(bus.EventAccountDisconnect),
}

// ValidateEvents checks a subscription list; "*" means every deliverable event.
func ValidateEvents(events []string) error {
	if len(events) == 0 {
		return fmt.Errorf("events is required (one or more of %s, or \"*\")", strings.Join(Events, ", "))
	}
	for _, e := range events {
		if e != "*" && !slices.Contains(Events, e) {
			return fmt.Errorf("unknown or undeliverable event %q", e)
		}
	}
	return nil
}

// Subscribed reports whether a subscription list includes event.
func Subscribed(events []string, event string) bool {
	if event == EventTest {
		return true
	}
	return slices.Contains(events, event) || (slices.Contains(events, "*") && slices.Contains(Events, event))
}

// ValidateURL allows https anywhere and http only to loopback.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("url must be an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("url must not contain credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return nil
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("http is only allowed to loopback; use https")
	default:
		return fmt.Errorf("url scheme must be https (or http to loopback)")
	}
}

// NewSecret returns a random signing secret (hex, 256 bits).
func NewSecret() string { return randHex(32) }

func newDeliveryID() string { return randHex(16) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// Sign returns the X-Imap-Mcp-Signature header value for body sent at ts:
// "t=<unix>,v1=<hex HMAC-SHA256(secret, "<unix>." + body)>". Receivers
// recompute it, compare in constant time and reject stale timestamps.
func Sign(secret string, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(t + "."))
	m.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// Payload is the JSON body of every delivery.
type Payload struct {
	DeliveryID string         `json:"delivery_id"`
	Event      string         `json:"event"`
	Timestamp  time.Time      `json:"timestamp"`
	Account    string         `json:"account,omitempty"`
	Data       map[string]any `json:"data,omitempty"`
}

// metadataKeys are the only payload fields copied into a delivery.
var metadataKeys = []string{
	"folder", "uid", "id", "message_id", "rule_id", "action", "matched", "flags",
	"anomaly_id", "kind", "new", "cached", "removed", "flags_updated", "rebuilt",
	"type", "severity", // anomaly.detected (D22): the finding's type and severity, never the sender
	"held", "waiting", // hold.digest (D31): how many held / waiting on the owner, never who or what (the account is on the event)
}

// Metadata reduces an event payload to allowlisted identifier/count fields.
// Any error field becomes "failed": true (the text is not sent).
func Metadata(e bus.Event) map[string]any {
	var m map[string]any
	switch p := e.Payload.(type) {
	case nil, string, error:
		// Plain strings are error texts (sync.error, account.error): dropped.
	case map[string]any:
		m = p
	default:
		b, err := json.Marshal(p)
		if err != nil || json.Unmarshal(b, &m) != nil {
			m = nil
		}
	}
	out := map[string]any{}
	for _, k := range metadataKeys {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
		}
	}
	// Enrichment results marshal with Go field names.
	if v, ok := m["MessageID"]; ok {
		out["message_id"] = v
	}
	if v, ok := m["error"]; ok && v != "" && v != nil {
		out["failed"] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
