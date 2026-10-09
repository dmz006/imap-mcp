# Changelog

All notable changes to imap-mcp are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed
- **Storage (v0.6.0).** The SQLite driver is now `github.com/ncruces/go-sqlite3`
  (pure Go, no cgo), replacing `modernc.org/sqlite`. Data now lives in two files:
  - `db.path` (`imap.db`) is the state DB: rules, webhooks and inbound nonces.
    It can't be rebuilt from IMAP, so back it up.
  - `db.cache.path` (default `cache.db`, next to `db.path`) is the mail cache:
    messages, full-text index, vectors, senders, knowledge graph and sync
    state. It's disposable and rebuilt from IMAP.

  **Upgrade:** on first open, a single-file `imap.db` from before 0.6.0 is split
  automatically:
  - It's first backed up with `VACUUM INTO` to
    `imap.db.bak-<timestamp>-pre-0.6.0`, and the backup is verified.
  - Every rule, webhook and nonce is fingerprinted (SHA-256) before and after.
    The cache tables are dropped in one transaction, which rolls back on any
    difference.
  - The split is logged with row counts.
  - It's idempotent and safe if `serve` and `run-rules` start at the same time.

### Added
- **Optional at-rest encryption per DB file (v0.6.0).** Set
  `db.encryption_key` and/or `db.cache.encryption_key`. This is whole-file
  encryption with the adiantum VFS: the full-text index, vectors and WAL are
  all encrypted.
  - Keys are `${secret:name}` or `${ENV}` passphrase references, run through
    Argon2id. A key is never generated.
  - A missing, unresolvable or wrong key refuses to open the file. A key set on
    an existing plaintext file also refuses to open.
  - `run-rules` opens only the state DB and resolves only its key.
  - Encryption state is reported in `/api/health` under `storage`.
  - New env overrides: `IMAP_MCP_DB_CACHE_PATH`, `IMAP_MCP_DB_ENCRYPTION_KEY`
    and `IMAP_MCP_DB_CACHE_ENCRYPTION_KEY`.

### Security
- `/api` and `/mcp` now require named, scoped bearer tokens (v0.5.3).
  `browserGuard` (v0.5.2) only stopped browsers: any local process could still
  send mail or call destructive MCP tools without credentials. New
  `server.auth` config:
  - `tokens: [{name, token, scopes}]`, with scopes `read`, `write`, `send` and
    `admin`. Values are `${secret:name}` or `${ENV}` references, at least 32
    characters.
  - Every REST route and MCP tool declares a required scope. MCP `tools/list`
    only shows tools the token may call; a denied call returns a tool error.
    Unknown routes and unmapped tools fail closed.
  - `serve` refuses to start without a token unless `server.auth.disabled: true`
    is set (insecure opt-out, warned at startup, reported in `/api/health` as
    `"auth"`, env `IMAP_MCP_SERVER_AUTH_DISABLED`). Unresolvable token
    references always stop startup.
  - `/api/health` stays open. Token names are logged; values never are.
  - stdio mode and `run-rules` are unaffected and do not need datawatch to
    resolve tokens.

  **Upgrade:** add tokens to the config and to every client before upgrading.
  Claude Code: `"headers": {"Authorization": "Bearer ..."}` on the `imap-mcp`
  HTTP entry. datawatch's `imap_mcp` backend needs a version that sends a token.
- Browser-originated requests against the local HTTP server are now refused
  (v0.5.2). Before, any web page open on the host could POST a `text/plain`
  body to `/api/accounts/{account}/messages/send` without a CORS preflight and
  make imap-mcp send mail, and DNS rebinding could reach `/mcp` tools. The new
  `browserGuard` middleware (`internal/server/guard.go`):
  - accepts only loopback names or the configured `server.host` in `Host`
  - rejects foreign or `null` `Origin` headers
  - requires `Content-Type: application/json` on unsafe `/api` methods

  Non-browser clients (curl, datawatch, Claude Code) are unaffected.

### Fixed
- Database files and their WAL/SHM sidecars are created and kept at mode 0600
  (v0.6.0). Before, the WAL/SHM files followed the umask.
- `Rules.List` no longer fails on rules with NULL description, priority or
  run count (v0.6.0).
- `GET /api/events` (SSE) now sends its 200 headers immediately, instead of at
  the first event or 15 s heartbeat, and is exempt from the 30 s route timeout
  and the 60 s server `WriteTimeout`, which had cut the stream every 30–60 s
  and forced datawatch's `imap_mcp` backend to reconnect (v0.5.3).
- SQLite connection pragmas were never applied (v0.5.1). The DSN used
  mattn-style `_journal`/`_fk`/`_timeout` params, which `modernc.org/sqlite`
  silently ignores, so the DB ran in rollback-journal mode with no busy timeout
  and foreign keys off. Concurrent writers — e.g. `imap-mcp serve` plus the
  hourly `run-rules` CLI on the same file — could fail immediately with
  `database is locked (SQLITE_BUSY)`. Now uses `_pragma=busy_timeout(5000)`,
  `journal_mode(WAL)` and `foreign_keys(1)`; covered by `internal/db/db_test.go`.

## [0.3.0] - 2026-06-14

Cleanup tooling and automation, built from the friction of a real ~16K-message
inbox cleanup. **42 MCP tools.**

### Added
- `purge_sender` — move ALL mail from a sender to Trash, draining the folder in
  one call; auto-detects the Trash mailbox (`\Trash` special-use / `[Gmail]/Trash`).
- `top_senders` — rank a folder's senders by count (address/domain), scanning the
  whole folder, so bulk/spam clusters surface in one call.
- Rules engine — `create_rule`, `list_rules`, `delete_rule`, `run_rules`. A rule is
  a match (from/subject/text/older_than_days) plus an action (trash/move/flag/seen),
  persisted in the `rules` table; `run_rules` supports `dry_run` to preview counts.
- `label_message` — apply a Gmail label by COPY into the label mailbox (creates it
  if missing).
- `empty_trash` — permanently delete everything in the auto-detected Trash mailbox.
- IMAP keepalive (NOOP every 4 min) with auto-reconnect on dropped connections.

### Changed
- `search_messages` now returns the true `total_matches` (previously capped at the
  page size, which hid real volumes behind "50").
- `/api/accounts` live-probes each connection (NOOP) instead of trusting pool
  membership, so a silently-dropped connection reports as disconnected.
- `create_folder` / `delete_folder` accept `folder` as an alias for `path`.

## [0.2.1] - 2026-06-07

Closes the datawatch comm loop (datawatch#127).

### Added
- `GET /api/events` — SSE event stream; fans out bus events to connected clients
  (15s heartbeats; slow clients dropped, never backpressure the bus). datawatch's
  `imap_mcp` backend consumes verified `inbound.command` events here.
- `POST /api/accounts/{account}/messages/send` — REST send via the account's SMTP
  (`account` may be `_default`); same semantics as the `send_message` MCP tool.

## [0.2.0] - 2026-06-07

datawatch integration across three independent, operator-opt-in layers (no
auto-injection).

### Added
- **Secrets** — `${secret:name}` credential references resolve via the datawatch
  secrets service when a `datawatch:` block is present; otherwise fully standalone
  (`${ENV}`/plain). Agent-scoped token (least privilege); clear startup error if a
  `${secret:}` reference is used without a datawatch block.
- **Skill** — companion usage skill published to the datawatch community registry
  at `skills/comms/imap-mcp`. Instructions only; pull-based; never auto-loaded.
- **Bidirectional comm**
  - Outbound: per-account `smtp:` config + `send_message` tool (header-injection
    guarded). **34 MCP tools.**
  - Inbound command channel: trust boundary with composable, default-deny gates —
    allowlist, DKIM/DMARC (Authentication-Results), HMAC over a fenced command
    envelope, nonce/replay, capability scoping. Emits `inbound.command` (verified)
    / `inbound.rejected` (audited).
  - PGP gate declared but **fails closed** — backlogged.

## [0.1.0]

Initial scaffold: multi-account IMAP connection pool, MCP server (stdio +
Streamable HTTP), REST API shell, SQLite cache (FTS5 + vectors), Ollama-backed
enrichment pipeline, enforced output sandbox, and the first message/folder/search
tools.

[0.3.0]: https://github.com/dmz006/imap-mcp/releases/tag/v0.3.0
[0.2.1]: https://github.com/dmz006/imap-mcp/releases/tag/v0.2.1
[0.2.0]: https://github.com/dmz006/imap-mcp/releases/tag/v0.2.0
