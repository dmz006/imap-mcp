# Webhooks

imap-mcp can POST event notifications to your HTTP endpoints. Delivery goes
through a durable outbox in `imap.db`, so events survive restarts and receiver
outages (AGENT.md D16).

## Register

All webhook routes need a token with the `admin` scope.

```bash
curl -sS -X POST https://imap-mcp.example.com/api/webhooks \
  -H "Authorization: Bearer $IMAP_MCP_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"url":"https://hooks.example.com/imap","events":["rule.fired","message.synced"],"payload":"metadata"}'
```

The response includes `secret`. **This is the only time it is shown.** Store it
with your receiver; you need it to verify signatures.

- URLs must be `https://`, or `http://` to loopback (`127.0.0.1`, `::1`,
  `localhost`).
- `events` lists event types, or `["*"]` for every deliverable event. `GET
  /api/webhooks` returns the deliverable list.
- `payload` picks what each delivery carries (see [Payload](#payload)):
  `metadata`, `full`, or a comma-separated list of payload fields. If you
  leave it out, a webhook subscribed to `rule.suggested` or `*` gets `full`;
  any other webhook gets `metadata`.
- Deliverable events: `message.synced`, `message.updated`, `message.deleted`,
  `folder.synced`, `sync.complete`, `sync.error`, `cache.cleaned`,
  `enrichment.done`, `enrichment.error`, `anomaly.detected`, `rule.fired`,
  `hold.digest`, `rule.suggested`, `account.connected`, `account.error`, `account.disconnected`.
  `webhook.*` and `inbound.*` are never delivered.
- You can subscribe to all of these, but `account.disconnected` is never
  published today (defined but not emitted). `anomaly.detected` carries
  `{id, type, severity}`; fetch the finding with `GET /api/anomalies`. See
  [intelligence.md](intelligence.md#anomalies). `hold.digest` carries
  `{held, waiting, suggested}` (counts) with the event's `account`; see
  [rules.md](rules.md#the-daily-digest). `rule.suggested` carries
  `{mode, suggested, created, suggestions: [...]}`: each suggestion's target
  address or domain, action, counts, ratio, INBOX match count and rule; see
  [rules.md](rules.md#learning-from-your-moves).

Other routes:

| Route | Purpose |
|-------|---------|
| `GET /api/webhooks` | List webhooks (never shows secrets) |
| `DELETE /api/webhooks/{id}` | Remove a webhook and its queued deliveries |
| `POST /api/webhooks/{id}/test` | Queue a `webhook.test` ping |
| `POST /api/webhooks/{id}/enable` | Re-enable after an auto-disable |
| `GET /api/webhooks/{id}/deliveries?limit=N` | Recent deliveries and their status |

## Payload

Each webhook has a `payload` setting (AGENT.md D46, which amends D16):

- **`metadata`**: identifiers, counts and flags only. Never subject, sender,
  recipients, body or error text. Fetch details over the REST API with the
  receiver's own scoped token. Webhooks registered before 0.17 use this.
- **`full`**: the whole event payload. For `rule.suggested` that includes
  sender addresses and domains; for `account.error` and `enrichment.error`
  it includes the error text.
- **A field list**, e.g. `"suggested,created"`: only those payload fields.

The `metadata` form looks like this:

```json
{
  "delivery_id": "3f1c0e8a9b7d4c2e8f6a5b4c3d2e1f00",
  "event": "message.synced",
  "timestamp": "2026-10-09T12:00:00Z",
  "account": "work",
  "data": {"folder": "INBOX", "uid": 4211, "id": 98}
}
```

In the `metadata` form, errors (`sync.error`, `account.error`, or a folder
sync that failed) arrive as `"failed": true` without the error text; check
`GET /api/health` or the logs.

Headers:

| Header | Value |
|--------|-------|
| `X-Imap-Mcp-Event` | event type |
| `X-Imap-Mcp-Delivery` | `delivery_id` (same on every retry) |
| `X-Imap-Mcp-Signature` | `t=<unix seconds>,v1=<hex HMAC-SHA256>` |

## Verify the signature

The signature is HMAC-SHA256 over `"<t>." + raw body`, keyed with the
webhook's secret. Compare in constant time and reject old timestamps:

```python
import hashlib, hmac, time

def verify(secret: str, header: str, body: bytes, max_age: int = 300) -> bool:
    parts = dict(p.split("=", 1) for p in header.split(","))
    t = parts["t"]
    if abs(time.time() - int(t)) > max_age:
        return False
    mac = hmac.new(secret.encode(), f"{t}.".encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, parts["v1"])
```

## Delivery guarantees

- **At least once.** A delivery can arrive more than once, for example after a
  crash mid-request. Deduplicate on `delivery_id`. Order is not guaranteed; use
  `timestamp`.
- **Success** is any 2xx response within 10 seconds. Redirects are **not**
  followed and count as failures.
- **Retries** back off exponentially: 30 s, 1 min, 2 min, … capped at 1 hour,
  for up to 12 attempts. After that the delivery is marked `failed` and a
  `webhook.failed` event is published on the event stream.
- **Auto-disable.** After 100 consecutive failed attempts the webhook is
  disabled. Its queued deliveries are kept and resume after
  `POST /api/webhooks/{id}/enable`. New events are not queued while it is
  disabled.
- **Retention.** Delivered rows are pruned after 7 days, failed rows after 30
  days.
- **Hourly rules job.** `run-rules` queues `rule.fired` into the outbox, and the
  running `serve` instance delivers it.
