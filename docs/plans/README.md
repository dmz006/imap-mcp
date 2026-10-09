# Plans, bugs & backlog

## Active plans

| Date | Plan | Status |
|------|------|--------|
| 2026-10-08 | [Iteration 2 — sync cache](2026-10-08-sync-cache.md) | Planned — decisions pending |

## Bugs

| Date | Bug | Status |
|------|-----|--------|
| 2026-10-08 | SQLite DSN params ignored by `modernc.org/sqlite` → no WAL, no busy timeout, FKs off; `serve` + `run-rules` could hit `SQLITE_BUSY` | Fixed in v0.5.1 |
| 2026-10-08 | Unauthenticated HTTP server reachable from browsers: `text/plain` CSRF to the send endpoint could send mail, and DNS rebinding could reach `/mcp` | Fixed in v0.5.2 (`browserGuard`); token auth tracked under plan D13 |

## Backlog

| Item | Notes |
|------|-------|
| Background sync is a scaffold (→ [plan](2026-10-08-sync-cache.md)) | `internal/sync/syncer.go` `syncFolder` selects the folder and stamps `sync_state.last_synced` but fetches no messages (`messages` stays empty). The `sync.interval_minutes` loop runs but caches nothing, so enrichment, FTS, and semantic search have no data. Live tools and the rules engine query IMAP directly and are unaffected. |
| PGP inbound gate | Declared but fails closed until implemented. |
| Email community skills (operator request 2026-10-08) | Review `skills/imap-mcp/SKILL.md` and the published `skills/comms/imap-mcp` in datawatch-community each iteration 2 release; update it or add new email skills (e.g. auth/token setup, cache management) as the surface changes. Tracked as part of P6. |
