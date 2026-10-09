# IMAP-MCP-CONTEXT.md

**Load this file at the start of every session before writing any code.**

```bash
Read IMAP-MCP-CONTEXT.md
```

Then re-read the relevant sections of `AGENT.md` for the task at hand. This file
is a quick reference of facts; the code is the source of truth when they differ.

---

## What is imap-mcp?

A Go binary that connects to one or more IMAP accounts and exposes them through:

1. **MCP**: 44 tools, over stdio or Streamable HTTP at `/mcp`.
2. **REST API** at `/api`: mailbox operations, search, analytics, rules,
   webhooks, a JSON query DSL and an SSE event stream. Every route is implemented.
3. **Local cache and enrichment**: a windowed SQLite cache of recent mail
   (FTS5 table, embeddings) filled by a background sync, and an enrichment
   pipeline that embeds and classifies cached mail through Ollama (or datawatch
   for classification).

---

## Project Identity

| Field | Value |
|-------|-------|
| Module | `github.com/dmz006/imap-mcp` |
| License | MIT |
| Go version | 1.25.10 |
| Current version | 0.12.0 |
| Location | the repo root |
| Status | 44 MCP tools registered (no stubs); sender profiles built by a header scanner; `kg_query`/`get_anomalies` empty until P3–P4; all REST routes implemented; scoped bearer-token auth; two-file storage with optional encryption; windowed sync cache; laned enrichment; rules engine; durable webhooks; query DSL; trust-gated inbound commands |

---

## Binary and subcommands

| Command | What it does |
|---------|--------------|
| `imap-mcp` | MCP over stdio. Takes no flags (see config path below) |
| `imap-mcp serve [--config PATH]` | HTTP server: MCP at `/mcp`, REST at `/api`. Starts the webhook dispatcher |
| `imap-mcp auth-setup [--account NAME] [--config PATH]` | OAuth2 browser flow for an `xoauth2` account (defaults to the default account). Callback on loopback only; verifies a random `state` |
| `imap-mcp run-rules [--dry-run] [--config PATH]` | Apply active rules once and exit. Opens only `imap.db`; queues `rule.fired` webhooks for `serve` to deliver |
| `imap-mcp db encrypt [--only state\|cache] [--config PATH]` | Encrypt existing plaintext DBs in place with the configured keys. Stop the service first |
| `imap-mcp version` / `help` | Print version / usage |

Any other first argument falls through to stdio mode.

**Config path.** `serve`, `auth-setup`, `run-rules` and `db encrypt` take
`--config`. Without it, and always in stdio mode, the path is `config.yaml`
in the current working directory if that file exists, otherwise
`~/.config/imap-mcp/config.yaml`. Stdio mode has no `--config` flag: an
argument such as `imap-mcp --config x.yaml` is treated as an unknown
subcommand and the flag is ignored. Set the MCP client's working directory to
pick a different `config.yaml`.

Do not build into the repo root (`go build -o imap-mcp`) while developing if a
deployed service runs that binary. Use `go build ./...` and `go test ./...`.

---

## Key Architecture Decisions (locked)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| MCP transport | stdio + Streamable HTTP (no MCP SSE transport) | MCP SSE transport is deprecated |
| Config | YAML + `IMAP_MCP_*` env overrides; `${ENV}` and `${secret:name}` refs | Structure in YAML, secrets out of it |
| Multi-account | All accounts connected at once, one default | Cross-account tooling |
| IMAP auth | `plain`, `xoauth2` (Google or Microsoft), `xoauth2_service_account` (Google Workspace) | Gmail, Workspace, Microsoft 365 |
| Service layer | `internal/service` shared by MCP and REST (D13) | No operation implemented twice |
| HTTP auth | Named bearer tokens with scopes `read`, `write`, `send`, `admin`; fails closed (D13a) | Local processes and browsers are untrusted |
| Storage | `imap.db` (state) + `cache.db` (disposable cache), ncruces pure-Go SQLite, optional adiantum encryption per file (D1, D1b) | Cache can be dropped; state cannot |
| Sync | Windowed, read-only (EXAMINE), UID diff, CONDSTORE flags, UIDVALIDITY rebuild (D8-D10) | One code path for Dovecot and Gmail |
| Enrichment | Embeddings via direct Ollama; classify via Ollama or datawatch LLM proxy; new-mail lane before backfill (D11a, D11b) | Never starve new mail; yield the GPU |
| Webhooks | Durable outbox in `imap.db`, metadata-only payloads (D16) | At-least-once, no content leaks |
| Query DSL | JSON over fixed read-only views, bound parameters (D17) | No SQL from clients |
| Internal bus | `internal/bus`, in-process pub/sub | SSE, webhooks and future consumers |

---

## Configuration

Template: `config.example.yaml`. Env template: `.env.example`.

Defaults that matter: server `127.0.0.1:8765`; state DB
`~/.local/share/imap-mcp/imap.db`; cache DB `cache.db` next to it; sync folders
`INBOX` + `\Sent`, 30-day window, every 15 minutes; Ollama at
`http://localhost:11434` with `nomic-embed-text` (embed) and `qwen3:1.7b`
(classify); `tools:` attachment inline 64 KB, attachment max 25 MB, export max
500 messages / 100 MB; `intelligence:` on, scan every 15 min, backfill
600 messages/min, batch 200, model roles 20 per tick.

Selected env overrides: `IMAP_MCP_SERVER_PORT`, `IMAP_MCP_SERVER_HOST`,
`IMAP_MCP_SERVER_AUTH_DISABLED`, `IMAP_MCP_DB_PATH`, `IMAP_MCP_DB_CACHE_PATH`,
`IMAP_MCP_DB_ENCRYPTION_KEY`, `IMAP_MCP_DB_CACHE_ENCRYPTION_KEY`,
`IMAP_MCP_SYNC_*`, `IMAP_MCP_OLLAMA_URL`, `IMAP_MCP_ENRICHMENT_*`,
`IMAP_MCP_LOG_LEVEL`. Full list: `applyEnvOverrides` in `internal/config/config.go`.

Credentials always via references:

```yaml
auth:
  password: ${IMAP_MCP_EXAMPLE_PASSWORD}   # environment variable
# or ${secret:name}, resolved from datawatch when a datawatch: block is set
```

Fail-closed rules: `serve` refuses to start with no tokens unless
`server.auth.disabled: true`; tokens must be at least 32 characters and carry
at least one scope; an unresolved `${...}` token or DB key reference is an
error; a missing or wrong DB key refuses to open the file.

---

## Claude Code integration

The key under `mcpServers` names the server; Claude Code exposes its tools as
`mcp__<key>__<tool>`. Use `imap-mcp` so tools appear as
`mcp__imap-mcp__list_accounts`.

Stdio (no auth; reconnects to IMAP each session):

```json
{ "mcpServers": { "imap-mcp": { "command": "/path/to/imap-mcp/imap-mcp" } } }
```

HTTP (start `imap-mcp serve` first; persistent IMAP connections; needs a token):

```json
{
  "mcpServers": {
    "imap-mcp": {
      "type": "http",
      "url": "http://localhost:8765/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

datawatch preserves non-datawatch entries in `~/.mcp.json` and project
`.mcp.json` files, so the entry survives session spawns. datawatch never
injects imap-mcp; the operator attaches it. See `docs/auth-tokens.md` for
token setup and `docs/datawatch-integration.md` for the datawatch side.

---

## datawatch integration (operator-controlled, never auto-injected)

1. **Secrets**: `${secret:name}` resolves through the datawatch secrets service
   when a `datawatch:` block (`api_url`, `token`) is present; otherwise only
   `${ENV}` and plain values work. `internal/config/secrets.go`.
2. **Skill**: `skills/imap-mcp/SKILL.md` is the source of truth for the
   community skill. Loaded on demand only.
3. **Comm**: imap-mcp is the trust boundary. It sends mail per account over
   SMTP and publishes `inbound.command` only for mail that passed every
   configured trust gate. A datawatch backend consumes `GET /api/events` (SSE)
   and replies with `POST /api/accounts/{account}/messages/send`.
4. **Enrichment** (optional): classification through datawatch's LLM proxy and
   a backfill gate on datawatch capacity pools.

The PGP inbound gate is declared but fails closed until implemented.

---

## Key Files

| File | Purpose |
|------|---------|
| `cmd/imap-mcp/main.go` | Entry point; subcommands; dependency wiring (`buildDeps`); config path lookup |
| `internal/config/config.go` | Config structs, defaults, YAML load, env overrides; `var Version` |
| `internal/config/auth.go` | `ServeAuth`: resolves `server.auth` tokens, fails closed |
| `internal/config/dbkeys.go` | `ResolveDBKeys`: resolves DB encryption keys, fails closed |
| `internal/config/secrets.go` | `${secret:name}` resolver via datawatch |
| `internal/bus/bus.go` | Event bus; all event type constants |
| `internal/imap/pool.go` | Multi-account connection pool; keepalive (NOOP every 4 min); reconnect; live probe |
| `internal/imap/auth/` | Authenticator interface; plain, xoauth2, service account; auth-setup flow |
| `internal/db/db.go` | Opens state + cache DBs (ncruces driver, optional adiantum encryption); repositories |
| `internal/db/schema.go` | `stateSchema` (imap.db) and `cacheSchema` (cache.db) |
| `internal/db/migrate.go` | One-time verified split of a pre-0.6.0 single-file `imap.db` |
| `internal/db/encrypt.go` | `imap-mcp db encrypt` implementation (verify before replace) |
| `internal/db/cache.go` | Cache repositories: sync state, messages, flags, Message-ID de-dup, sweep |
| `internal/db/rules.go`, `webhooks.go`, `nonces.go` | State repositories |
| `internal/sync/syncer.go` | Cache sync engine: window, UID diff, CONDSTORE flags, UIDVALIDITY rebuild, cleaning/vacuum |
| `internal/sync/source.go` | Read-only mailbox `Source`; SPECIAL-USE resolution |
| `internal/sync/mime.go` | MIME decode for the cache: text/html parts, attachment metadata, thread ID |
| `internal/enrichment/pipeline.go` | Enrichment worker: lanes, per-provider caps, backfill rate limit, backoff, stats |
| `internal/enrichment/providers.go` | `Embedder` / `Classifier`: direct Ollama, datawatch LLM proxy |
| `internal/enrichment/gates.go` | Backfill `LoadGate`s: time window, Ollama residency, datawatch capacity |
| `internal/enrichment/cleaner.go` | `Cleaner` hook for content cleaning before models (iteration 3) |
| `internal/service/` | Operations shared by MCP and REST: mail, search, intel, rules, send, stats, webhooks, query |
| `internal/mcp/server.go` | MCP server; registers all 44 tools |
| `internal/mcp/scopes.go` | Tool → scope table; middleware and `tools/list` filter |
| `internal/mcp/tools/definitions.go` | All tool schemas (names, params, descriptions) |
| `internal/mcp/tools/impl_*.go` | Tool handlers; `impl_content.go` holds threads, attachments, export, cross-account search |
| `internal/intel/` | Header scanner (D19, D20, D28): resumable, rate-limited, PEEK-only scan of all folders; sender profiles, hashed per-message index, reply pairing, roles (signals → cached hall tags → classify model via `Pipeline.ClassifyWhenIdle`) |
| `internal/service/intelstats.go` | Scan progress for `/api/health` |
| `internal/service/thread.go`, `attachments.go`, `export.go`, `xsearch.go` | Thread lookup (cache + live fallback), attachment list/fetch, .eml/.mbox export, cross-account search (D23–D27) |
| `internal/api/server.go` | REST router with per-route scopes; SSE `/api/events` |
| `internal/server/server.go` | Combined HTTP server: `browserGuard` → auth → `/mcp` + `/api` |
| `internal/server/guard.go` | `browserGuard`: Host/Origin checks, JSON-only unsafe `/api` methods |
| `internal/httpauth/` | Named, scoped bearer-token auth for `/api` and `/mcp` (D13a) |
| `internal/query/query.go` | `/api/query` DSL compiler over read-only views |
| `internal/webhook/` | Enqueuer (bus → outbox) and Dispatcher (outbox → HTTP, retries) |
| `internal/smtp/smtp.go` | Per-account SMTP sender (STARTTLS / implicit TLS, PLAIN auth, header-injection safe) |
| `internal/trust/` | Inbound trust gates: allowlist, DKIM/DMARC, HMAC, replay; PGP fails closed |
| `internal/inbound/` | Watcher polls inbound-enabled folders every 60 s; Processor publishes `inbound.command` / `inbound.rejected` |
| `internal/output/writer.go` | Enforced output sandbox; the only file writer in the MCP layer |
| `AGENT.md` | Operating rules and decision log |
| `docs/architecture/overview.md` | Architecture overview with diagrams |

---

## MCP Tools Status

44 tools registered in `internal/mcp/server.go`. Scopes from
`internal/mcp/scopes.go` (enforced over HTTP only; stdio has no auth).

| Group | Tools | Scope |
|-------|-------|-------|
| Accounts | `list_accounts`; `sync_account` | read; admin |
| Folders | `list_folders`; `create_folder`, `delete_folder` | read; write |
| Labels / Trash | `label_message`, `label_bulk`, `empty_trash` | write |
| Read | `list_messages`, `get_message`, `get_headers` | read |
| Write | `move_message`, `copy_message`, `delete_message`, `set_flags`, `append_message`, `move_bulk`, `flag_bulk`, `purge_sender` | write |
| Send | `send_message` | send |
| Analytics | `top_senders`, `summarize_folder`, `detect_subscriptions`, `get_sender_history` | read |
| Rules | `list_rules`; `create_rule`, `delete_rule`, `run_rules` | read; write |
| Search | `search_messages`, `semantic_search` | read |
| Enrichment | `enrichment_status`; `trigger_enrichment` | read; admin |
| Cache | `cache_sweep` (cache only, `dry_run` defaults to true) | admin |
| Sandbox files | `read_file`, `list_files`; `write_file`, `delete_file` | read; write |
| Threads / content | `get_thread`, `get_attachments` (list), `cross_account_search` (read); `get_attachments` with `part`, `export_message` (write; into `working_dir`) | D23–D26 |
| Intelligence | `get_sender_profile` (built by `internal/intel`); `kg_query`, `get_anomalies` (empty until P3/P4) | read |

Behaviour notes:

- `search_messages` is plain IMAP SEARCH on one folder (default `INBOX`);
  `hall`/`wing`/`room` are accepted but ignored; no FTS. Returns the true
  `total_matches`.
- `move_bulk` and `flag_bulk` match `query` as a **From header substring**, not
  as IMAP SEARCH syntax; `limit` (default 100) keeps the newest matches.
  `move_bulk` also reads an undeclared `subject` argument.
- `purge_sender` loops until the folder has no matches; Trash is auto-detected
  (`\Trash` special-use, then common names).
- `summarize_folder` returns only `total` and `recent` counts.
- Message summaries carry `message_id` and a `thread_id` derived exactly as
  sync derives it (`sync.ThreadID`: References root, else In-Reply-To, else
  Message-ID); every header fetch also PEEKs the References field.
- `get_thread` reads the cache, then searches live (`\All` mailbox, else INBOX
  + `\Sent` + `\Archive`) when the root isn't cached.
- `get_attachments` lists from live BODYSTRUCTURE; a download (`part`) needs
  write scope (argument-dependent rule in `scopes.go`), is saved into
  `working_dir`, and only `text/*` under `tools.attachment_inline_kb` comes back
  inline.
- `export_message`: `uid` → `.eml`; `uids` / `thread_id` / `from` → mboxrd
  `.mbox`; over `tools.export_max_*` is refused, never truncated.
- `cross_account_search`: cache FTS by default, `live: true` fans out IMAP SEARCH.
- `get_message` returns the raw `BODY[TEXT]` section (not MIME-decoded).
- `semantic_search` and `get_sender_history` read the cache, so they see only
  mail inside the sync window.
- Deferred: IMAP IDLE / `watch_folder`.

---

## Database Schema Summary

Two files since 0.6.0 (D1b), each optionally encrypted (adiantum, Argon2id
key derivation). `imap-mcp db encrypt` converts an existing plaintext file
(service stopped; verified before the original is replaced; D14).

**`imap.db`** (state; cannot be rebuilt from the cache):

```sql
rules              -- automation rules (conditions + actions JSON, run_count)
webhooks           -- registered endpoints (signing secret, active, fail_count)
webhook_deliveries -- durable outbox (metadata-only payloads, retries)
inbound_nonces     -- replay protection for inbound commands (account, nonce)
senders            -- sender profiles: counts each way, dates, reply stats, list/bulk/auto, DKIM/DMARC, role (D19, D20)
intel_messages     -- D28 index: Message-ID hash, date, sender id, direction, In-Reply-To hash (no addresses/content)
intel_scan         -- header-scan progress per account/folder (UIDVALIDITY, last UID, completed_at)
kg_entities        -- KG nodes                   (not populated yet: P3)
kg_relationships   -- KG edges, valid_from/to    (not populated yet: P3)
anomalies          -- anomaly log                (not populated yet: P4)
```

**`cache.db`** (disposable; rebuilt from IMAP; dropped and recreated when its
schema version changes):

```sql
messages          -- cached messages, bodies, flags, hall/wing/room tags, enrichment_status
messages_fts      -- FTS5 over subject, body_text, from_addr, from_name (trigger-maintained)
message_vectors   -- float32 embedding blobs (model, dims)
folders           -- folder metadata
sync_state        -- per account/folder: UIDVALIDITY, highest_modseq, last_uid
enrichment_queue  -- pending/processing/done/error jobs, lane 0 = new, 1 = backfill
```

---

## Enrichment Pipeline

```
Sync caches a message → enrichment_queue (lane new | backfill)
Pipeline: new lane first; per-provider concurrency cap (default 2);
          backfill rate limit (default 30/min) and LoadGates
          (backfill_window, ollama_load, datawatch capacity);
          exponential backoff on transient provider errors (cap 300 s);
          max_attempts (default 3) then status = error
  1. embed (direct Ollama, nomic-embed-text) → message_vectors
  2. classify (Ollama qwen3:1.7b or datawatch LLM proxy) → hall/wing/room
  3. publish enrichment.done (or enrichment.error)
```

New mail is never gated. `trigger_enrichment` bypasses window, yield and rate
limit but not backoff or caps.

---

## Event Bus

`internal/bus` is in-process pub/sub. `/api/events` (SSE) and the webhook
enqueuer subscribe to every event.

| Event | Published by |
|-------|--------------|
| `message.synced`, `message.updated`, `message.deleted` | sync |
| `folder.synced`, `sync.complete`, `sync.error` | sync |
| `cache.cleaned` | sync (cleaning, `cache_sweep`) |
| `enrichment.done`, `enrichment.error` | enrichment pipeline |
| `rule.fired` | rules (`run_rules`, `run-rules`, REST) |
| `account.connected`, `account.error` | IMAP pool |
| `account.disconnected` | declared, not published |
| `anomaly.detected` | declared, not published (no detector yet) |
| `webhook.delivered`, `webhook.failed` | webhook dispatcher |
| `inbound.command`, `inbound.rejected` | inbound processor |

---

## REST API Endpoints

All routes are implemented. All except `/api/health` require a bearer token
with the listed scope (`internal/api/server.go` `Router`). Full reference:
`docs/rest-api.md`.

```
GET    /api/health                                                     open
GET    /api/events                                                     read (SSE)
GET    /api/accounts                                                   read
POST   /api/accounts/{account}/sync                                    admin
GET    /api/accounts/{account}/stats                                   read
GET    /api/accounts/{account}/folders                                 read
GET    /api/accounts/{account}/folders/{folder}/messages               read  (folder %2F-encoded)
GET    /api/accounts/{account}/folders/{folder}/messages/{uid}         read
DELETE /api/accounts/{account}/folders/{folder}/messages/{uid}         write
PUT    /api/accounts/{account}/folders/{folder}/messages/{uid}/flags   write
POST   /api/accounts/{account}/folders/{folder}/messages/{uid}/move    write
POST   /api/accounts/{account}/messages/send                           send
GET    /api/accounts/{account}/folders/{folder}/messages/{uid}/attachments          read
GET    /api/accounts/{account}/folders/{folder}/messages/{uid}/attachments/{part}   write (download)
GET    /api/accounts/{account}/folders/{folder}/messages/{uid}/export.eml           write (download)
POST   /api/export                                                     write (.mbox download)
GET    /api/threads/{thread_id}                                        read
GET    /api/search/cross                                               read
GET    /api/search                                                     read
POST   /api/search/semantic                                            read
GET    /api/senders, /api/senders/{address}                            read
GET    /api/kg, /api/anomalies                                         read
GET    /api/enrichment/status                                          read
POST   /api/enrichment/trigger                                         admin
POST   /api/cache/sweep                                                admin
GET    /api/rules                                                      read
POST   /api/rules, PUT|DELETE /api/rules/{id}, POST /api/rules/{id}/test   write
GET|POST /api/webhooks, DELETE /api/webhooks/{id}                     admin
POST   /api/webhooks/{id}/enable, /api/webhooks/{id}/test             admin
GET    /api/webhooks/{id}/deliveries                                   admin
GET|POST /api/query                                                    admin
```

Unsafe `/api` methods must send `Content-Type: application/json`
(`browserGuard`).

---

## Open items

| Item | Notes |
|------|-------|
| Intelligence | KG extractor (P3) and anomaly detector (P4): nothing writes `kg_*` or `anomalies`; `anomaly.detected` never fires. Content cleaning (`Cleaner`) |
| `search_messages` | Honour `hall`/`wing`/`room`; FTS hybrid over `messages_fts` |
| SMTP OAuth | SMTP send supports PLAIN auth only, so accounts without an SMTP password cannot send |
| PGP inbound gate | Declared, fails closed |
| datawatch peer token | Capacity gate and LLM proxy need a separate datawatch peer token; both stay off until a config field exists |
| IMAP IDLE / `watch_folder` | Iteration 4 |
| Auth rejection log volume | Rate-limit repeated per-request rejection logs |

Plans and backlog: `docs/plans/README.md`.

---

## Operating Rules

See `AGENT.md` for the full rule set.

**Prime rule: the user makes all decisions.**
When any design decision is not covered by an existing rule, stop and run DIP.

**Always load this file before starting work in a new session.**
