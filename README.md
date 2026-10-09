# imap-mcp

**Current release: v0.10.4.** See [CHANGELOG.md](CHANGELOG.md).

imap-mcp is a single Go binary that connects to one or more IMAP accounts and
exposes them to AI agents and scripts:

- an [MCP](https://modelcontextprotocol.io) server (stdio, or Streamable HTTP at `/mcp`) for Claude Code and other MCP clients;
- a REST API at `/api` with an SSE event stream, webhooks and a JSON query DSL;
- a local SQLite mail cache with background enrichment (embeddings for semantic search, classification);
- a rules engine you can run on demand or on a schedule.

It runs fully standalone. Integration with [datawatch](https://github.com/dmz006/datawatch)
(secrets, LLM routing, messaging backend) is optional.

---

## Features

### MCP tools (44)

Every tool takes an optional `account` parameter (the default account is used
when omitted). Over HTTP with auth enabled, each tool needs the token scope
shown; `tools/list` only shows tools the caller's token can use.

| Area | Tools | Scope |
|------|-------|-------|
| Accounts & sync | `list_accounts` | read |
| | `sync_account` | admin |
| Folders & labels | `list_folders` | read |
| | `create_folder`, `delete_folder`, `label_message`, `label_bulk`, `empty_trash` | write |
| Reading | `list_messages`, `get_message`, `get_headers`, `get_thread`, `get_attachments` (list) | read |
| | `get_attachments` (download into `working_dir`), `export_message` (`.eml` / `.mbox` into `working_dir`) | write |
| Writing | `move_message`, `copy_message`, `delete_message`, `set_flags`, `append_message`, `move_bulk`, `flag_bulk`, `purge_sender` | write |
| Sending | `send_message` | send |
| Search | `search_messages`, `semantic_search`, `cross_account_search` | read |
| Analytics | `summarize_folder`, `detect_subscriptions`, `top_senders`, `get_sender_history` | read |
| Intelligence | `get_sender_profile`, `kg_query`, `get_anomalies`, `enrichment_status` | read |
| | `trigger_enrichment` | admin |
| Cache | `cache_sweep` | admin |
| Rules | `list_rules` | read |
| | `create_rule`, `delete_rule`, `run_rules` | write |
| File sandbox (`working_dir`) | `read_file`, `list_files` | read |
| | `write_file`, `delete_file` | write |

Notes:

- `get_thread` answers from the cache and searches the server when the thread
  reaches outside the sync window. `thread_id` comes from any message summary.
- `cross_account_search` searches the cache across every account by default;
  `live: true` runs IMAP SEARCH on each account in parallel.
- Attachment downloads and exports are capped by the `tools:` config block.
- `search_messages` is plain IMAP SEARCH (INBOX by default). Its `hall`/`wing`/`room`
  parameters are accepted but ignored. For meaning-based search use `semantic_search`.
- `get_sender_profile` returns a profile built from a header-only scan of all
  your history: contact dates, counts each way, reply times, DKIM/DMARC results
  and a role. See [docs/intelligence.md](docs/intelligence.md).
- `kg_query` and `get_anomalies` work but return empty results: nothing builds
  the knowledge graph or detects anomalies yet.

See [docs/known-limitations.md](docs/known-limitations.md) for the full list.

### Beyond MCP

| Feature | Summary | Docs |
|---------|---------|------|
| Bearer-token auth | Named tokens with `read`, `write`, `send`, `admin` scopes guard `/api` and `/mcp`. `serve` refuses to start without a token unless auth is explicitly disabled. `/api/health` stays open. | [docs/auth-tokens.md](docs/auth-tokens.md) |
| REST API | Accounts, folders, messages, threads, attachments, export, search (incl. cross-account), analytics, rules, webhooks, cache sweep, enrichment, send. SSE event stream at `GET /api/events`. | [docs/rest-api.md](docs/rest-api.md) |
| Webhooks | POST bus events to your endpoints through a durable outbox (`webhook_deliveries`) that survives restarts and receiver outages. | [docs/webhooks.md](docs/webhooks.md) |
| Query DSL | `POST /api/query`: read-only JSON queries over the cache. Allowlisted views, fields and operators; never SQL. | [docs/query.md](docs/query.md) |
| Sync cache | Read-only background sync into `cache.db` over a rolling window. SPECIAL-USE folder tokens, per-account overrides, CONDSTORE flag refresh, UIDVALIDITY rebuild, automatic cleaning and VACUUM. | [docs/sync-cache.md](docs/sync-cache.md) |
| Enrichment | Embeddings (Ollama) and classification (Ollama or datawatch). New mail before backfill; backfill rate limits, quiet hours and GPU-yield gates. | [docs/enrichment.md](docs/enrichment.md) |
| Sender intelligence | A resumable, header-only scan of all history builds a profile per sender in `imap.db`: contact dates, counts each way, reply times, DKIM/DMARC results and a role (signals, cached tags, then the classify model). | [docs/intelligence.md](docs/intelligence.md) |
| Rules | Match on sender, subject, body text or age; actions `trash`, `move`, `flag`, `seen`. Run via MCP, REST or `imap-mcp run-rules`. | [docs/rules.md](docs/rules.md) |
| Encryption | Separate state (`imap.db`) and cache (`cache.db`) files, each optionally encrypted at rest (adiantum + Argon2id). Fails closed on a missing or wrong key. | [docs/encryption.md](docs/encryption.md) |
| Outbound mail | Per-account SMTP block; mail leaves through that domain's server. Password (PLAIN) auth only. | [config.example.yaml](config.example.yaml) |
| Inbound commands | Opt-in, default-deny command channel over email, gated by allowlist, DKIM, DMARC, HMAC and replay protection. | [docs/datawatch-integration.md](docs/datawatch-integration.md) |
| datawatch integration | `${secret:name}` credential references, LLM routing for classification, capacity-aware backfill, the `imap_mcp` messaging backend and a companion skill. | [docs/datawatch-integration.md](docs/datawatch-integration.md) |

---

## Quick start

### 1. Build

Requires Go 1.25 or newer.

```bash
git clone https://github.com/dmz006/imap-mcp
cd imap-mcp
go build -o /path/to/bin/imap-mcp ./cmd/imap-mcp/
```

Ollama with `nomic-embed-text` (embeddings) and `qwen3:1.7b` (classification) is
optional. Without it, everything except enrichment and `semantic_search` works.

### 2. Minimal config

`imap-mcp` reads `./config.yaml` if present, otherwise `~/.config/imap-mcp/config.yaml`.
`serve`, `run-rules`, `auth-setup` and `db encrypt` also accept `--config PATH`.

```yaml
# ~/.config/imap-mcp/config.yaml
accounts:
  - name: personal
    imap: { host: imap.example.com, port: 993, tls: true }
    auth:
      type: plain
      username: you@example.com
      password: ${IMAP_MCP_PERSONAL_PASSWORD}

server:
  host: 127.0.0.1
  port: 8765
  auth:
    tokens:
      - name: claude
        token: ${IMAP_MCP_TOKEN_CLAUDE}
        scopes: [read, write, send, admin]
```

For Gmail App Passwords, Google Workspace OAuth, Microsoft 365, SMTP, sync,
enrichment and encryption, start from the annotated
[config.example.yaml](config.example.yaml).

### 3. Run the HTTP server

```bash
export IMAP_MCP_PERSONAL_PASSWORD='<password>'
export IMAP_MCP_TOKEN_CLAUDE="$(openssl rand -hex 32)"
imap-mcp serve

curl -s http://127.0.0.1:8765/api/health
curl -s -H "Authorization: Bearer $IMAP_MCP_TOKEN_CLAUDE" http://127.0.0.1:8765/api/accounts
```

Tokens must resolve to at least 32 characters. For a long-running setup, see
[docs/deployment.md](docs/deployment.md) (systemd user service).

### 4. Connect Claude Code

Over HTTP, with a static header:

```bash
claude mcp add --transport http imap-mcp http://127.0.0.1:8765/mcp \
  --header "Authorization: Bearer <token>"
```

Or in `.mcp.json`, with `headersHelper` so the token is fetched at connect time
instead of being stored in the file:

```json
{
  "mcpServers": {
    "imap-mcp": {
      "type": "http",
      "url": "http://127.0.0.1:8765/mcp",
      "headersHelper": "/path/to/print-imap-mcp-headers.sh"
    }
  }
}
```

The helper prints a JSON object such as `{"Authorization": "Bearer <token>"}`.
See [docs/auth-tokens.md](docs/auth-tokens.md) for helper examples, including
tokens held in datawatch.

### 5. Or use stdio

Stdio needs no server and no token: the MCP client starts the binary itself.
It reads the default config path (no `--config` flag).

```json
{
  "mcpServers": {
    "imap-mcp": { "command": "/path/to/imap-mcp", "args": [] }
  }
}
```

---

## CLI

| Command | Purpose |
|---------|---------|
| `imap-mcp` | MCP over stdio (any unknown subcommand also falls through to stdio) |
| `imap-mcp serve [--config PATH]` | HTTP server: MCP at `/mcp`, REST at `/api` |
| `imap-mcp auth-setup --account NAME [--config PATH]` | One-time OAuth2 browser sign-in for an `xoauth2` account (Google or Microsoft); callback on `http://localhost:8766/oauth/callback`; saves the token to `token_file` |
| `imap-mcp run-rules [--dry-run] [--config PATH]` | Apply all active rules once and exit (opens only the state DB) |
| `imap-mcp db encrypt [--only state\|cache] [--config PATH]` | Encrypt existing plaintext DB files with the configured keys (stop the service first) |
| `imap-mcp version` | Print the version |
| `imap-mcp help` | Print usage |

---

## Configuration

[config.example.yaml](config.example.yaml) documents every option. Top-level blocks:

| Block | Purpose |
|-------|---------|
| `accounts` | IMAP accounts: `auth.type` is `plain`, `xoauth2` or `xoauth2_service_account`; optional `smtp`, `sync` and `inbound` per account |
| `working_dir` | The only directory the file-sandbox tools read and write (default `~/workspace/email`) |
| `server` | Bind host/port and `auth` (tokens and scopes) |
| `db` | State DB path, cache DB path, optional encryption keys |
| `enrichment` | Providers, models, lanes, rate limits, yield gates |
| `sync` | Interval, folders, window, size cap, cleaning, VACUUM |
| `log` | Level and format |
| `datawatch` | Optional: API URL, token and CA file for `${secret:name}` resolution and LLM routing |

Credential values can be `${ENV_VAR}` (from the environment), `${secret:name}`
(from datawatch; needs the `datawatch` block) or a literal. Prefer references.

### Environment overrides

These `IMAP_MCP_*` variables override YAML values (see [.env.example](.env.example)):

| Variable | Overrides |
|----------|-----------|
| `IMAP_MCP_SERVER_HOST`, `IMAP_MCP_SERVER_PORT` | `server.host`, `server.port` |
| `IMAP_MCP_SERVER_AUTH_DISABLED` | `server.auth.disabled` |
| `IMAP_MCP_DB_PATH`, `IMAP_MCP_DB_ENCRYPTION_KEY` | `db.path`, `db.encryption_key` |
| `IMAP_MCP_DB_CACHE_PATH`, `IMAP_MCP_DB_CACHE_ENCRYPTION_KEY` | `db.cache.path`, `db.cache.encryption_key` |
| `IMAP_MCP_SYNC_INTERVAL_MINUTES`, `IMAP_MCP_SYNC_WINDOW_DAYS`, `IMAP_MCP_SYNC_MAX_MESSAGE_MB` | `sync.interval_minutes`, `sync.window_days`, `sync.max_message_mb` |
| `IMAP_MCP_SYNC_KEEP_FLAGGED`, `IMAP_MCP_SYNC_VACUUM_INTERVAL_HOURS` | `sync.keep_flagged`, `sync.vacuum_interval_hours` |
| `IMAP_MCP_OLLAMA_URL` | `enrichment.ollama_url` |
| `IMAP_MCP_ENRICHMENT_CONCURRENCY`, `_BACKFILL_PER_MINUTE`, `_MAX_ATTEMPTS`, `_BACKOFF_MAX_SECONDS` | matching `enrichment.*` fields |
| `IMAP_MCP_ENRICHMENT_BACKFILL_WINDOW`, `_CLASSIFY_PROVIDER`, `_YIELD` | `enrichment.backfill_window`, `enrichment.classify.provider`, `enrichment.yield.enabled` |
| `IMAP_MCP_LOG_LEVEL` | `log.level` |

---

## Documentation

| Doc | Contents |
|-----|----------|
| [docs/README.md](docs/README.md) | Docs index |
| [docs/examples.md](docs/examples.md) | **Start here:** runnable recipes: ask-your-agent prompts, `/api/query` snapshots, a cron digest, rules, webhook notifications, semantic search, live events, cache tuning, encryption |
| [docs/auth-tokens.md](docs/auth-tokens.md) | Token auth and scopes, generating tokens, client headers, `headersHelper`, datawatch-held tokens, opt-out |
| [docs/rest-api.md](docs/rest-api.md) | Every REST route, `/api/health` fields, `/api/events` SSE format and event types |
| [docs/webhooks.md](docs/webhooks.md) | Registering webhooks, delivery, retries, signatures |
| [docs/query.md](docs/query.md) | `/api/query` DSL reference |
| [docs/rules.md](docs/rules.md) | Rules engine, `run-rules`, dry run, scheduling |
| [docs/sync-cache.md](docs/sync-cache.md) | Sync window, folders, overrides, cleaning, `cache_sweep` |
| [docs/enrichment.md](docs/enrichment.md) | Enrichment pipeline, providers, lanes, gates, status |
| [docs/encryption.md](docs/encryption.md) | Storage split, at-rest encryption, keys, `db encrypt`, backups |
| [docs/deployment.md](docs/deployment.md) | systemd user service, scheduling, upgrades |
| [docs/known-limitations.md](docs/known-limitations.md) | What does not work yet |
| [docs/datawatch-integration.md](docs/datawatch-integration.md) | Secrets, companion skill, messaging backend, inbound commands |
| [docs/enterprise-gmail-oauth.md](docs/enterprise-gmail-oauth.md) | Google Workspace OAuth: user consent and service-account delegation |
| [docs/cookbook-inbox-cleanup.md](docs/cookbook-inbox-cleanup.md) | Worked example: dig a large inbox out of bulk mail, sort it into labels, and keep it clean automatically with an hourly datawatch spawn job |
| [docs/architecture/overview.md](docs/architecture/overview.md) | Architecture overview |
| [IMAP-MCP-CONTEXT.md](IMAP-MCP-CONTEXT.md) | Context loader for agent sessions |
| [skills/imap-mcp/SKILL.md](skills/imap-mcp/SKILL.md) | Companion usage skill for agents |
| [config.example.yaml](config.example.yaml), [.env.example](.env.example) | Annotated configuration and environment examples |

---

## Project layout

```
cmd/imap-mcp/       Entry point and CLI subcommands
internal/
  api/              REST API router and handlers
  bus/              In-process event bus
  config/           YAML + env loading, secret references, auth and DB key resolution
  db/               SQLite state DB (imap.db) and cache DB (cache.db), encryption, migration
  enrichment/       Background embedding/classification pipeline
  httpauth/         Scoped bearer-token authentication
  imap/             IMAP connection pool and auth (plain, xoauth2, service account)
  inbound/          Trust-gated inbound command channel
  mcp/              MCP server, tool definitions, handlers, scope map
  output/           working_dir file sandbox
  query/            /api/query DSL compiler
  server/           Combined HTTP server (MCP + REST, browser guard)
  service/          Shared implementation of operations
  smtp/             Per-account outbound mail
  sync/             Background mail cache sync and cleaning
  testutil/         Test helpers
  trust/            Inbound trust gates (DKIM, DMARC, HMAC, replay)
  webhook/          Durable webhook outbox and dispatcher
docs/               User documentation (see above); docs/plans/ holds design plans
skills/imap-mcp/    Companion agent skill
```

Development:

```bash
go build ./...
go test ./...
```

To add an MCP tool: define it in `internal/mcp/tools/definitions.go`, implement
the handler in `internal/mcp/tools/`, register it in `internal/mcp/server.go`,
and add its scope to `internal/mcp/scopes.go` (a test fails if it is missing).
See [AGENT.md](AGENT.md) for project conventions.

---

## Roadmap

Open items only:

- Intelligence: the knowledge graph and anomaly detection on top of sender profiles, so `kg_query`, `get_anomalies` and the matching `/api/query` views return data and `anomaly.detected` is published ([plan](docs/plans/2026-10-09-intelligence-and-stubs.md), P3–P4).
- IMAP IDLE for push delivery of new mail (sync is currently interval-based).
- PGP gate for inbound commands (`require_pgp` currently fails closed).

---

## License

MIT. See [LICENSE](LICENSE).
