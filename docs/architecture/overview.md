# imap-mcp Architecture Overview

**Version:** 0.1.0  
**Date:** 2026-05-31

---

## System Overview

imap-mcp is a Go binary that bridges IMAP email accounts to Claude via the MCP protocol,
exposes a REST API for algorithmic automation, and maintains a local intelligence layer
(SQLite + vectors + temporal KG) for fast, offline-capable mail analysis.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              imap-mcp binary                                │
│                                                                             │
│  ┌──────────┐    ┌──────────────────────────────────────────────────────┐  │
│  │  stdio   │    │              HTTP server (:8765)                     │  │
│  │ (Claude  │    │                                                      │  │
│  │  Code    │    │   /mcp  ──► MCP Streamable HTTP (mcp-go)            │  │
│  │  native) │    │   /api  ──► REST API (chi router)                   │  │
│  └────┬─────┘    │            /api/health  /api/accounts               │  │
│       │          │            /api/search  /api/senders                │  │
│       │          │            /api/kg      /api/anomalies              │  │
│       │          │            /api/rules   /api/webhooks               │  │
│       │          │            /api/query   /api/events (future SSE)    │  │
│       │          └──────────────────────────────────────────────────────┘  │
│       │                              │                                      │
│       └──────────┬───────────────────┘                                     │
│                  │                                                          │
│          ┌───────▼────────┐                                                │
│          │   MCP Tools    │  list_accounts, list_folders, list_messages    │
│          │   (28 tools)   │  search_messages, semantic_search              │
│          │                │  summarize_folder, detect_subscriptions        │
│          │                │  kg_query, get_anomalies, ...                  │
│          └───────┬────────┘                                                │
│                  │                                                          │
│          ┌───────▼────────┐                                                │
│          │   Event Bus    │  in-process pub/sub; all subsystems route here │
│          └─┬──────────────┘                                                │
│            │                                                               │
│   ┌────────┼───────────────────────────────────────────────────────┐       │
│   │        │                                                       │       │
│   ▼        ▼              ▼                   ▼                   ▼       │
│ IMAP    Sync          Enrichment          SQLite DB           Webhooks/   │
│ Pool    Engine        Pipeline            (cache +            Rules       │
│         (15m)         (qwen3:1.7b         vectors +           Engine      │
│                        nomic-embed)       KG + FTS5)          (future)    │
│   │                       │                   │                           │
│   └───── 3 accounts ──────┘                   │                           │
│          (all connected)                       └── Ollama :11434           │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Transport Modes

| Mode | Invocation | Use case |
|------|-----------|---------|
| **stdio** | `imap-mcp` (no args) | Claude Code native MCP — zero config |
| **HTTP** | `imap-mcp serve` | REST API + MCP over network; persistent IMAP connections |
| **auth-setup** | `imap-mcp auth-setup` | OAuth2 browser flow for Gmail/Outlook |

Claude Code `.claude/mcp.json` entry for stdio mode:
```json
{
  "mcpServers": {
    "imap": {
      "command": "/home/dmz/workspace/imap-mcp/imap-mcp",
      "args": [],
      "env": { "IMAP_MCP_PERSONAL_PASSWORD": "${IMAP_MCP_PERSONAL_PASSWORD}" }
    }
  }
}
```

For HTTP mode (persistent connections, recommended when using REST API):
```json
{
  "mcpServers": {
    "imap": {
      "url": "http://localhost:8765/mcp"
    }
  }
}
```

---

## Directory Structure

```
imap-mcp/
├── cmd/imap-mcp/main.go          Entry point; subcommand routing
├── internal/
│   ├── config/config.go          YAML + env config loader
│   ├── bus/bus.go                In-process event bus (all subsystems use this)
│   ├── imap/
│   │   ├── pool.go               Multi-account connection pool with reconnect
│   │   └── auth/
│   │       ├── auth.go           Authenticator interface
│   │       ├── plain.go          Plain username/password
│   │       └── xoauth2.go        XOAUTH2 (Gmail, Outlook) + auth-setup flow
│   ├── db/
│   │   ├── db.go                 SQLite connection + repository wiring
│   │   └── schema.go             Full schema: messages, vectors, senders, KG, anomalies
│   ├── enrichment/
│   │   ├── ollama.go             Ollama API client (embed + generate)
│   │   └── pipeline.go           Background enrichment worker
│   ├── sync/syncer.go            IMAP sync engine (background + on-demand)
│   ├── mcp/
│   │   ├── server.go             MCP server wiring (all 28 tools registered)
│   │   └── tools/
│   │       ├── handlers.go       Handlers struct (shared deps)
│   │       ├── definitions.go    All tool definitions (names, descriptions, params)
│   │       ├── impl_accounts.go  list_accounts, sync_account (implemented)
│   │       ├── impl_folders.go   list_folders, create_folder, delete_folder (implemented)
│   │       └── impl_stubs.go     All other tools (stub → iteration 2)
│   ├── api/server.go             REST API router (chi); /api/health implemented
│   └── server/server.go          Combined HTTP server (MCP at /mcp, REST at /api)
├── docs/
│   ├── architecture/overview.md  This file
│   └── plans/                    Implementation plan docs
├── AGENT.md                      Operating rules for Claude sessions
├── IMAP-MCP-CONTEXT.md           Full context loader (load at session start)
├── config.example.yaml           Annotated config reference
└── .env.example                  Environment variable template
```

---

## Data Model

### Messages Table
Core cache of all synced messages. Enriched with:
- `hall` — email type: `transactional|conversation|newsletter|notification|alert|personal`
- `wing` — project/context label (e.g. `imap-mcp`, `work-q1`)
- `room` — topic cluster (e.g. `deployment`, `billing`)
- FTS5 virtual table for full-text search over subject + body

### Vectors Table
One row per message: float32 embedding blob (768 dims, `nomic-embed-text`).
Similarity search runs in Go (cosine distance) at query time.

### Senders Table (entity profiles)
Per-address enriched profile: role, first/last contact, message count, avg reply time,
anomaly score. Sender is a first-class entity, not just a string.

### Knowledge Graph (kg_entities + kg_relationships)
Subject-predicate-object triples with `valid_from`/`valid_to` timestamps.
Examples:
- `alice@corp.com → manages → bob@corp.com`  
- `thread:X → belongs_to → project:imap-mcp`  
- `newsletter@acme.com → is_subscription → true`

### Anomalies Table
Episodic log of detected behavioral changes:
- `behavior_change` — sender role/pattern shifted
- `silence` — thread went quiet after activity
- `reply_spike` — unusual reply volume
- `new_sender` — first contact from a domain

---

## Enrichment Pipeline

```
Sync → messages.enrichment_status = 'pending'
     → enrichment_queue INSERT

Background (10s tick):
  SELECT pending FROM enrichment_queue LIMIT batch_size
  FOR EACH message:
    1. Ollama embed(nomic-embed-text, subject + body[:500])
       → INSERT message_vectors
    2. Ollama generate(qwen3:1.7b, classify prompt)
       → UPDATE messages SET hall, wing, room
    3. Bus.Publish(EventEnrichmentDone)
```

---

## Event Bus

All subsystems communicate through `internal/bus`. Current event types:

| Event | Published by | Consumed by (planned) |
|-------|-------------|----------------------|
| `message.synced` | Sync | Enrichment, Rules, Webhooks |
| `sync.complete` | Sync | API stats |
| `enrichment.done` | Pipeline | KG builder, Anomaly detector |
| `anomaly.detected` | Anomaly detector | Webhooks, Rules |
| `account.connected` | Pool | API stats |
| `rule.fired` | Rules engine | Webhooks, Audit log |

The event bus is the **primary extension point** for future capabilities:
- Autonomous agents subscribe to events and fire actions
- Federation layer publishes events to remote instances
- `/api/events` SSE endpoint streams events to external consumers
- Plugin system hooks into the bus without modifying core code

---

## MCP Tools (v0.1.0)

28 tools registered. Implemented: `list_accounts`, `sync_account`, `list_folders`,
`create_folder`, `delete_folder`. All others return "iteration 2" stub.

| Category | Tools |
|----------|-------|
| Accounts | `list_accounts`, `sync_account` |
| Folders | `list_folders`, `create_folder`, `delete_folder` |
| Read | `list_messages`, `get_message`, `get_thread`, `get_headers`, `get_attachments`, `export_message` |
| Write | `move_message`, `copy_message`, `delete_message`, `set_flags`, `append_message`, `move_bulk`, `flag_bulk` |
| Search | `search_messages`, `cross_account_search`, `semantic_search` |
| Intelligence | `summarize_folder`, `detect_subscriptions`, `get_sender_history`, `get_sender_profile`, `kg_query`, `get_anomalies`, `enrichment_status`, `trigger_enrichment` |

---

## REST API (v0.1.0)

Implemented: `GET /api/health`, `GET /api/accounts`.  
All other routes return `501 Not Implemented` until iteration 2.

Full route table in `internal/api/server.go`.

---

## Future: Option 4

The architecture is designed to support without major refactoring:

- **Autonomous agents** — rule engine (schema exists in `rules` table) triggers actions
  on bus events without human input
- **Federation** — a `federation` package subscribes to local events and publishes to
  remote imap-mcp instances; KG and enrichment results can be shared across instances
- **Streaming event bus** — `GET /api/events` SSE endpoint streams `bus.Event` as JSON
  to external consumers (scripts, datawatch sessions, dashboards)
- **Plugin system** — a `plugins` package loads external binaries/scripts that register
  bus handlers; plugins declare their event subscriptions in a manifest
