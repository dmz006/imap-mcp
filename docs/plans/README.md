# Plans, bugs & backlog

## Active plans

| Date | Plan | Status |
|------|------|--------|
| 2026-10-09 | [Intelligence builders and stub tools](2026-10-09-intelligence-and-stubs.md) | In progress — P1–P3 done (0.11.0–0.13.0); P4 next |
| 2026-10-08 | [Iteration 2 — sync cache](2026-10-08-sync-cache.md) | In progress — P0–P5 built (0.5.3–0.10.0) |

## Bugs

| Date | Bug | Status |
|------|-----|--------|
| 2026-10-08 | SQLite DSN params ignored by `modernc.org/sqlite` → no WAL, no busy timeout, FKs off; `serve` + `run-rules` could hit `SQLITE_BUSY` | Fixed in v0.5.1 |
| 2026-10-08 | Background sync was a scaffold: `syncFolder` fetched nothing, so `messages` stayed empty and enrichment/FTS/semantic search had no data | Fixed in v0.7.0 (P2) |
| 2026-10-08 | Enrichment rows with NULL body/sender name stuck `pending` forever, able to starve the queue | Fixed in v0.7.0 |
| 2026-10-08 | `/api/events` SSE withheld headers until first event/heartbeat and was cut by the 30 s route timeout / 60 s WriteTimeout | Fixed in v0.5.3 |
| 2026-10-09 | `serve` exited 1 (`context deadline exceeded`) on every stop while an SSE/MCP stream was open | Fixed in v0.10.3 |
| 2026-10-09 | Microsoft xoauth2 requested Google's scope (never worked); `auth-setup` state unchecked and callback on all interfaces; Gmail auto-detect panicked on short addresses | Fixed in v0.10.4 |
| 2026-10-09 | datawatch schedule `Update()` changes only the display command, not what the spawn fires (reported by the datawatch agent; datawatch-side) | Open (datawatch) |
| 2026-10-09 | Reads set `\Seen`: `get_message`, `get_headers`, `detect_subscriptions` and the inbound watcher fetched without PEEK (the watcher marked all unread mail in its folder as read) | Fixed in v0.10.4 |
| 2026-10-08 | Unauthenticated HTTP server reachable from browsers: `text/plain` CSRF to the send endpoint could send mail, and DNS rebinding could reach `/mcp` | Fixed in v0.5.2 (`browserGuard`) and v0.5.3 (scoped token auth, D13a) |

## Backlog

| Item | Notes |
|------|-------|
| PGP inbound gate | Declared but fails closed until implemented. |
| Email community skills (operator request 2026-10-08) | Review `skills/imap-mcp/SKILL.md` and the published `skills/comms/imap-mcp` in datawatch-community each iteration 2 release; update it or add new email skills (e.g. auth/token setup, cache management) as the surface changes. Tracked as part of P6. |
| datawatch capacity gate / LLM proxy credential | Per the datawatch agent (2026-10-09): the imap-mcp service token is accepted **only** by `GET /api/external/secrets/{name}`. `/api/capacity` (yield gate, `autonomous:read`) and `/api/proxy/llm/<name>` (classify provider, `sessions:input`) need a separate federation-peer token (`POST /api/federation/peers` with those capabilities; operator action on the datawatch side). imap-mcp currently sends `datawatch.token` to both, so enabling either feature needs a DIP: a separate config field for the peer token. `sessions:input` also gates session rollback and WS input, so the operator must sign off before it is granted. Both features stay off until then. |
| Auth rejection log volume | Stale MCP clients (Claude sessions started before the header helper existed) retry OAuth discovery in a loop: about 60 WARN lines per minute per client. Consider rate-limiting the per-request rejection log (e.g. one summary line per client per minute). |
| Intelligence tables never populated | Nothing writes `senders`, `kg_entities`, `kg_relationships` or `anomalies`, so `get_sender_profile`, `kg_query`, `get_anomalies` and the `/api/query` views `senders`/`anomalies`/`kg` return empty, and `anomaly.detected` never fires. Planned in [2026-10-09-intelligence-and-stubs.md](2026-10-09-intelligence-and-stubs.md) (P2–P4). |
| `search_messages` filters | Plain IMAP SEARCH (INBOX default). `hall`/`wing`/`room` are accepted but ignored, and there is no FTS hybrid despite the docs. |
| SMTP OAuth | `send_message` supports PLAIN auth only; OAuth-only accounts (Gmail/Microsoft 365 with basic auth off) cannot send. Add XOAUTH2 to the SMTP sender, reusing the IMAP token file (Google scope already covers SMTP; Microsoft needs `SMTP.Send`). |
| `account.disconnected` never published | Declared on the bus and in the webhook event list, but the pool only logs disconnects (`internal/imap/pool.go`). Publish it, or drop it from the deliverable list. |
| `move_bulk` / `flag_bulk` descriptions | `query` is described as "IMAP SEARCH criteria or FTS query" but `move_bulk` only matches it against From; it also reads an undeclared `subject` param. Align schema, description and code. |
| `send_message` for OAuth accounts | Always registered; for an `xoauth2` account with no SMTP password it attempts an unauthenticated send. Refuse with a clear error until SMTP OAuth exists. |
| Capacity gate fails open on 401 | With the service token (no `autonomous:read`) the datawatch capacity gate never yields and logs nothing useful. Warn once, or refuse to enable it without a peer token. |
| `datawatch.token` literal | Docs require `${ENV}`; config validation does not enforce it. Decide whether to enforce (DIP). |
| `enrichment.done` SSE payload | Carries the raw result with Go field names and the full embedding vector. Send a small metadata payload (message id, hall/tags, model) instead. |
| Vector model name | `applyResult` stores `enrichment.embed_model` (legacy field) instead of the resolved `enrichment.embed.model` in `message_vectors.model`. |
| Multi-action rules | Actions after `move`/`trash` in the same rule fail (UIDs left the folder); only the first action is reported. Validate (move/trash must be last) or re-resolve. |
| `run-rules` exit status | Exits 0 even when rules error; a broken account is visible only in per-rule output. Non-zero exit on any rule error would let schedulers alert. |
| `DeleteMessage` Trash lookup | Hard-coded `Trash`/`[Gmail]/Trash`; use `ResolveTrash` (SPECIAL-USE first) like rules do. |
| `account` ignored | `enrichment_status` / `trigger_enrichment` accept `account` but ignore it; `list_messages` ignores `sort`; `search_messages` `text` searches body only; `detect_subscriptions` schema says limit default 50, code uses 500. |
| `get_message` content | Returns the raw `BODY[TEXT]` section, not decoded MIME, and no attachment metadata despite the description. `summarize_folder` returns only total/recent. |
| `cache.cleaned` webhook | No fields on the webhook metadata allowlist, so the delivery carries no data. |
| `vacuum_interval_hours` comment | `config.go` says VACUUM runs after a cycle that removed messages; code runs it whenever the interval elapsed. |
| Stdio + webhooks | stdio mode never starts the webhook enqueuer/dispatcher, so `rule.fired` from `run_rules` there is never delivered. |

| `run-rules --json` summary | Machine-readable run summary (rules evaluated, per-rule matched/action/error, duration; no message content) so the scheduled wrapper can post it to datawatch once dmz006/datawatch#204 (result panel) exists. |
| "Asks for payment" anomaly | LLM check for a first-time or unusual sender asking for payment, credentials or gift cards. Deferred from D22 (2026-10-09); builds on the P4 detector. |
