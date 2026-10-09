# Changelog

All notable changes to imap-mcp are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- **`/api/query` JSON query DSL (v0.10.0, D17).** `POST /api/query` answers ad-hoc
  questions over fixed, read-only views of the cache: `messages`, `senders`,
  `anomalies` and `kg`.
  - A query can use allowlisted fields, filters
    (`eq`/`ne`/`lt`/`lte`/`gt`/`gte`/`in`/`nin`/`contains`/`prefix`/null
    checks), `group_by` with
    `count`/`count_distinct`/`min`/`max`/`sum`/`avg`, `order_by`, `limit`
    (at most 1000) and `offset`.
  - It is compiled to parameterized SQL; raw SQL is never accepted.
  - Message bodies are returned only when named.
  - Queries run read-only with a 10-second timeout and need the `admin`
    scope.
  - `GET /api/query` describes the views. See `docs/query.md`.
- **Webhook delivery (v0.10.0, D16).** `POST /api/webhooks {url, events}` registers an
  endpoint and returns its signing secret. The secret is shown only once.
  - **Durable outbox.** Deliveries are stored in `imap.db`
    (`webhook_deliveries`). They are retried with exponential backoff (30 s up
    to 1 h, 12 attempts) across restarts.
  - **At least once.** Each delivery carries a unique `delivery_id` that
    receivers can use to drop duplicates.
  - **Metadata only.** Payloads carry identifiers, counts and flags, never
    subject, sender, addresses, body or error text.
  - **Signed.** Requests are HMAC-SHA256 signed
    (`X-Imap-Mcp-Signature: t=<unix>,v1=<hex>`).
  - **URL rules.** URLs must be https, or http to loopback only. Redirects are
    not followed.
  - **Auto-disable.** After 100 consecutive failed attempts a webhook is
    disabled; its pending deliveries are kept and resume when it is
    re-enabled.
  - **New routes:** `POST /api/webhooks/{id}/enable`, `POST .../test` (a
    `webhook.test` ping) and `GET .../deliveries`. All webhook routes need the
    `admin` scope.
  - **`rule.fired` is now published** (rule id, action, match count) by
    `run_rules`, the REST API and the hourly `run-rules` CLI. The CLI queues
    its events and `serve` delivers them.
  - The outbox prunes delivered rows after 7 days and failed rows after
    30 days.
- **`datawatch.ca_file` (v0.10.0, D15a).** Pins datawatch's self-signed TLS certificate
  (e.g. `~/.datawatch/tls/server/cert.pem`) for every call imap-mcp makes to
  datawatch: secrets, the capacity gate and the LLM proxy. It is trusted in
  addition to the system roots. Verification is never disabled, and a missing
  or invalid file fails startup.
- **`imap-mcp db encrypt` (v0.10.0, D14).** Encrypts an existing plaintext `imap.db`
  and/or `cache.db` in place with the configured keys (`--only state|cache`).
  - It refuses while another process has the file open, so stop the service
    first.
  - It writes an encrypted copy and verifies it: the key opens it, the
    integrity check passes, and every table's row count and content digest
    match. Only then does it replace the plaintext original.
  - It lists other plaintext copies, such as the pre-0.6.0 backup, but never
    deletes them.
  - Re-running is safe: already-encrypted files are skipped.
- **REST platform (v0.10.0, D13).**
  - **Shared service layer.** Every operation is implemented once in
    `internal/service`, and both MCP tools and REST call it. Existing MCP
    output shapes are unchanged.
  - **New REST routes:**
    - Folders: `GET /api/accounts/{a}/folders`.
    - Messages: paged `GET .../messages` (`limit`, `offset`, `order`),
      `GET`/`DELETE .../messages/{uid}` (`?permanent=true`),
      `PUT .../flags` and `POST .../move`.
    - `GET /api/search`, `POST /api/accounts/{a}/sync`,
      `GET /api/accounts/{a}/stats`.
    - Rules CRUD: `GET`, `POST`, `PUT /api/rules/{id}` (full replace),
      `DELETE`, and `POST /api/rules/{id}/test` (a dry run).
    - Folder names containing `/` are passed as `%2F`.
    - Errors map to 400, 404, 422, 502 and 503. The send endpoint keeps its
      existing contract.
  - **Semantic search.** The `semantic_search` tool and
    `POST /api/search/semantic` rank cached, enriched mail by similarity to a
    query or a reference message.
  - **Intelligence reads.** `get_sender_profile`, `get_sender_history`,
    `kg_query` and `get_anomalies`, plus `GET /api/senders[/{address}]`,
    `/api/kg` and `/api/anomalies`, read the cache tables. They return data
    once iteration-3 intelligence fills those tables.
  - **Still pending:** webhooks and `/api/query`. Each waits on its own
    decision.
- **Enrichment load handling (v0.9.0, D11a/D11b).**
  - **Providers per call type.** Embeddings always go straight to Ollama
    (`enrichment.embed.url/model`). Classification uses either `ollama`
    (direct, the default) or `datawatch`, which goes through
    `POST /api/proxy/llm/<datawatch_llm>` for LLM-registry routing and failover.
  - **Two queue lanes.** New mail (UIDs above a synced folder's previous
    high-water mark) is always processed before backfill (first sync, window
    growth).
  - **Load caps.**
    - Each provider has a concurrency cap (`concurrency`, default 2).
    - Backfill has a token-bucket rate limit (`backfill_per_minute`, default 30).
    - Transient provider errors (5xx, 429, network) trigger exponential
      backoff (`backoff_max_seconds`). A message is retried up to
      `max_attempts` times, then marked as an error.
    - Rows left `processing` by a restart are requeued.
  - **Yielding.** Backfill pauses while models other than ours exceed
    `yield.max_foreign_resident_gb` on the embed Ollama (`/api/ps`), or while
    any `yield.datawatch_pools` capacity pool is full or has waiters
    (`GET /api/capacity`). Optional `backfill_window` "quiet hours" restrict
    when backfill runs. New mail is never paused by any of these. Unreachable
    sources never block.
  - **Status and triggers.**
    - `enrichment_status` and `trigger_enrichment` are implemented: MCP tools
      plus `GET /api/enrichment/status` and `POST /api/enrichment/trigger`.
      A trigger bypasses gating and the rate limit, but not backoff or caps.
    - `/api/health` reports queue depth per lane, done in the last hour,
      oldest pending age, pause reason and backoff.
  - The cache schema moves to v3 (queue lane). The cache is rebuilt from IMAP
    on first start.
- **Cache cleaning (v0.8.0).** Cleaning touches the cache only, never the
  mailbox.
  - **After every sync cycle:**
    - Folders dropped from config or gone from the server, and accounts
      removed from config, are pruned from the cache. Pruning runs per account,
      and only after that account's folder list was read, so a disconnected
      account is never wiped.
    - Orphaned vectors and queue rows are removed.
    - The FTS5 index is integrity-checked and rebuilt if it's inconsistent.
    - The WAL is checkpointed. VACUUM runs at most every
      `sync.vacuum_interval_hours` (default 24).
  - **`cache_sweep`** (MCP tool, `admin` scope) and **`POST /api/cache/sweep`**:
    - Filter by account, folder, `older_than_days`, `errors_only` or `all`. At
      least one filter is required.
    - `dry_run` defaults to true and returns per-folder counts.
    - A real sweep deletes the cached copies, resets their sync state, cleans
      orphans and runs VACUUM. Mail still inside the window comes back on the
      next sync.
  - **`sync.keep_flagged`** (default off) keeps `\Flagged` mail cached even
    outside the window.
  - A `cache.cleaned` bus event is published for each pass.
  - **Content-cleaning hook:** an `enrichment.Cleaner` interface, no-op by
    default, shapes the text sent to the embedding and classification models.
    The cached body is never changed. Real cleaners are planned for
    iteration 3.
  - New env overrides: `IMAP_MCP_SYNC_KEEP_FLAGGED` and
    `IMAP_MCP_SYNC_VACUUM_INTERVAL_HOURS`.
- **Mail cache sync (v0.7.0).** The background sync now fills `cache.db`.
  Before, it only stamped `sync_state`. Per account and per configured folder:
  - **Folders:** `sync.folders` takes SPECIAL-USE tokens (`\Sent`, `\Archive`,
    …) or literal names. The default is `INBOX` + `\Sent`; an account's
    `sync.folders` replaces the global list.
  - **Window:** `sync.window_days` (default 30) is a rolling window by IMAP
    INTERNALDATE. Overrides go in `sync.folder_window_days`, `accounts[].sync.window_days`
    or `accounts[].sync.folder_window_days`. Shrinking the window purges the
    cache; growing it backfills.
  - **Change detection:** a UID diff each cycle. New mail is fetched newest-first
    in batches (envelope, flags, size, INTERNALDATE, full body via `BODY.PEEK[]`).
    Expunged, moved or aged-out mail is removed from the cache only. Flags
    refresh via CONDSTORE `CHANGEDSINCE` where available, otherwise by
    re-fetching the window. A UIDVALIDITY change rebuilds the folder.
  - **Read-only:** folders are opened with EXAMINE, so no flag changes on the
    server, not even `\Seen`. The connection lock is released between
    batches, so MCP tools stay responsive during a backfill.
  - **MIME:** decoded with go-message (transfer encodings and charsets). The
    first text/plain and text/html parts are kept; attachments are recorded as
    name, type and size only. Messages over `sync.max_message_mb` (default 25)
    are cached headers-only.
  - **Enrichment:** new messages are queued, de-duplicated by Message-ID
    across folders. Thread IDs come from References / In-Reply-To.
  - **Events:** `message.synced`, `message.updated`, `message.deleted`,
    `folder.synced` and `sync.complete` on the bus and `/api/events`.
  - `sync_account` now returns per-folder results: cached, new, removed and
    flag updates, plus CONDSTORE use and any errors.
  - Sync settings appear in `/api/health`. New env overrides:
    `IMAP_MCP_SYNC_WINDOW_DAYS`, `IMAP_MCP_SYNC_MAX_MESSAGE_MB` and
    `IMAP_MCP_SYNC_INTERVAL_MINUTES`.

  The cache schema is versioned (`PRAGMA user_version`). A cache from 0.6.0 is
  dropped and rebuilt from IMAP on first start.

### Changed
- **datawatch secrets come from the external-service endpoint (v0.10.0, D15).**
  `${secret:name}` now resolves via `GET /api/external/secrets/{name}`
  (datawatch v8.75.0 or later) with imap-mcp's service token, minted by the
  operator with `datawatch secrets mint-service-token imap-mcp`. Secrets must
  be scoped `service:imap-mcp`. The old `/api/agents/secrets/` path is no
  longer used. Errors now say what to fix (401: re-mint the token; 403/404:
  missing or unscoped secret).
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
- **Permanent deletes now remove only the targeted messages (v0.10.0).** Before,
  `delete_message permanent`, `purge_sender permanent` and the move-by-copy
  fallback ran a folder-wide `EXPUNGE`. That also destroyed any other message
  already marked `\Deleted` by a mail client. go-imap's own `Move` fallback
  does the same on servers without UIDPLUS. Now:
  - Servers with UIDPLUS use `UID EXPUNGE` on exactly the target messages.
  - Without UIDPLUS, the operation refuses if any other message is already
    `\Deleted`.
  - All moves go through native MOVE, or COPY plus that same targeted delete.
- **Enrichment ordering (v0.10.0).** Every new-mail item now finishes before
  any backfill item in the same batch starts. Before, the concurrency cap
  could let a backfill item go first.
- Database files and their WAL/SHM sidecars are created and kept at mode 0600
  (v0.7.0). Before, the WAL/SHM files followed the umask.
- Enrichment no longer leaves rows with a NULL body, subject or sender name
  stuck in `pending` forever, which could starve the queue (v0.7.0).
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
