# imap-mcp Architecture Overview

**Version:** 0.10.4

imap-mcp is a single Go binary. It connects to one or more IMAP accounts and
exposes them to AI agents over MCP and to scripts over a REST API. It keeps a
local cache of recent mail that is enriched in the background (embeddings and
classification). Facts here come from the code; where they differ, the code
wins. Current gaps are listed in [known limitations](../known-limitations.md).

---

## Components

```
                MCP client (stdio)        MCP client / scripts / datawatch (HTTP)
                       │                                │
                       │                  ┌─────────────▼──────────────┐
                       │                  │ browserGuard               │ Host / Origin / JSON-only checks
                       │                  │ httpauth (bearer + scopes) │ fails closed
                       │                  ├──────────────┬─────────────┤
                       │                  │ /mcp         │ /api        │
                       │                  │ Streamable   │ REST (chi)  │
                       │                  │ HTTP         │ + SSE       │
                       ▼                  └──────┬───────┴──────┬──────┘
               ┌────────────────────────────────▼──┐            │
               │ MCP tools (45; scope per tool)    │            │
               └───────────────┬───────────────────┘            │
                               ▼                                ▼
               ┌──────────────────────────────────────────────────────┐
               │ service layer (internal/service), shared by MCP+REST │
               └──┬──────────┬────────────┬──────────────┬────────────┘
                  │          │            │              │
        ┌─────────▼───┐ ┌────▼─────┐ ┌────▼──────┐ ┌─────▼──────┐
        │ IMAP pool   │ │ SMTP     │ │ state DB  │ │ cache DB   │
        │ N accounts  │ │ per acct │ │ imap.db   │ │ cache.db   │
        │ keepalive   │ └──────────┘ └───────────┘ └─▲───────▲──┘
        └──┬──────┬───┘                              │       │
           │      │      ┌───────────────────────────┘       │
           │  ┌───▼──────┴─┐   queue    ┌───────────────────┴─────────────┐
           │  │ sync engine├───────────►│ enrichment pipeline             │
           │  │ (windowed) │            │ lanes: new > backfill           │
           │  └─────┬──────┘            │ gates: window, ollama, datawatch│
           │        │                   │ providers: Ollama / datawatch   │
     ┌─────▼──────┐ │                   └──────────────┬──────────────────┘
     │ inbound    │ │                                  │
     │ watcher +  │ │        ┌─────────────────────────▼───────┐
     │ trust gates├─┴───────►│ event bus (internal/bus)        │
     └────────────┘          └───┬───────────────┬─────────────┘
                                 │               │
                        /api/events (SSE)   webhook enqueuer → outbox (imap.db)
                        → datawatch, tools          → dispatcher → HTTP POST
```

| Component | Package | Role |
|-----------|---------|------|
| IMAP pool | `internal/imap` | One connection per account, all connected at start. NOOP keepalive every 4 minutes, reconnect on failure, live probe for `/api/accounts`. Publishes `account.connected` / `account.error` |
| IMAP auth | `internal/imap/auth` | `plain`, `xoauth2` (Google or Microsoft; token file refreshed automatically), `xoauth2_service_account` (Google Workspace). `auth-setup` runs the browser flow on a loopback callback and verifies a random `state` |
| Sync engine | `internal/sync` | Per account and configured folder: resolve SPECIAL-USE (`\Sent`), EXAMINE (read-only), `UID SEARCH SINCE` the window, fetch new mail, drop gone mail from the cache, update flags with CONDSTORE where available, rebuild a folder on UIDVALIDITY change. Never changes the mailbox |
| Cache | `internal/db` | `cache.db`: messages, FTS5, vectors, sync state, enrichment queue. Disposable |
| Enrichment pipeline | `internal/enrichment` | Embeds and classifies cached mail. See below |
| Sender intelligence | `internal/intel` | Resumable, PEEK-only header scan of all folders; sender profiles, hashed per-message index, reply pairing, roles, the knowledge graph and anomaly detection in `imap.db`. See [intelligence.md](../intelligence.md) |
| Service layer | `internal/service` | One implementation of each operation, called by MCP tools and REST handlers. Typed errors map to REST status codes and MCP tool errors. Some MCP cleanup tools (`move_bulk`, `flag_bulk`, `purge_sender`, `label_*`, `empty_trash`, `top_senders`, `summarize_folder`, `detect_subscriptions`) still call the IMAP pool directly |
| MCP server | `internal/mcp` | Registers 45 tools; scope middleware and `tools/list` filter when HTTP auth is on |
| REST API | `internal/api` | chi router, one scope per route, SSE at `/api/events` |
| HTTP server | `internal/server` | Mounts `/mcp` and `/api` behind `browserGuard` and auth; graceful shutdown ends open streams |
| Auth middleware | `internal/httpauth` | Named bearer tokens with scopes `read`, `write`, `send`, `admin` |
| Event bus | `internal/bus` | In-process pub/sub; `Publish` is synchronous, `PublishAsync` uses a goroutine |
| Webhooks | `internal/webhook` | Enqueuer writes one outbox row per matching webhook; Dispatcher (in `serve`) delivers with retries |
| Rules | `internal/service/rules.go`, `internal/db/rules.go` | Persisted match → action rules (`trash`, `move`, `flag`, `seen`) run on demand or by `run-rules` |
| Query DSL | `internal/query` | `/api/query` JSON compiled to parameterized SQL over read-only views (cache and state) |
| Inbound trust and comm | `internal/inbound`, `internal/trust` | Watcher polls inbound-enabled folders every 60 s; gates decide whether a command email becomes `inbound.command` |
| SMTP | `internal/smtp` | Per-account outbound mail, STARTTLS or implicit TLS, PLAIN auth, header-injection safe |
| Output sandbox | `internal/output` | The only file writer in the MCP layer; confined to `working_dir` |
| Config | `internal/config` | YAML, `IMAP_MCP_*` env overrides, `${ENV}` and `${secret:name}` references, fail-closed resolution of tokens and DB keys |

---

## Process modes

| Mode | Command | Runs |
|------|---------|------|
| stdio | `imap-mcp` | MCP over stdin/stdout, no HTTP auth. Also runs sync, enrichment and the inbound watcher. No REST, no SSE, no webhook delivery |
| HTTP | `imap-mcp serve [--config PATH]` | Everything in stdio mode plus `/mcp`, `/api`, auth and the webhook dispatcher |
| One-shot rules | `imap-mcp run-rules [--dry-run] [--config PATH]` | Opens only `imap.db`, connects accounts, applies active rules, queues `rule.fired` webhooks for `serve` to deliver, exits |
| OAuth setup | `imap-mcp auth-setup [--account NAME] [--config PATH]` | Browser flow for an `xoauth2` account |
| Encryption | `imap-mcp db encrypt [--only state\|cache] [--config PATH]` | Converts plaintext DB files in place (service stopped) |

Config lookup: `--config` when given. Otherwise, and always in stdio mode
(which takes no flags), `config.yaml` in the current directory if present,
else `~/.config/imap-mcp/config.yaml`.

---

## Package tree

```
imap-mcp/
├── cmd/imap-mcp/main.go        subcommands, dependency wiring, config lookup
├── internal/
│   ├── api/                    REST router + handlers (server, handlers, intel, webhooks)
│   ├── bus/                    event bus and event types
│   ├── config/                 config, env overrides, server auth, DB keys, datawatch secrets + TLS
│   ├── db/                     state + cache DBs, schema, migration, encryption, repositories
│   ├── enrichment/             pipeline, providers, gates, cleaner hook, Ollama client
│   ├── httpauth/               bearer-token auth and scopes
│   ├── imap/                   connection pool
│   │   └── auth/               plain, xoauth2, service account, auth-setup
│   ├── inbound/                inbound watcher, parser, processor
│   ├── mcp/                    MCP server and tool scopes
│   │   └── tools/              tool definitions and handlers
│   ├── output/                 working_dir sandbox writer
│   ├── query/                  /api/query DSL compiler
│   ├── server/                 combined HTTP server, browserGuard
│   ├── service/                shared operations (mail, intel, rules, send, stats, query, webhooks)
│   ├── smtp/                   per-account SMTP sender
│   ├── sync/                   sync engine, mailbox source, MIME decode
│   ├── testutil/imaptest/      in-memory IMAP server for tests
│   ├── trust/                  inbound trust gates and command envelope
│   └── webhook/                outbox enqueuer and dispatcher
├── skills/imap-mcp/SKILL.md    agent skill
├── docs/                       documentation
├── config.example.yaml         annotated config reference
├── AGENT.md                    operating rules and decision log
└── IMAP-MCP-CONTEXT.md         agent quick reference
```

---

## Data model

Two SQLite files (pure-Go ncruces driver). Each can be encrypted at rest with
the adiantum VFS, keyed by its own passphrase.

### `imap.db`: state

Operator data that cannot be rebuilt from IMAP. Default path
`~/.local/share/imap-mcp/imap.db`.

| Table | Contents |
|-------|----------|
| `rules` | Name, description, conditions JSON, actions JSON, active, priority, run_count |
| `webhooks` | URL, subscribed event types, signing secret, active, fail_count |
| `webhook_deliveries` | Durable outbox: delivery id, event, metadata-only payload, status, attempts, next attempt |
| `inbound_nonces` | `(account, nonce)` pairs already honoured, for replay protection |
| `senders` | Sender profiles: contact dates, counts each way, reply stats, list/bulk/auto counts, DKIM/DMARC results, role and its source. Built by the header scanner |
| `intel_messages` | D28 index: one row per message (Message-ID hash, date, sender id, direction, In-Reply-To hash); no addresses or content |
| `intel_scan` | Header-scan progress per account and folder |
| `kg_entities`, `kg_relationships` | Temporal knowledge graph: people, organizations, threads, projects, topics; edges with weight, valid_from, last_seen, valid_to and confidence. Built by the header scanner, cached tags and the classify model |
| `anomalies` | Findings: new senders, auth failures, look-alike domains, Reply-To mismatches, silences, volume spikes; location for per-message ones; resolved flag |

### `cache.db`: cache

Rebuilt from IMAP. Default: `cache.db` next to `imap.db`. When its schema
version changes the file is dropped and recreated.

| Table | Contents | Populated |
|-------|----------|-----------|
| `messages` | Account, folder, UID, Message-ID, thread ID, headers, flags, bodies, hall/wing/room tags, enrichment status | By sync and enrichment |
| `messages_fts` | FTS5 over subject, body_text, from_addr, from_name, kept in step by triggers | By triggers |
| `message_vectors` | float32 embedding per message, model, dims | By enrichment |
| `folders` | Folder metadata | |
| `sync_state` | Per account/folder: UIDVALIDITY, highest_modseq, last UID | By sync |
| `enrichment_queue` | Status, lane (0 new, 1 backfill), attempts, last error | By sync and enrichment |

The `/api/query` DSL reads the view `messages` from `cache.db` and the views
`senders`, `anomalies` and `kg` from `imap.db`.

---

## Enrichment pipeline

```
sync caches message ──► enrichment_queue
                          lane 0: new mail     (never gated)
                          lane 1: backfill     (rate limit, default 30/min; LoadGates)

worker: take new lane first, then backfill if every gate allows
  1. embed     direct Ollama (default nomic-embed-text)          → message_vectors
  2. classify  Ollama (default qwen3:1.7b) or datawatch LLM proxy → hall / wing / room
  3. publish   enrichment.done  |  enrichment.error
```

- **Caps:** in-flight calls per provider (default 2).
- **Gates** (backfill only): `backfill_window` (local time window),
  `ollama_load` (pause while other models hold more than the configured memory
  on Ollama, default 8 GB), datawatch capacity pools. A gate that cannot reach
  its data source allows backfill and reports why.
- **Errors:** transient provider errors (network, 429, 5xx) trigger
  exponential backoff (default cap 300 s). After `max_attempts` (default 3) a
  message is marked `error`.
- `enrichment_status` / `GET /api/enrichment/status` report queue depth per
  lane, throughput, backoff and any pause reason. `trigger_enrichment` skips
  the window, yield and rate limit, but not backoff or caps.

---

## Request flow

### MCP

```
stdio:  client ─► mcp-go stdio server ─► tool handler ─► service / IMAP pool ─► result
HTTP:   client ─► browserGuard ─► httpauth.Middleware (bearer → Principal)
               ─► /mcp Streamable HTTP ─► scope filter on tools/list
               ─► scope middleware on tools/call ─► tool handler ─► service ─► result
```

A tool missing from the scope table is denied. A denied call returns
`forbidden: tool "<name>" requires scope "<scope>"`.

### REST

```
client ─► browserGuard ─► httpauth.Middleware ─► chi route
       ─► RequireScope(<scope>) ─► handler ─► service ─► JSON (service error kind → HTTP status)
```

Routes other than `/api/events` have a 30-second timeout. `/api/events`
lifts the write deadline and sends a heartbeat comment every 15 seconds. The
full route list is in the [REST reference](../rest-api.md).

---

## Event flow

```
publishers ──► bus ──┬──► /api/events SSE clients  (data: {"type", "account", "payload"})
                     │        └── datawatch imap_mcp backend acts on inbound.command,
                     │            replies via POST /api/accounts/{account}/messages/send
                     └──► webhook enqueuer ─► webhook_deliveries ─► dispatcher ─► HTTP POST
```

| Event | Publisher |
|-------|-----------|
| `message.synced`, `message.updated`, `message.deleted` | sync |
| `folder.synced`, `sync.complete`, `sync.error` | sync |
| `cache.cleaned` | sync cleaning, `cache_sweep` |
| `enrichment.done`, `enrichment.error` | enrichment |
| `rule.fired` | rules |
| `account.connected`, `account.error` | IMAP pool |
| `webhook.delivered`, `webhook.failed` | webhook dispatcher |
| `inbound.command`, `inbound.rejected` | inbound processor |
| `anomaly.detected` | intel scanner (`{id, type, severity}`) |
| `account.disconnected` | declared, never published |

Webhook payloads carry identifiers, counts and flags only, never subjects,
addresses or bodies. Receivers fetch details over REST with their own token.
Delivery is at least once, deduplicable by delivery id, and survives restarts.
The `run-rules` CLI only enqueues (`rule.fired`); `serve` delivers. See
[webhooks](../webhooks.md).

---

## Security model

| Control | Behaviour |
|---------|-----------|
| Scoped tokens | `server.auth.tokens`: each has a name, a value of at least 32 characters and one or more scopes (`read`, `write`, `send`, `admin`). Values are compared in constant time and never logged; names are logged. `/api/health` is the only open path |
| Fail closed | `serve` refuses to start with no tokens unless `server.auth.disabled: true` (logged as insecure). Unresolved `${...}` references in tokens or DB keys, a `${secret:}` without a datawatch block, and a missing or wrong DB key all stop startup |
| browserGuard | Accepts only loopback hosts or the configured bind host in `Host` and `Origin` (blocks DNS rebinding and cross-site requests; `null` origins refused). Unsafe `/api` methods must send `application/json`, which blocks simple-request CSRF |
| Encryption at rest | Optional per file (`db.encryption_key`, `db.cache.encryption_key`), adiantum VFS. `imap-mcp db encrypt` converts existing files after verifying tables and row counts. See [encryption](../encryption.md) |
| Secrets | `${ENV}` or `${secret:name}` from datawatch. Credentials never appear in YAML in plain text by convention |
| Inbound commands | Default deny. Every configured gate must pass (allowlist, DKIM/DMARC from `Authentication-Results`, HMAC over the envelope, nonce replay store) and the verb must be in the account's `capabilities`. PGP gate fails closed |
| Output sandbox | MCP file tools write only under `working_dir`; absolute paths and `..` are rejected |
| Sync is read-only | EXAMINE and `BODY.PEEK`; cache deletes never touch the mailbox |
| stdio | No HTTP auth: the MCP client process owns the session and every tool is available |

---

## MCP client naming (`.mcp.json`)

The key under `mcpServers` is the server name, and Claude Code prefixes tools
with it: `mcp__<name>__<tool>`. Use `imap-mcp` so tools appear as
`mcp__imap-mcp__list_accounts` and match the docs and skill.

```json
{ "mcpServers": { "imap-mcp": { "command": "/path/to/imap-mcp/imap-mcp" } } }
```

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

Put the entry in `~/.mcp.json` (all sessions) or a project `.mcp.json`.
datawatch keeps non-datawatch entries when it rewrites these files. Token
setup, including a header helper so the token stays out of the file, is in
[auth tokens](../auth-tokens.md).

---

## Extension points

The bus is the extension point. New consumers (IDLE push, anomaly detection,
federation) subscribe to events rather than calling subsystems directly.
Planned work is tracked in [plans](../plans/README.md).
