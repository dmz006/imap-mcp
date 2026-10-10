# REST API reference

`imap-mcp serve` runs one HTTP server. The MCP endpoint is at `/mcp` and the
REST API is under `/api`. The default address is `http://127.0.0.1:8765`.

This page lists every REST route. The query DSL and webhooks have their own
pages: [query.md](query.md) and [webhooks.md](webhooks.md).

## Conventions

### Authentication and scopes

Every route except `GET /api/health` needs `Authorization: Bearer <token>`.
Each token carries one or more scopes. Each route requires exactly one scope:

| Scope | Covers |
|-------|--------|
| `read` | Accounts, folders, messages, threads, attachment lists, search, analytics, rules listing, the event stream |
| `write` | Mailbox changes (delete, flags, move), rule changes, and content downloads (attachments, exports) |
| `send` | Outbound mail |
| `admin` | Sync, enrichment trigger, cache sweep, webhooks, query DSL |

If `server.auth.disabled: true` is set, the server checks no tokens and no
scopes. See [auth-tokens.md](auth-tokens.md).

### Request guard

The server checks every request before it checks authentication:

- **Host:** the `Host` header must be `localhost`, a loopback IP, or the
  configured `server.host`. A wildcard bind (`0.0.0.0`, `::`) allows only
  loopback names. Any other host gets **403** `forbidden: host not allowed`.
- **Origin:** if an `Origin` header is present, it must be `http` or `https`
  and name an allowed host. Otherwise the request gets **403**
  `forbidden: cross-origin request`. An `Origin` of `null` is always refused.
- **Content type:** every `/api/` request with a method other than GET, HEAD
  or OPTIONS must send `Content-Type: application/json`. Otherwise it gets
  **415**. This applies to POST, PUT and DELETE, even routes that read no body
  (for example `DELETE /api/rules/{id}`).

Routes that read a JSON body (marked "Body" below) need a valid JSON value. An
empty body fails with 400, so send `{}` when you have no fields to set.

curl, datawatch and MCP clients send loopback `Host` headers and no `Origin`,
so the guard does not affect them.

### Path parameters

- `{account}` is a configured account name. `_default` means the default
  account.
- `{folder}` is the mailbox name. Encode `/` as `%2F`, for example
  `%5BGmail%5D%2FSent%20Mail` for `[Gmail]/Sent Mail`.
- `{uid}` and `{id}` must be positive integers. Anything else gets 400.

### Errors

| Status | Meaning | Body |
|--------|---------|------|
| 400 | Bad or missing input, or a body that is not valid JSON | text |
| 401 | Missing or invalid bearer token | JSON `{"error": "..."}` and a `WWW-Authenticate: Bearer realm="imap-mcp"` header |
| 403 | Token lacks the route's scope (JSON body), or the guard refused the host or origin (text body) | |
| 404 | Unknown account, folder, message, rule or webhook | text |
| 415 | Unsafe method without `Content-Type: application/json` | text |
| 422 | The config cannot serve the request, for example sending from a receive-only account or testing a disabled webhook | text |
| 502 | The IMAP or SMTP server failed | text |
| 503 | The subsystem is not running in this mode (for example no syncer or no enrichment pipeline) | text |
| 500 | Anything else | text |

Every route except `/api/events` has a 30-second request timeout; the
content downloads (attachment, `export.eml`, `POST /api/export`) have 5 minutes. A request
that runs longer gets **504**, and its context is cancelled. A manual sync of a
large account, for example, stops early. The next scheduled cycle picks up
where it left off.

## Health

### `GET /api/health`

No token needed.

```json
{
  "status": "ok",
  "version": "<version>",
  "accounts": 2,
  "auth": "enabled",
  "sync": {
    "interval_minutes": 15,
    "window_days": 30,
    "max_message_mb": 25,
    "keep_flagged": false,
    "vacuum_interval_hours": 24,
    "folders": ["INBOX", "\\Sent"]
  },
  "enrichment": {
    "enabled": true,
    "concurrency": 2,
    "backfill_per_minute": 30,
    "backfill_window": "",
    "max_attempts": 3,
    "backoff_max_seconds": 300,
    "yield": true,
    "embed_provider": "ollama:nomic-embed-text",
    "classify_provider": "ollama:qwen3:1.7b",
    "pending": {"new": 0, "backfill": 120},
    "done_last_hour": 340,
    "errors": 2,
    "oldest_pending_seconds": {"backfill": 600},
    "backfill_paused": "backfill_window: outside backfill window 22:00-07:00",
    "backoff_seconds": 40
  },
  "intelligence": {"enabled": true, "folders": 14, "folders_complete": 14, "backfill_complete": true,
                   "messages_indexed": 48210, "senders": 3120, "replies_paired": 912,
                   "roles": {"newsletter": 1210, "bot": 640, "personal": 380, "unknown": 890}, "last_scan": "2026-10-09T22:49:45Z"},
  "rules": {"hold_digest": true, "hold_digest_hour": 8, "held": 37, "awaiting_digest": 0, "last_digest": "2026-10-10T14:24:01Z"},
  "tools": {"attachment_inline_kb": 64, "attachment_max_mb": 25, "export_max_messages": 500, "export_max_mb": 100},
  "storage": {"state_encrypted": false, "cache_encrypted": false}
}
```

| Field | Meaning |
|-------|---------|
| `accounts` | Number of accounts in the connection pool |
| `auth` | `enabled` or `disabled` |
| `sync.*` | Global sync settings. `window_days` and `folders` are the global values, not per-account overrides. See [sync-cache.md](sync-cache.md). |
| `enrichment.enabled` … `yield` | Enrichment config |
| `enrichment.embed_provider`, `classify_provider`, `pending`, `done_last_hour`, `errors`, `oldest_pending_seconds` | Live queue state. These appear only when the pipeline is running and its stats query succeeds. |
| `enrichment.backfill_paused` | Present only while a gate pauses backfill. Gives the reason. |
| `enrichment.backoff_seconds` | Present only during a provider backoff |
| `intelligence.*` | Header-scan progress and the role breakdown (counts only). `backfill_complete` is true once every folder has been scanned once. `accounts` gives per-account progress by position (`index`, config order) without names, including `rescan_complete` and `rescan_folders_remaining` for the one-time 0.16 reply-tracking rescan (`reply_history_complete` is true when every account has caught up); `GET /api/intelligence/status` has the names. See [intelligence.md](intelligence.md#watching-progress). |
| `rules.*` | The held-mail digest settings (`rules:` config block) and counts: messages held by `new_sender` rules and not released (last 90 days), how many are waiting for the next digest, and when the last digest was sent. Counts only. See [rules.md](rules.md#the-daily-digest). |
| `tools.*` | Limits for attachment downloads and exports (the `tools:` config block) |
| `storage.*` | Whether each database has an encryption key configured. See [encryption.md](encryption.md). |

For more enrichment detail, use `GET /api/enrichment/status`. See
[enrichment.md](enrichment.md).

## Event stream

### `GET /api/events` (`read`)

This is a Server-Sent Events stream of every internal bus event. It has no
request timeout.

- Response headers: `Content-Type: text/event-stream`,
  `Cache-Control: no-cache`, `X-Accel-Buffering: no`. The server sends the
  status line right away.
- Each event is a single `data:` line holding one JSON object, followed by a
  blank line. There is no `event:` or `id:` field.
- Every 15 seconds the server sends a heartbeat comment, `: heartbeat`.
- Missed events are not buffered and there is no replay. A client that falls
  more than 32 events behind loses events, so the bus never blocks. Reconnect
  when the stream drops.

```
data: {"type":"message.synced","account":"work","payload":{"folder":"INBOX","id":98,"queued":true,"uid":4211}}

: heartbeat

```

The event object:

| Field | Type | Notes |
|-------|------|-------|
| `type` | string | Event type, listed below |
| `account` | string | Omitted when empty |
| `payload` | any | Omitted when empty. The shape depends on the type. |

```bash
curl -N -H "Authorization: Bearer <token>" http://127.0.0.1:8765/api/events
```

#### Event types

| Type | `account` | `payload` |
|------|-----------|-----------|
| `message.synced` | yes | `{folder, uid, id, queued}`. `id` is the cache row id. `queued` is false when a copy with the same Message-ID is already queued for enrichment. |
| `message.updated` | yes | `{folder, uid, flags}` (flags changed on the server) |
| `message.deleted` | yes | `{folder, uid}`. The message left the cache because it was expunged, moved or aged out of the window. The mailbox is not touched. |
| `folder.synced` | yes | Folder stats: `{account, entry, folder, window_days, cached, new, removed, flags_updated, condstore, rebuilt?, error?, at}` |
| `sync.complete` | yes | none |
| `sync.error` | yes | Error text (string) |
| `cache.cleaned` | no | After a sync cycle: `{stale_folders?, orphans: {vectors, queue_entries, fts_rebuilt}, vacuumed, at}`. After a real `cache_sweep`: `{dry_run, folders, total, orphans, note}`. |
| `enrichment.done` | no | The enrichment result: `{MessageID, Hall, Wing, Room, Embedding, Entities, Anomalies}`. The field names are Go names, and `Embedding` is the full vector. |
| `enrichment.error` | no | `{message_id, error}`, sent when a message reaches `max_attempts` |
| `rule.fired` | rule's `account` condition (empty for the default account) | `{rule_id, action, matched}` |
| `anomaly.detected` | yes | `{id, type, severity}`. Fetch the finding with `GET /api/anomalies`. See [intelligence.md](intelligence.md#anomalies). |
| `account.connected` | yes | none |
| `account.error` | yes | Error text (string) |
| `webhook.delivered` | no | `{id, delivery_id}` |
| `webhook.failed` | no | `{id, delivery_id, consecutive_failures, disabled}` |
| `inbound.command` | yes | A verified inbound command. See [datawatch-integration.md](datawatch-integration.md). |
| `inbound.rejected` | yes | The trust-gate result for a rejected command email |

`account.disconnected` is defined but nothing publishes it yet.

Webhooks receive a reduced, metadata-only form of these events. See
[webhooks.md](webhooks.md).

## Accounts

### `GET /api/accounts` (`read`)

```json
[{"name": "work", "default": true, "connected": true}]
```

`connected` comes from a live NOOP probe.

### `POST /api/accounts/{account}/sync` (`admin`)

Runs an immediate cache sync of one account. It returns that account's
last-sync stats for each folder.

```json
{"account": "work", "folders": [{"account": "work", "entry": "INBOX", "folder": "INBOX", "window_days": 30, "cached": 812, "new": 3, "removed": 1, "flags_updated": 2, "condstore": true, "at": "2026-10-09T12:00:00Z"}]}
```

Errors: 404 unknown account, 502 sync failure, 503 no syncer.

### `GET /api/accounts/{account}/stats` (`read`)

Shows cache counts, the last sync results and the enrichment state counts for
one account.

```json
{
  "account": "work",
  "connected": true,
  "cached": [{"account": "work", "folder": "INBOX", "count": 812}],
  "sync": [{"entry": "INBOX", "folder": "INBOX", "...": "..."}],
  "enrichment": {"done": 790, "pending": 20, "duplicate": 2}
}
```

## Folders

### `GET /api/accounts/{account}/folders` (`read`)

```json
[{"path": "INBOX", "delimiter": "/"}, {"path": "[Gmail]/Sent Mail", "delimiter": "/", "attributes": ["\\HasNoChildren", "\\Sent"]}]
```

## Messages

These routes talk to the IMAP server directly. They do not read the cache.

### `GET /api/accounts/{account}/folders/{folder}/messages` (`read`)

| Query | Type | Default | Notes |
|-------|------|---------|-------|
| `limit` | int | 50 | Must be 1–200. Values outside that range use 50. |
| `offset` | int | 0 | |
| `order` | string | newest first | `asc` for oldest first |

```json
{"folder": "INBOX", "total": 812, "offset": 0, "limit": 50, "count": 50,
 "messages": [{"uid": 4211, "seq_num": 812, "subject": "Hello", "from": "Ann <ann@example.com>",
               "to": ["you@example.com"], "date": "2026-10-09T11:58:00Z", "flags": ["\\Seen"], "size_bytes": 5120}]}
```

`date` is the INTERNALDATE. Each summary also has `message_id` and
`thread_id`. `thread_id` is derived the same way as in the cache: the first
References entry, else In-Reply-To, else the message's own Message-ID. Pass it
to [`GET /api/threads/{thread_id}`](#get-apithreadsthread_id-read). An unknown
folder gets 404.

### `GET /api/accounts/{account}/folders/{folder}/messages/{uid}` (`read`)

Returns the message header fields above plus `body_text`, which is the raw
`BODY[TEXT]` section. The fetch uses `BODY.PEEK`, so reading a message does
not set `\Seen`. An unknown UID gets 404.

### `DELETE /api/accounts/{account}/folders/{folder}/messages/{uid}` (`write`)

| Query | Type | Default |
|-------|------|---------|
| `permanent` | bool | false |

By default the server moves the message to `Trash` or `[Gmail]/Trash`. If
neither exists, it marks the message `\Deleted` without expunging it. With
`permanent=true`, it expunges exactly this UID and never touches other
`\Deleted` mail.

```json
{"uid": 4211, "permanent": false, "trash": "[Gmail]/Trash"}
```

### `PUT /api/accounts/{account}/folders/{folder}/messages/{uid}/flags` (`write`)

Body:

| Field | Type | Notes |
|-------|------|-------|
| `add` | string | Comma-separated flags. The leading backslash is optional: `seen,flagged`. |
| `remove` | string | Same format |

You must send at least one of `add` or `remove`. Response:
`{"uid": 4211, "status": "updated"}`.

### `POST /api/accounts/{account}/folders/{folder}/messages/{uid}/move` (`write`)

Body: `{"destination": "Archive"}` (required). The server uses `MOVE` when it
supports it. Otherwise it copies the message and deletes exactly that UID.
Response: `{"uid": 4211, "status": "moved", "destination": "Archive"}`.

### `GET /api/threads/{thread_id}` (`read`)

Returns a conversation, oldest message first. URL-encode the thread ID; angle
brackets are optional. The server reads the cache first: every cached folder,
Sent included. It searches the server live by Message-ID, References and
In-Reply-To when:
- nothing is cached;
- the thread's root message isn't cached (it is older than the sync window);
- or `live=true` is set.

| Query | Type | Default | Notes |
|-------|------|---------|-------|
| `account` | string | every account | |
| `live` | bool | false | Search the server even when the cache looks complete |
| `folders` | string | see notes | Comma-separated folders for the live search |
| `limit` | int | 100 | Must be 1–500 |

The default live scope is the `\All` mailbox if the server has one (Gmail's All
Mail), else INBOX plus the `\Sent` and `\Archive` mailboxes.

```json
{"thread_id": "root@example.com", "count": 3, "live_search": true,
 "messages": [{"account": "work", "folder": "INBOX", "uid": 4100, "message_id": "root@example.com",
               "subject": "Plan", "from": "ann@example.com", "date": "2026-08-01T09:00:00Z", "flags": ["\\Seen"], "source": "live"}]}
```

`source` is `cache` or `live`. A message found in both is listed once. An
account that fails during the live search appears in `errors` and does not
fail the request. `truncated: true` means the thread has more than `limit`
messages.

### `GET /api/accounts/{account}/folders/{folder}/messages/{uid}/attachments` (`read`)

Lists the attachment parts from the message's live BODYSTRUCTURE. No content is
read, and the message is not marked read.

```json
{"uid": 4211, "count": 2, "attachments": [
  {"part": "2", "filename": "invoice.pdf", "mime": "application/pdf", "size_bytes": 48211, "encoding": "base64", "disposition": "attachment"},
  {"part": "3", "filename": "invite.ics", "mime": "text/calendar", "size_bytes": 812, "encoding": "7bit", "disposition": "attachment"}]}
```

An attachment is a part with an attachment disposition, a filename, or a
non-text type (such as an inline image). The message's own text and HTML
bodies are not listed. `size_bytes` is the encoded size on the server.

### `GET /api/accounts/{account}/folders/{folder}/messages/{uid}/attachments/{part}` (`write`)

Downloads one attachment, decoded. The response is always
`application/octet-stream` with `X-Content-Type-Options: nosniff`, whatever
type the message declares. `Content-Disposition` carries a sanitised filename,
and nothing is written on the server.

Errors:
- 400 for a malformed `part`;
- 404 when the part isn't an attachment;
- 422 when it is larger than `tools.attachment_max_mb`.

### `GET /api/accounts/{account}/folders/{folder}/messages/{uid}/export.eml` (`write`)

Downloads the raw message (RFC 822, unchanged) as `<uid>.eml`. The fetch
uses `BODY.PEEK[]`. A message larger than `tools.export_max_mb` gets 422.

## Search

### `GET /api/search` (`read`)

Runs a plain IMAP `UID SEARCH` in one folder.

| Query | Type | Default | Notes |
|-------|------|---------|-------|
| `account` | string | default account | |
| `folder` | string | `INBOX` | |
| `from`, `subject` | string | | Header match |
| `text` | string | | Body match |
| `since`, `before` | string | | `YYYY-MM-DD` |
| `flags` | string | | One of `seen`, `unseen`, `flagged`, `answered` |
| `limit` | int | 50 | Must be 1–500 |

```json
{"folder": "INBOX", "total_matches": 1204, "returned": 50, "messages": [ ... ]}
```

`total_matches` is exact. `messages` holds the newest `limit` matches. Some
servers, Gmail among them, match search terms as whole tokens. See
[rules.md](rules.md#gotcha-whole-token-matching).

### `POST /api/search/semantic` (`read`)

Ranks cached, enriched messages by cosine similarity. Body:

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `query` | string | | Free text, embedded with the enrichment embedder |
| `reference_uid` | int | | Use this cached message's vector instead. Needs `folder`. |
| `account` | string | all accounts | |
| `folder` | string | all folders | Filters results only when `query` is used |
| `limit` | int | 10 | Must be 1–100 |
| `threshold` | float | 0.7 | Must be in (0, 1] |

You must send either `query` or `reference_uid`.

```json
{"hits": [{"account": "work", "folder": "INBOX", "uid": 4211, "subject": "...", "from": "ann@example.com",
           "date": "2026-10-09T11:58:00Z", "hall": "conversation", "score": 0.83}],
 "searched": 790}
```

`searched` counts the messages with vectors in scope. If it is 0, the response
includes a `note`. Errors: 503 if the pipeline is not running (for `query`),
404 if the reference message is not cached and enriched, 502 if embedding
fails.

### `GET /api/search/cross` (`read`)

Searches every account at once and merges the hits newest first.

| Query | Type | Default | Notes |
|-------|------|---------|-------|
| `from` | string | | Sender address or name contains (case-insensitive) |
| `subject` | string | | Subject contains |
| `text` | string | | Words in the subject or body (full-text phrase) |
| `since`, `before` | string | | `YYYY-MM-DD` |
| `limit` | int | 20 | Per account, 1–200 |
| `live` | bool | false | IMAP SEARCH on each account in parallel instead of the cache |
| `folder` | string | `INBOX` | Live search only |

Give at least one criterion. By default the search covers the cache, which
means every cached folder but only mail inside the sync window. The response
includes a `note` saying so. With `live=true` it covers each account's full
history in one folder, and `total_matches` gives each account's exact count.

```json
{"source": "cache", "accounts": 2, "count": 3,
 "hits": [{"account": "home", "folder": "INBOX", "uid": 812, "subject": "Invoice", "from": "billing@example.com",
           "date": "2026-10-09T08:00:00Z", "thread_id": "inv-77@example.com", "source": "cache"}],
 "note": "cache search: only mail inside the sync window; pass live: true for full history"}
```

An account that fails appears in `errors` and does not fail the search. The
`text` value is matched as a literal phrase: full-text query syntax in it is
not interpreted.

## Export

### `POST /api/export` (`write`)

Downloads several messages as one mboxrd file, `export.mbox`. The
`X-Export-Count` response header gives the number of messages. The body takes
exactly one selector:

| Field | Type | Notes |
|-------|------|-------|
| `uids` | int array | With `folder` (required) |
| `thread_id` | string | The whole conversation, as `GET /api/threads/{thread_id}` finds it |
| `from` | string | Every message in `folder` (default `INBOX`) whose From contains this |
| `account` | string | Default account; for `thread_id`, omit to use every account |
| `folder` | string | |

```bash
curl -sS -X POST "$IMAP_MCP/api/export" -H "Authorization: Bearer $WRITE_TOKEN" \
  -H "Content-Type: application/json" -d '{"thread_id":"root@example.com"}' -o thread.mbox
```

A selection over `tools.export_max_messages` messages or `tools.export_max_mb`
in total is refused with 422 before any content is downloaded. It is never
truncated. Messages are fetched with `BODY.PEEK[]`.

A `thread_id` export always searches the server for the thread and uses the
server's locations. The cache can lag a move (by a rule, say) by one sync
interval. Messages that no longer exist are skipped, and the
`X-Export-Missing` header gives their number. A `uids` or `from` export is
strict: a UID that doesn't exist gets 404.

## Intelligence

These routes read `imap.db`. Sender profiles and the knowledge graph come from
the header scanner and cover all history ([intelligence.md](intelligence.md)).
Anomalies come from the same scanner (see [intelligence.md](intelligence.md#anomalies)).

| Route | Scope | Query | Response |
|-------|-------|-------|----------|
| `GET /api/senders` | `read` | `role`, `domain`, `limit` (default 50, max 500) | `{count, senders: [...]}`, most messages first |
| `GET /api/senders/{address}` | `read` | | Profile fields ([intelligence.md](intelligence.md#what-a-profile-holds)) plus `cached_messages`, `scan_complete`, `relationships`, `anomalies`. 404 when there is neither a profile nor cached mail. |
| `GET /api/intelligence/status` | `read` | | The `/api/health` `intelligence` block with account names in `accounts[].account` |
| `GET /api/kg` | `read` | `entity` (exact name, either end), `predicate`, `entity_type`, `limit` (default 50, max 500) | `{count, relationships: [{subject, subject_type, predicate, object, object_type, valid_from, valid_to, confidence, weight, last_seen, current, properties}]}`, strongest (`weight`) first. See [intelligence.md](intelligence.md#knowledge-graph) |
| `GET /api/anomalies` | `read` | `account`, `severity` (`low`, `medium`, `high`), `type`, `sender`, `include_resolved` (bool), `limit` (default 20, max 500) | `{count, anomalies: [{id, account, sender, type, description, severity, detected_at, resolved, resolved_at, folder, uid, message_ref, details}]}`, newest first |
| `POST /api/anomalies/{id}/resolve` | `write` | | The anomaly, now `resolved: true`. 404 for an unknown id. |
| `GET /api/replies/needed` | `read` | `account` (omit for all), `older_than_days` (default 2), `within_days` (default 90; e.g. 3650 for all history), `limit` (default 20, max 200) | `{count, items: [{account, thread_id, counterpart, name, subject, message_ref, folder, uid, last_date, days_waiting}], history_complete}`: conversations waiting on you, longest-waiting first. See [intelligence.md](intelligence.md#reply-tracking) |
| `GET /api/replies/awaiting` | `read` | as above | The same shape: conversations where you wrote last; `counterpart` is your message's first recipient |
| `POST /api/replies/dismiss` | `write` | Body `{"account", "thread_id"}` | `{account, thread_id, dismissed: true}`. The conversation leaves both lists until a newer message arrives in it. 404 for an unknown thread. |

## Enrichment

### `GET /api/enrichment/status` (`read`)

```json
{
  "enabled": true,
  "embed_provider": "ollama:nomic-embed-text",
  "classify_provider": "ollama:qwen3:1.7b",
  "pending": {"new": 0, "backfill": 120},
  "processing": 2,
  "done": 790,
  "errors": 2,
  "duplicates": 14,
  "done_last_hour": 340,
  "oldest_pending_seconds": {"backfill": 600},
  "backfill_paused": "ollama_load: other models resident on ollama (...)",
  "backoff_seconds": 40,
  "consecutive_failures": 3,
  "last_error": "..."
}
```

The last four fields appear only when they are set. 503 if the pipeline is not
running. See [enrichment.md](enrichment.md).

### `POST /api/enrichment/trigger` (`admin`)

Body: `{"limit": 50}`. `limit` defaults to 50. Send `{}` to use the default.

The pipeline processes up to `limit` messages now. A trigger bypasses the
backfill window, the yield gates and the rate limit. It does not bypass
backoff or the concurrency caps.

```json
{"triggered": true, "limit": 50}
```

`triggered` is false when a trigger is already pending. 503 when enrichment is
disabled.

## Cache maintenance

### `POST /api/cache/sweep` (`admin`)

Deletes cached copies only, never mailbox mail. Body:

| Field | Type | Default |
|-------|------|---------|
| `account` | string | |
| `folder` | string | Resolved mailbox name |
| `older_than_days` | number | By INTERNALDATE |
| `errors_only` | bool | false |
| `all` | bool | false |
| `dry_run` | bool | **true** |

You must set at least one filter or `all: true`. Otherwise the request gets
400. See [sync-cache.md](sync-cache.md#on-demand-cache_sweep) for the response
and behaviour.

## Webhooks

All webhook routes need the `admin` scope. [webhooks.md](webhooks.md) covers
payloads, signatures and delivery.

| Route | Body / query | Response |
|-------|--------------|----------|
| `GET /api/webhooks` | | `{count, webhooks: [{id, url, events, active, created_at, last_fired?, fail_count, pending}], events: [deliverable types]}` |
| `POST /api/webhooks` | `{url, events}` | **201** with the webhook and `secret`. This is the only time the secret is shown. 400 for a bad URL or bad events. |
| `DELETE /api/webhooks/{id}` | | `{id, status: "deleted"}` |
| `POST /api/webhooks/{id}/enable` | | The webhook, re-enabled with its failure count reset |
| `POST /api/webhooks/{id}/test` | | **202** `{id, status: "queued", event: "webhook.test"}`. 422 if the webhook is disabled. |
| `GET /api/webhooks/{id}/deliveries` | `limit` (default 50, max 500) | `{count, deliveries: [{webhook_id, delivery_id, event, payload, status, attempts, next_attempt, last_status?, last_error?, created_at, done_at?}]}`, newest first |

## Rules

[rules.md](rules.md) covers the rule model and behaviour.

| Route | Scope | Body | Response |
|-------|-------|------|----------|
| `GET /api/rules` | `read` | | `{count, rules: [rule]}` in priority order |
| `POST /api/rules` | `write` | rule body | **201** `{id, name}` |
| `PUT /api/rules/{id}` | `write` | rule body (full replacement) | The updated rule |
| `DELETE /api/rules/{id}` | `write` | | `{id, status: "deleted"}` |
| `POST /api/rules/{id}/test` | `write` | | Dry run: `{id, name, matched, action, error?}` |

The rule body:

| Field | Type | Default |
|-------|------|---------|
| `name` | string | Required, unique |
| `description` | string | |
| `conditions` | object | `{account, folder, from, subject, text, older_than_days}` |
| `actions` | array | `[{type, dest?, flags?}]` |
| `active` | bool | true |
| `priority` | int | 100 (0 also means 100) |

## Query DSL

| Route | Scope | |
|-------|-------|--|
| `GET /api/query` | `admin` | Lists views, fields, operators and aggregates |
| `POST /api/query` | `admin` | Runs a JSON query. Unknown fields get 400. |

See [query.md](query.md).

## Send

### `POST /api/accounts/{account}/messages/send` (`send`)

Body:

| Field | Type | Notes |
|-------|------|-------|
| `to` | string | Required. Comma-separated. |
| `cc` | string | Optional. Comma-separated. |
| `subject` | string | Required |
| `body` | string | Required. Plain text. |

The message goes out through the account's own `smtp` block.

```json
{"status": "sent", "from": "you@example.com", "account": "work"}
```

Errors: 400 missing field, 404 unknown account, 422 account has no `smtp`
config, 502 SMTP failure.
