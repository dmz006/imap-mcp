# IMAP-MCP-CONTEXT.md

**Load this file at the start of every session before writing any code.**

```bash
Read /home/dmz/workspace/imap-mcp/IMAP-MCP-CONTEXT.md
```

Then re-read the relevant sections of `AGENT.md` for the task at hand.

---

## What is imap-mcp?

A Go binary that connects to one or more IMAP email accounts and exposes them via:
1. **MCP (Model Context Protocol)** — Claude Code can call 28 email management tools
2. **REST API** — HTTP endpoints for algorithmic/scripted automation
3. **Intelligence layer** — local SQLite cache with FTS5 full-text search, vector
   semantic search (nomic-embed-text via Ollama), temporal knowledge graph, sender
   profiles, and anomaly detection

Primary use case: design and run mail management tools from Claude Code sessions,
with a local LLM (qwen3:1.7b) enriching email data in the background.

---

## Project Identity

| Field | Value |
|-------|-------|
| Module | `github.com/dmz006/imap-mcp` |
| License | MIT |
| Go version | 1.25.10 |
| Current version | 0.1.0 |
| Location | `/home/dmz/workspace/imap-mcp` |
| Status | v0.1.0 — 34 MCP tools registered; datawatch secrets + bidirectional comm (SMTP send + trust-gated inbound) implemented; some intelligence tools still stubbed |

## datawatch integration (operator-controlled, never auto-injected)

Three independent, opt-in layers. The operator decides if/when imap-mcp attaches
to a session — there is no auto-injection.

1. **Secrets** — `${secret:name}` resolves via datawatch secrets service when a
   `datawatch:` block (api_url, token as env refs) is present; else standalone
   (`${ENV}`/plain). `internal/config/secrets.go`.
2. **Skill** — published to the datawatch community registry at
   `skills/comms/imap-mcp` (github.com/dmz006/datawatch-community). Source of
   truth: `skills/imap-mcp/SKILL.md`. On-demand only.
3. **Comm** — imap-mcp is the *trust boundary*: it sends (per-account SMTP) and
   emits verified `inbound.command` events for trust-gated mail. The datawatch
   *messaging.Backend* that consumes those events is **datawatch-side dev**
   (handed off via issue + session message), not built in this repo.

PGP inbound gate is **backlogged** — declared but fails closed until implemented.

---

## Key Architecture Decisions (locked)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| MCP transport | stdio + Streamable HTTP (no SSE) | SSE is deprecated in MCP spec |
| Config | YAML + env overrides | YAML for structure, env for secrets |
| Multi-account | All accounts connected simultaneously | Enables cross-account tools |
| Auth | Plain + XOAUTH2 + pluggable interface | Gmail/Outlook compatibility |
| `--auth-setup` | Browser OAuth2 flow built in | Never manually copy tokens |
| Cache | SQLite + FTS5 + vectors | Fast offline analytics |
| Embedding | nomic-embed-text via Ollama | Local, free, 768-dim, already installed |
| Enrichment LLM | qwen3:1.7b via Ollama | Local background classification |
| REST API scope | Full platform (MCP mirror + analytics + webhooks + rules + query DSL) | Algorithmic layer |
| Internal bus | Event bus at core (`internal/bus`) | Foundation for agents/federation/plugins |
| Architecture | Designed for Option 4 | Agents, federation, streaming, plugins |

---

## Running the Binary

```bash
# Build
go build -o imap-mcp ./cmd/imap-mcp/

# stdio mode (for Claude Code)
./imap-mcp

# HTTP server mode (persistent connections + REST API)
./imap-mcp serve --config ~/.config/imap-mcp/config.yaml

# OAuth2 setup for Gmail/Outlook
./imap-mcp auth-setup --account work

# Version
./imap-mcp version
```

---

## Configuration

Config file: `~/.config/imap-mcp/config.yaml` (or `--config` flag)  
Template: `config.example.yaml` in project root  
Env vars: see `.env.example`

Key env vars:
```bash
IMAP_MCP_SERVER_PORT=8765
IMAP_MCP_OLLAMA_URL=http://localhost:11434
IMAP_MCP_LOG_LEVEL=debug
```

Credentials always via env refs in YAML:
```yaml
auth:
  password: ${IMAP_MCP_PERSONAL_PASSWORD}
```

---

## Claude Code Integration

### Wiring alongside datawatch (no conflict)

datawatch manages `~/.mcp.json` and project-level `.mcp.json` files, but
**preserves all non-datawatch entries** on every session spawn (`WriteProjectMCPConfig`
in `internal/channel/mcp_config.go`). Add imap-mcp once and it persists.

Add to `~/.mcp.json` (global — available in every Claude Code session):
```json
{
  "mcpServers": {
    "datawatch": { ... },
    "imap-mcp": {
      "command": "/home/dmz/workspace/imap-mcp/imap-mcp",
      "args": [],
      "env": {}
    }
  }
}
```

Or use HTTP mode if you want persistent IMAP connections (start `imap-mcp serve` first):
```json
{
  "mcpServers": {
    "datawatch": { ... },
    "imap-mcp": { "url": "http://localhost:8765/mcp" }
  }
}
```

### datawatch feature request (GH #118)
A first-class `extra_mcp_servers` config option in datawatch would auto-inject
imap-mcp into every spawned session. Filed at:
https://github.com/dmz006/datawatch/issues/118

### stdio (zero config, reconnects each session)
```json
{
  "mcpServers": {
    "imap-mcp": {
      "command": "/home/dmz/workspace/imap-mcp/imap-mcp"
    }
  }
}
```

### HTTP (recommended for persistent connections)
Start server: `./imap-mcp serve --config ~/.config/imap-mcp/config.yaml`
```json
{
  "mcpServers": {
    "imap-mcp": { "url": "http://localhost:8765/mcp" }
  }
}
```

---

## Key Files

| File | Purpose |
|------|---------|
| `cmd/imap-mcp/main.go` | Entry point; subcommand routing; `var Version` |
| `internal/config/config.go` | Config struct; YAML load; env overrides; `var Version` |
| `internal/bus/bus.go` | Event bus; all event type constants |
| `internal/imap/pool.go` | Multi-account connection pool; reconnect logic |
| `internal/imap/auth/` | Authenticator interface, Plain, XOAuth2, auth-setup flow |
| `internal/db/schema.go` | Full SQLite schema (messages, vectors, senders, KG, anomalies, rules, webhooks) |
| `internal/db/db.go` | SQLite open; all repository types |
| `internal/enrichment/pipeline.go` | Background enrichment worker; cosine similarity |
| `internal/enrichment/ollama.go` | Ollama embed + generate API client |
| `internal/sync/syncer.go` | IMAP sync engine |
| `internal/mcp/server.go` | MCP server; all 28 tools registered |
| `internal/mcp/tools/definitions.go` | All tool definitions (names, params, descriptions) |
| `internal/mcp/tools/impl_accounts.go` | list_accounts, sync_account ✅ |
| `internal/mcp/tools/impl_folders.go` | list_folders, create_folder, delete_folder ✅ |
| `internal/mcp/tools/impl_files.go` | write_file, read_file, list_files, delete_file |
| `internal/mcp/tools/impl_send.go` | send_message (per-account SMTP outbound) |
| `internal/output/writer.go` | Enforced output sandbox (only file writer in MCP layer) |
| `internal/config/secrets.go` | `${secret:name}` resolver via datawatch secrets service (optional `datawatch:` block) |
| `internal/smtp/smtp.go` | Per-account SMTP sender (STARTTLS/implicit TLS, header-injection safe) |
| `internal/trust/` | Inbound command-channel trust boundary: composable gates (allowlist, DKIM/DMARC, HMAC, replay), PGP gate fails closed (backlog) |
| `internal/inbound/` | Watcher polls inbound-enabled folders; Processor emits `inbound.command`/`inbound.rejected` bus events |
| `internal/db/nonces.go` | SQLite `inbound_nonces` replay store (implements trust.NonceStore) |
| `internal/mcp/tools/impl_stubs.go` | Remaining unimplemented tools |
| `internal/api/server.go` | REST API router; /api/health, /api/accounts ✅ |
| `internal/server/server.go` | Combined HTTP server (MCP at /mcp, REST at /api) |
| `AGENT.md` | Operating rules for Claude sessions |
| `docs/architecture/overview.md` | Full architecture doc with diagrams |

---

## MCP Tools Status

**Implemented (v0.2.0):**
- `list_accounts` — list configured accounts and connection status
- `sync_account` — trigger immediate sync
- `list_folders` — folder tree for an account (live-tested: returns all mailboxes)
- `create_folder` — create mailbox folder
- `delete_folder` — delete mailbox folder
- `list_messages` — paginated messages with headers, date sort, total count (live-tested: 20,655-message inbox)
- `get_message` — full message fetch by UID including raw body (live-tested; body is raw MIME — decode pass needed)
- `get_headers` — headers-only fetch by UID
- `search_messages` — IMAP SEARCH by from/subject/text/date range/flags (live-tested)

**Known limitation (iteration 2):**
`get_message` body is raw MIME (quoted-printable encoded). Needs MIME decoder pass for clean plain text.

**File output tools (v0.4.0):**
- `write_file` — enforced sandbox write to `working_dir`; rejects absolute paths and `..` traversal
- `read_file` — read file from `working_dir`
- `list_files` — list files in `working_dir` or subdirectory; returns root path
- `delete_file` — delete file from `working_dir`

**Stubbed (iteration 3):**  
Remaining 15 tools return "not yet implemented" error with descriptive message.

**Deferred (later iteration):**  
`watch_folder` — IMAP IDLE push notifications

---

## Database Schema Summary

```sql
messages          -- cached messages with hall/wing/room enrichment tags
message_vectors   -- float32 embedding blobs (768-dim nomic-embed-text)
senders           -- enriched sender profiles (role, counts, anomaly_score)
kg_entities       -- KG nodes: person|organization|topic|project|thread
kg_relationships  -- KG edges with valid_from/valid_to timestamps
anomalies         -- episodic anomaly log: behavior_change|silence|reply_spike
folders           -- folder metadata and last-sync state
sync_state        -- per-account/folder UID watermark
enrichment_queue  -- pending/processing/done enrichment jobs
webhooks          -- registered webhook endpoints
rules             -- automation rules (conditions + actions JSON)
```

Memory patterns borrowed from datawatch:
- **Wing/Room/Hall tagging** — spatial classification for spatial search
- **Temporal KG** — entity relationships with time validity
- **Sender profiles** — first-class entities with relationship history
- **Anomaly episodic log** — behavioral change detection

---

## Enrichment Pipeline

```
Sync writes message → status: pending → enrichment_queue
Pipeline (10s tick):
  1. nomic-embed-text → message_vectors (768-dim float32 blob)
  2. qwen3:1.7b → hall/wing/room classification (JSON from prompt)
  3. bus.Publish(EventEnrichmentDone)
```

Ollama must be running at `http://localhost:11434`.
Models installed: `nomic-embed-text` (261MB), `qwen3:1.7b` (1296MB).

---

## Event Bus

`internal/bus` is the backbone. Every subsystem publishes and subscribes here.
Never couple subsystems via direct calls — always through the bus.

Current event types: `message.synced`, `message.updated`, `message.deleted`,
`folder.synced`, `sync.complete`, `sync.error`, `enrichment.done`, `enrichment.error`,
`anomaly.detected`, `rule.fired`, `account.connected`, `account.error`,
`account.disconnected`, `webhook.delivered`, `webhook.failed`.

---

## REST API Endpoints (v0.1.0)

```
GET  /api/health                          ✅ implemented
GET  /api/accounts                        ✅ implemented
POST /api/accounts/{account}/sync         501 stub
GET  /api/accounts/{account}/folders      501 stub
GET  /api/accounts/{account}/folders/{folder}/messages   501 stub
GET  /api/search                          501 stub
POST /api/search/semantic                 501 stub
GET  /api/senders                         501 stub
GET  /api/kg                              501 stub
GET  /api/anomalies                       501 stub
POST /api/webhooks                        501 stub
POST /api/rules                           501 stub
POST /api/query                           501 stub  ← algorithmic layer entry point
GET  /api/events                          501 stub  ← future streaming/federation
```

---

## Next Iterations

**Iteration 2 (message CRUD):**
- Implement `list_messages` — paginated IMAP UID FETCH with header-only mode
- Implement `get_message` — full fetch with body + MIME parsing
- Implement `search_messages` — IMAP SEARCH + SQLite FTS5 hybrid
- Implement corresponding REST endpoints
- Wire up sync UID range fetch in `internal/sync/syncer.go`

**Iteration 3 (intelligence):**
- `summarize_folder`, `detect_subscriptions`, `get_sender_history`
- Sender profile builder on enrichment events
- KG entity extractor in enrichment pipeline
- `semantic_search` using stored vectors

**Iteration 4 (watch_folder + streaming):**
- IMAP IDLE support
- `GET /api/events` SSE stream
- Real-time enrichment on new messages

**Option 4 (future):**
- Autonomous rule engine (triggers actions on bus events)
- Federation (multi-instance KG sharing)
- Plugin system (external bus subscribers)

---

## Operating Rules

See `AGENT.md` for the full rule set.

**Prime rule: the user makes all decisions.**  
When any design decision is not covered by an existing rule, stop and run DIP.

**Always load this file before starting work in a new session.**
