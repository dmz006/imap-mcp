# Plans, bugs & backlog

## Active plans

| Date | Plan | Status |
|------|------|--------|
| 2026-10-08 | [Iteration 2 — sync cache](2026-10-08-sync-cache.md) | In progress — P0–P4 built (0.5.3–0.9.0) |

## Bugs

| Date | Bug | Status |
|------|-----|--------|
| 2026-10-08 | SQLite DSN params ignored by `modernc.org/sqlite` → no WAL, no busy timeout, FKs off; `serve` + `run-rules` could hit `SQLITE_BUSY` | Fixed in v0.5.1 |
| 2026-10-08 | Background sync was a scaffold: `syncFolder` fetched nothing, so `messages` stayed empty and enrichment/FTS/semantic search had no data | Fixed in v0.7.0 (P2) |
| 2026-10-08 | Enrichment rows with NULL body/sender name stuck `pending` forever, able to starve the queue | Fixed in v0.7.0 |
| 2026-10-08 | `/api/events` SSE withheld headers until first event/heartbeat and was cut by the 30 s route timeout / 60 s WriteTimeout | Fixed in v0.5.3 |
| 2026-10-08 | Unauthenticated HTTP server reachable from browsers: `text/plain` CSRF to the send endpoint could send mail, and DNS rebinding could reach `/mcp` | Fixed in v0.5.2 (`browserGuard`) and v0.5.3 (scoped token auth, D13a) |

## Backlog

| Item | Notes |
|------|-------|
| PGP inbound gate | Declared but fails closed until implemented. |
| Email community skills (operator request 2026-10-08) | Review `skills/imap-mcp/SKILL.md` and the published `skills/comms/imap-mcp` in datawatch-community each iteration 2 release; update it or add new email skills (e.g. auth/token setup, cache management) as the surface changes. Tracked as part of P6. |
| datawatch capacity gate / LLM proxy credential | Per the datawatch agent (2026-10-09): the imap-mcp service token is accepted **only** by `GET /api/external/secrets/{name}`. `/api/capacity` (yield gate, `autonomous:read`) and `/api/proxy/llm/<name>` (classify provider, `sessions:input`) need a separate federation-peer token (`POST /api/federation/peers` with those capabilities; operator action on the datawatch side). imap-mcp currently sends `datawatch.token` to both, so enabling either feature needs a DIP: a separate config field for the peer token. `sessions:input` also gates session rollback and WS input, so the operator must sign off before it is granted. Both features are off in production. |
