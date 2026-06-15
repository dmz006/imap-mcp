# imap-mcp

**Current release: v0.3.1 (2026-05-31)**

An IMAP server with a [Model Context Protocol (MCP)](https://modelcontextprotocol.io) interface for use with Claude Code, plus a REST API for scripted mail automation. Connects to one or more IMAP accounts simultaneously, maintains a local SQLite intelligence layer (full-text search, vector semantic search, sender profiles, temporal knowledge graph, anomaly detection), and enriches email data in the background using local Ollama models.

---

## What it does

- **MCP interface** — 28 tools covering reading, searching, writing, and mail intelligence, accessible from Claude Code sessions
- **REST API** — HTTP endpoints for scripted automation, algorithmic workflows, and integration with other tools
- **Multi-account** — connects to all configured IMAP accounts simultaneously; tools accept an optional `account` parameter
- **Local intelligence layer** — SQLite cache with FTS5 full-text search, vector semantic search (nomic-embed-text via Ollama), temporal knowledge graph, and enriched sender profiles
- **Background enrichment** — qwen3:1.7b classifies each email (hall/wing/room tags) and extracts entity relationships without blocking MCP calls
- **Subscription detection** — scans for `List-Unsubscribe` headers across all accounts and returns ranked sender list with unsubscribe URLs
- **Bulk operations** — move/flag/delete by sender, subject, or date range with safety caps

---

## Architecture

```
imap-mcp binary
├── stdio transport       → Claude Code native MCP (zero config)
└── HTTP server (:8765)
    ├── /mcp              → MCP Streamable HTTP (2025-03-26 spec)
    └── /api              → REST API

Internal:
├── IMAP pool             → persistent connections to all accounts
├── Event bus             → all subsystems publish/subscribe here
├── SQLite DB             → messages, vectors, senders, KG, anomalies
├── Enrichment pipeline   → Ollama background worker
└── Sync engine           → periodic + on-demand IMAP sync
```

Both MCP and REST run on the same port. Claude Code connects via stdio (no network) or HTTP (persistent connections). The binary auto-detects the mode based on how it's invoked.

---

## Requirements

- **Go 1.21+** to build
- **Ollama** running locally for enrichment (optional but recommended)
  - `nomic-embed-text` model for vector search
  - `qwen3:1.7b` model for email classification
- An IMAP account with credentials

---

## Installation

```bash
git clone https://github.com/dmz006/imap-mcp
cd imap-mcp
go build -o imap-mcp ./cmd/imap-mcp/
```

Or install directly:

```bash
go install github.com/dmz006/imap-mcp/cmd/imap-mcp@latest
```

---

## Configuration

Copy the example and edit:

```bash
cp config.example.yaml ~/.config/imap-mcp/config.yaml
```

### Minimal config (single account, plain auth)

```yaml
accounts:
  - name: personal
    default: true
    imap:
      host: imap.example.com
      port: 993
      tls: true
    auth:
      type: plain
      username: user@example.com
      password: your-password-here   # or ${ENV_VAR} to read from environment

server:
  host: 127.0.0.1
  port: 8765

db:
  path: ~/.local/share/imap-mcp/imap.db
```

### Gmail

Gmail requires an **App Password** — not your regular Google password. IMAP is always enabled (no toggle needed since January 2025).

**Setup (5 minutes):**
1. Enable 2-Step Verification on your Google account: https://myaccount.google.com/security
2. Generate an App Password: https://myaccount.google.com/apppasswords
   - Name it `imap-mcp`
   - Copy the 16-character code (no spaces)
3. Use the code as your password:

```yaml
accounts:
  - name: gmail
    imap:
      host: imap.gmail.com
      port: 993
      tls: true
    auth:
      type: plain
      username: you@gmail.com
      password: abcdabcdabcdabcd   # 16-char app password, no spaces
```

### Outlook / Microsoft 365

```yaml
accounts:
  - name: work
    imap:
      host: outlook.office365.com
      port: 993
      tls: true
    auth:
      type: plain
      username: you@company.com
      password: your-app-password   # generate at account.microsoft.com
```

For organizational accounts that require OAuth2, use `type: xoauth2` with client credentials and run `imap-mcp auth-setup --account work` to complete the browser flow.

### Multiple accounts

```yaml
accounts:
  - name: personal
    default: true
    imap:
      host: imap.fastmail.com
      port: 993
      tls: true
    auth:
      type: plain
      username: user@fastmail.com
      password: ${FASTMAIL_PASSWORD}

  - name: gmail
    imap:
      host: imap.gmail.com
      port: 993
      tls: true
    auth:
      type: plain
      username: you@gmail.com
      password: ${GMAIL_APP_PASSWORD}
```

All accounts connect simultaneously on startup. Every MCP tool accepts an optional `account` parameter; omit it to use the default account.

### Full config reference

See [`config.example.yaml`](config.example.yaml) for all options with comments. Key sections:

| Section | Purpose |
|---------|---------|
| `accounts[]` | IMAP account definitions |
| `server` | HTTP server host and port |
| `db.path` | SQLite database location |
| `enrichment` | Ollama settings, models, batch size |
| `sync` | Sync interval, folders to watch |
| `log` | Level (debug/info/warn/error), format (text/json) |

Environment variable overrides: prefix any config field with `IMAP_MCP_` in SCREAMING_SNAKE_CASE. Example: `IMAP_MCP_SERVER_PORT=9000` overrides `server.port`.

---

## Running

### stdio mode (for Claude Code)

```bash
./imap-mcp
```

Reads config from `~/.config/imap-mcp/config.yaml` by default. Use `--config` to specify a different path. This is the mode Claude Code uses when you add imap-mcp as an MCP server.

### HTTP server mode

```bash
./imap-mcp serve
./imap-mcp serve --config /path/to/config.yaml
```

Starts the combined HTTP server. MCP is available at `/mcp`, REST API at `/api`. Recommended when you want persistent IMAP connections shared across multiple Claude sessions, or when using the REST API for automation.

### OAuth2 setup (Gmail / Outlook)

```bash
./imap-mcp auth-setup --account gmail
./imap-mcp auth-setup --account work --config /path/to/config.yaml
```

Opens a browser, completes the OAuth2 flow, and saves the refresh token to the configured `token_file`. Only needed for `xoauth2` auth type accounts.

---

## Claude Code integration

### Wiring with Claude Code

imap-mcp runs **alongside** any existing MCP servers (including datawatch). Claude Code's MCP config merges all servers — they don't conflict.

**Add to `~/.mcp.json`** (global — available in all Claude Code sessions):

```json
{
  "mcpServers": {
    "imap-mcp": {
      "command": "/path/to/imap-mcp",
      "args": [],
      "env": {}
    }
  }
}
```

Or use HTTP mode (recommended for persistent IMAP connections — start `imap-mcp serve` first):

```json
{
  "mcpServers": {
    "imap-mcp": {
      "url": "http://localhost:8765/mcp"
    }
  }
}
```

### Using with datawatch

> **Full walkthrough:** [`docs/datawatch-integration.md`](docs/datawatch-integration.md) — an end-to-end, task-oriented guide to all three layers (secrets, skill, comm), standalone vs integrated, with copy-paste config. The sections below summarize each layer.


If you use [datawatch](https://github.com/dmz006/datawatch), imap-mcp coexists without configuration changes. datawatch's `WriteProjectMCPConfig` preserves all non-datawatch entries in `.mcp.json` on every session spawn. Add imap-mcp to `~/.mcp.json` once and it persists through datawatch session spawns automatically.

A `extra_mcp_servers` config option for datawatch is tracked at [datawatch#118](https://github.com/dmz006/datawatch/issues/118). Note: imap-mcp does **not** rely on auto-injection. Whether, when, and where imap-mcp is connected to a session is an **operator decision** — you attach it to the specific sessions or projects you choose. A session that wasn't given imap-mcp simply doesn't have it.

### Companion skill (datawatch community registry)

A usage skill is published to the datawatch community registry at
`skills/comms/imap-mcp` ([dmz006/datawatch-community](https://github.com/dmz006/datawatch-community)).
It teaches an agent the safe workflows for these tools (triage, unsubscribe,
sender audit, bulk-archive, search, export). It is **instructions only** — it
bundles no tools and opens no connection. Pull it on demand:

```
# over datawatch MCP:    skills_registry_sync { name: "community", skills: "imap-mcp" }
# or CLI:                datawatch skills registry sync community imap-mcp
```

The source of truth lives in this repo under `skills/imap-mcp/SKILL.md`.

### Bidirectional comm: outbound send + trust-gated inbound

imap-mcp can act as a full communication channel — sending mail *and* receiving
trust-gated commands — so datawatch (or any consumer) can treat email as a
first-class comm. **None of this auto-injects into any session;** it activates
only via per-account config you opt into.

**Outbound (per-domain SMTP).** Each account gets its own `smtp:` block, so mail
leaves through the correct domain's server, never a shared relay. Credentials
default to the IMAP auth block. The `send_message` MCP tool drives it:

```yaml
smtp:
  host: smtp.example.com
  port: 587            # 587=STARTTLS, 465=implicit TLS
  starttls: true
  from: "Me <me@example.com>"
```

**Inbound command channel (default-deny, composable gates).** An account can
become a trust-gated control channel. A command (a fenced envelope in the body)
is acted on **only if every required gate passes** — imap-mcp is the trust
boundary and emits a verified-command event only then. Because `From:` is
spoofable, gates are layered and composable per account:

| Gate | What it proves |
|------|----------------|
| `allowlist` | sender address/domain is expected (coarse; spoofable alone) |
| `require_dkim` / `require_dmarc` | the receiving server validated domain auth (`dkim=pass`/`dmarc=pass` in `Authentication-Results`) |
| `hmac_secret` | the command envelope carries a valid HMAC-SHA256 (shared secret) |
| `replay_window_minutes` + nonce | command is fresh and single-use (no replay) |
| `require_pgp` | **backlog** — PGP-signed envelope; **fails closed** until implemented |

With `inbound.enabled: true` you must configure at least one gate, or startup
refuses (no ungated command channel). A verified command must also name a verb
in the account's `capabilities` list — capability scoping, default-deny. The
command envelope format and the full gate config are documented in
`config.example.yaml`.

> Note: imap-mcp emits `inbound.command` (verified) and `inbound.rejected`
> (audited) events over `GET /api/events` (SSE). A downstream consumer acts
> **only** on verified events, never on raw mail. datawatch's `imap_mcp`
> messaging backend does exactly this (consumes verified events via SSE, sends
> via `POST /api/accounts/{account}/messages/send`) — the loop is closed as of
> imap-mcp v0.2.1 + datawatch#127. imap-mcp owns the mail + crypto trust
> boundary; datawatch owns command dispatch. See
> [`docs/datawatch-integration.md`](docs/datawatch-integration.md).

### Credentials: standalone vs datawatch secrets

imap-mcp resolves each credential in priority order:

1. `${ENV_VAR}` — read from the environment at startup (**standalone**, the default)
2. `${secret:name}` — fetched from a datawatch secrets service at startup (**datawatch-integrated**)
3. plain value — used as-is

**Standalone** (no datawatch dependency):

```yaml
auth:
  type: plain
  username: user@gmail.com
  password: ${GMAIL_APP_PASSWORD}      # from the environment
```

**datawatch-integrated** — store the credential in datawatch's secrets service and reference it. This requires a `datawatch:` block; without one, a `${secret:...}` reference is a startup error (so you never get a silent placeholder):

```yaml
accounts:
  - name: gmail
    auth:
      type: plain
      username: user@gmail.com
      password: ${secret:gmail_app_password}   # fetched from datawatch

# Resolve ${secret:...} against a running datawatch instance.
datawatch:
  api_url: ${DATAWATCH_API_URL}          # e.g. http://localhost:7777
  token: ${DATAWATCH_SECRETS_TOKEN}      # agent-scoped secrets token (least privilege)
```

imap-mcp fetches each secret over `GET {api_url}/api/agents/secrets/{name}` with the bearer token. The token and API URL are themselves `${ENV_VAR}` references — **never put a literal token in a config file or commit one to a repo.** Both modes are fully supported; the datawatch block is optional and additive — remove it and imap-mcp runs entirely on its own.

---

## MCP Tools

All tools accept an optional `account` parameter. Omit to use the default account.

### Account & sync

| Tool | Description |
|------|-------------|
| `list_accounts` | List all configured accounts and connection status |
| `sync_account` | Trigger an immediate IMAP sync for an account |

### Folders

| Tool | Parameters | Description |
|------|-----------|-------------|
| `list_folders` | `account` | List all mailbox folders/labels |
| `create_folder` | `account`, `path`* | Create a new folder |
| `delete_folder` | `account`, `path`* | Delete a folder (must be empty) |

### Reading messages

| Tool | Key parameters | Description |
|------|---------------|-------------|
| `list_messages` | `folder`, `limit`, `offset`, `sort`, `order` | Paginated message list with headers |
| `get_message` | `folder`*, `uid`* | Full message fetch including raw body |
| `get_thread` | `thread_id`* | All messages in a conversation thread |
| `get_headers` | `folder`*, `uid`* | Headers-only fetch (fast, no body) |
| `get_attachments` | `folder`*, `uid`*, `part` | List or download attachments |
| `export_message` | `folder`*, `uid`* | Export message as EML format |

### Writing messages

| Tool | Key parameters | Description |
|------|---------------|-------------|
| `move_message` | `folder`*, `uid`*, `destination`* | Move message to another folder |
| `copy_message` | `folder`*, `uid`*, `destination`* | Copy message to another folder |
| `delete_message` | `folder`*, `uid`*, `permanent` | Delete (to Trash or expunge) |
| `set_flags` | `folder`*, `uid`*, `add`, `remove` | Set/clear flags (Seen, Flagged, etc.) |
| `append_message` | `folder`*, `message`*, `flags` | Write a raw RFC 2822 message into a folder |
| `move_bulk` | `folder`*, `query`/`subject`, `destination`* | Move all matching messages in bulk |
| `flag_bulk` | `folder`*, `query`, `add`/`remove` | Flag all matching messages in bulk |
| `purge_sender` | `from`*, `folder`, `permanent` | Move **all** mail from a sender to Trash, draining the folder in one call (auto-detects Trash) |
| `label_message` | `uid`*, `label`*, `folder`, `create` | Apply a Gmail label (COPY into the label mailbox; creates it if missing) |
| `empty_trash` | `account` | Permanently delete everything in the auto-detected Trash mailbox |
| `send_message` | `to`*, `subject`*, `body`*, `cc` | Send via the account's SMTP (per-domain) |

### Cleanup automation & analytics

| Tool | Key parameters | Description |
|------|---------------|-------------|
| `top_senders` | `folder`, `top`, `scan`, `group_by` | Rank a folder's senders by count (address/domain) — surfaces bulk/spam clusters across the whole folder in one call |
| `create_rule` | `name`*, `action`*, `from`/`subject`/`text`/`older_than_days`, `dest`, `flags`, `account`, `folder` | Persist a match→action rule (trash/move/flag/seen) |
| `list_rules` | — | List persisted rules with run counts |
| `delete_rule` | `id`* | Delete a rule |
| `run_rules` | `id`, `dry_run` | Apply active rules now (`dry_run` previews match counts) |

> `search_messages` now returns the **true** `total_matches` (no longer capped at the page size), and connections self-heal via a NOOP keepalive + auto-reconnect, with `/api/accounts` reporting live-probed state.

### Search

| Tool | Key parameters | Description |
|------|---------------|-------------|
| `search_messages` | `folder`, `from`, `subject`, `text`, `since`, `before`, `flags`, `hall`, `wing`, `room` | IMAP SEARCH + FTS hybrid |
| `cross_account_search` | `from`, `subject`, `text`, `since`, `before` | Search all accounts simultaneously |
| `semantic_search` | `query`, `reference_uid`, `threshold` | Vector similarity search via embeddings |

### Intelligence

| Tool | Key parameters | Description |
|------|---------------|-------------|
| `summarize_folder` | `folder`* | Folder stats (total, recent, unseen) without reading bodies |
| `detect_subscriptions` | `folder`, `limit` | Scan for List-Unsubscribe headers; return ranked sender list with unsubscribe URLs |
| `get_sender_history` | `address`* | All messages from a specific address |
| `get_sender_profile` | `address`* | Enriched sender profile (role, counts, reply time, anomalies) |
| `kg_query` | `entity`, `predicate`, `entity_type` | Query the temporal knowledge graph |
| `get_anomalies` | `severity`, `unresolved_only` | Behavioral anomalies (silence, reply spikes, role shifts) |
| `enrichment_status` | `account` | Enrichment pipeline status (pending/done/error counts) |
| `trigger_enrichment` | `account`, `limit` | Force immediate enrichment run |

### File output (working directory sandbox)

All file writes are enforced to stay inside `working_dir`. These are the **only** tools that write to disk — `os.WriteFile` is never called directly from the MCP layer.

| Tool | Parameters | Description |
|------|-----------|-------------|
| `write_file` | `filename`*, `content`* | Write content to a file inside `working_dir`. Relative paths only; subdirectories created automatically. Returns the absolute path written. |
| `read_file` | `filename`* | Read a file from `working_dir` |
| `list_files` | `subdir` | List files in `working_dir` or a subdirectory; returns `working_dir` root path |
| `delete_file` | `filename`* | Delete a file from `working_dir` |

Absolute paths and `..` traversal are rejected server-side — the enforcement is structural, not conventional. Configure the output location with `working_dir` in `config.yaml`.

`*` = required parameter

---

## REST API

Base URL: `http://localhost:8765` (when running `imap-mcp serve`)

### Implemented

```
GET  /api/health                                    Server status, version, account count
GET  /api/accounts                                  List accounts and connection status
GET  /api/events                                    SSE event stream (bus events; datawatch consumes inbound.command)
POST /api/accounts/{account}/messages/send          Send mail via the account's SMTP ({to,subject,body,cc}; account may be _default)
```

### Planned (returns 501 until implemented)

```
POST /api/accounts/{account}/sync
GET  /api/accounts/{account}/folders
GET  /api/accounts/{account}/folders/{folder}/messages
GET  /api/accounts/{account}/folders/{folder}/messages/{uid}
DELETE /api/accounts/{account}/folders/{folder}/messages/{uid}
PUT  /api/accounts/{account}/folders/{folder}/messages/{uid}/flags
POST /api/accounts/{account}/folders/{folder}/messages/{uid}/move

GET  /api/search
POST /api/search/semantic

GET  /api/accounts/{account}/stats
GET  /api/senders
GET  /api/senders/{address}
GET  /api/kg
GET  /api/anomalies

POST /api/webhooks
GET  /api/webhooks
DELETE /api/webhooks/{id}

GET  /api/rules
POST /api/rules
PUT  /api/rules/{id}
DELETE /api/rules/{id}
POST /api/rules/{id}/test

POST /api/query              ← algorithmic query DSL entry point
```

---

## Intelligence layer

### Email classification (hall/wing/room)

Each synced message gets classified by the local qwen3:1.7b model:

- **hall** — email type: `transactional`, `conversation`, `newsletter`, `notification`, `alert`, `personal`
- **wing** — project/context label (e.g. `imap-mcp`, `work-q1`)
- **room** — topic cluster (e.g. `deployment`, `billing`)

These tags are stored in SQLite and searchable via `search_messages` filters.

### Vector semantic search

Every message gets a 768-dimensional vector from `nomic-embed-text` (Ollama). Stored as float32 blobs in SQLite. `semantic_search` computes cosine similarity in Go at query time — no external vector database needed.

### Sender profiles

Each sender address gets a profile in the `senders` table: first/last contact date, message count, average reply time, detected role (colleague, vendor, newsletter, bot), and anomaly score. Updated by the enrichment pipeline.

### Temporal knowledge graph

Subject-predicate-object triples with `valid_from`/`valid_to` timestamps:

```
alice@corp.com  → manages       → bob@corp.com
thread:X        → belongs_to    → project:imap-mcp
newsletter@co   → is_subscription → true
```

Queryable via `kg_query` MCP tool or `GET /api/kg`.

### Anomaly detection

The enrichment pipeline flags behavioral changes:
- **behavior_change** — sender pattern or role shifted
- **silence** — thread went quiet after activity
- **reply_spike** — unusual reply volume
- **new_sender** — first contact from a domain

Stored in the `anomalies` table, queryable via `get_anomalies`.

---

## Database schema

SQLite at `~/.local/share/imap-mcp/imap.db` (configurable).

Key tables:

| Table | Purpose |
|-------|---------|
| `messages` | Cached message headers + bodies + hall/wing/room tags |
| `message_vectors` | Float32 embedding blobs (768-dim, nomic-embed-text) |
| `senders` | Enriched sender profiles |
| `kg_entities` | Knowledge graph nodes |
| `kg_relationships` | KG edges with temporal validity |
| `anomalies` | Behavioral anomaly events |
| `folders` | Folder metadata and sync state |
| `sync_state` | Per-account/folder UID watermark |
| `enrichment_queue` | Pending/processing enrichment jobs |
| `webhooks` | Registered webhook endpoints |
| `rules` | Automation rules (conditions + actions) |

Full schema: [`internal/db/schema.go`](internal/db/schema.go)

---

## Event bus

All internal subsystems communicate through `internal/bus`. Every significant action publishes an event. This is the primary extension point for autonomous agents, federation, and plugins.

Current event types:

| Event | Published by |
|-------|-------------|
| `message.synced` | Sync engine |
| `sync.complete` | Sync engine |
| `enrichment.done` | Enrichment pipeline |
| `anomaly.detected` | Anomaly detector |
| `account.connected` | IMAP pool |
| `account.error` | IMAP pool |
| `rule.fired` | Rules engine |

---

## Roadmap

**Iteration 2 (in progress):**
- Full IMAP sync with UID range fetch and delta updates
- MIME decoder for clean plain text in `get_message`
- REST API endpoints for messages, search, folders

**Iteration 3:**
- Sender profile builder triggered by enrichment events
- KG entity extractor in enrichment pipeline
- `cross_account_search` and `semantic_search` implementations

**Iteration 4 (watch_folder + streaming):**
- IMAP IDLE push notifications (`watch_folder`)
- `GET /api/events` SSE stream for real-time consumers

**Option 4 (future):**
- Autonomous rule engine (rules fire on bus events without human input)
- Federation (multiple imap-mcp instances sharing KG/enrichment data)
- Plugin system (external bus subscribers with manifest declarations)
- Streaming event bus for external consumers

---

## Development

```bash
# Build
go build -o imap-mcp ./cmd/imap-mcp/

# Run tests
go test ./...

# Build and run against a real account
./imap-mcp serve --config ~/your-config.yaml

# Check health
curl http://localhost:8765/api/health
```

### Project layout

```
cmd/imap-mcp/main.go          Entry point; subcommand routing
internal/
  config/     YAML + env config loader
  bus/        In-process event bus
  imap/       IMAP connection pool + auth (plain, xoauth2)
  db/         SQLite repositories + schema
  enrichment/ Ollama client + background pipeline
  sync/       IMAP sync engine
  mcp/        MCP server + all 28 tool definitions and handlers
  api/        REST API router and handlers
  server/     Combined HTTP server (MCP at /mcp, REST at /api)
docs/
  architecture/  Architecture overview and diagrams
  plans/         Implementation plan documents
config.example.yaml   Annotated config reference
AGENT.md              Operating rules for Claude Code sessions
IMAP-MCP-CONTEXT.md   Session context loader for Claude sessions
```

### Adding a new MCP tool

1. Add the tool definition to `internal/mcp/tools/definitions.go`
2. Register it in `internal/mcp/server.go`
3. Add handler method to `Handlers` struct
4. Implement in a new `impl_*.go` file (or add to an existing one)

### Contributing

Issues and PRs welcome. See [`AGENT.md`](AGENT.md) for development rules and conventions used in this project.

---

## License

MIT — see [LICENSE](LICENSE)
