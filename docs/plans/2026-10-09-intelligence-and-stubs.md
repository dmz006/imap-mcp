# Plan: intelligence builders and the remaining stub tools

- **Date:** 2026-10-09
- **Starting version:** 0.10.5
- **Status:** Planned. Decisions D19–D26 are open; they are put to the operator
  one at a time (AGENT.md DIP) before the phase that needs them starts.

## Scope

Make the seven tools that currently return nothing do real work.

| Tool | Today | Goal |
|------|-------|------|
| `get_sender_profile` | Reads `senders`, which nothing fills | Profiles built from mail history |
| `kg_query` | Reads `kg_entities` / `kg_relationships`, which nothing fills | A knowledge graph of people, organisations, threads and subscriptions |
| `get_anomalies` | Reads `anomalies`, which nothing fills; `anomaly.detected` never fires | A detector that records anomalies and publishes the event |
| `get_thread` | Stub | Return a conversation |
| `get_attachments` | Stub | List attachments, and fetch one |
| `export_message` | Stub | Export a message as `.eml` |
| `cross_account_search` | Stub | One search across every account |

The REST routes and `/api/query` views that read the same tables
(`/api/senders`, `/api/kg`, `/api/anomalies`, views `senders`, `kg`,
`anomalies`) start returning data as a side effect, so they need no new routes.

Out of scope:
- content cleaning (D7-D);
- IMAP IDLE;
- federation of intelligence data.

New components must still follow the Event Bus and Architecture-for-Option-4
rules: builders subscribe to bus events and sit behind interfaces, so a
plugin or another classifier can replace them later.

## What exists already

- **Schema.** `senders`, `kg_entities`, `kg_relationships` (with
  `valid_from`/`valid_to`) and `anomalies` are defined in `cache.db`. Read paths
  for all three (`GetSenderProfile`, `KGQuery`, `Anomalies`) are implemented and
  tested.
- **Threads.** Sync stores `thread_id` (the root of `References`, else
  `In-Reply-To`, else the message's own Message-ID). The live `list_messages`
  output builds its `thread_id` differently (it joins `In-Reply-To`), so the
  two don't match today.
- **Attachments.** Sync stores `has_attachments` and an `attachments` JSON list
  (name, MIME type, size) per cached message. Content is not stored.
- **Classification.** Enrichment tags each message with `hall` (conversation,
  newsletter, transactional, notification, alert, personal), `wing` and `room`.
- **Sent mail.** `\Sent` is in the default sync folders, so replies (and so
  reply times) are in the cache.
- **Bus.** `anomaly.detected` is declared but never published.
- **Cache lifetime.** `cache.db` is disposable (D1b): a schema-version change
  drops and recreates it, and the window purge deletes rows older than the sync
  window (default 30 days).

That last point is the main design problem. Profiles, a graph and anomaly
baselines all need more history than a 30-day, wipe-on-upgrade cache holds.

## Open decisions

Each one is asked separately, with options and a recommendation, before the
phase that needs it.

| # | Decision | Blocks | Status |
|---|----------|--------|--------|
| D19 | Where intelligence data lives and what history it is built from (cache only / persistent store / one-off historical scan) | P2–P4 | Open |
| D20 | How sender profiles are built: header statistics only, or with an LLM-assigned role | P2 | Open |
| D21 | How the knowledge graph is extracted: deterministic from headers, LLM entity extraction from bodies, or both | P3 | Open |
| D22 | Which anomaly types to detect first, how they are triggered, and whether security signals (auth failures, lookalike domains, first-time sender asking for payment) are included | P4 | Open |
| D23 | `get_thread`: cache `thread_id` only, live IMAP (`THREAD` extension, Gmail `X-GM-THRID`), or cache with live fallback; and fixing the mismatched `list_messages` `thread_id` | P1 | Open |
| D24 | `get_attachments`: where fetched content goes (working-dir sandbox, inline base64, or both with a size cap) and any MIME-type limits | P1 | Open |
| D25 | `export_message`: return the `.eml` inline, write it to the working-dir sandbox, or both; single message only or a batch/mbox option | P1 | Open |
| D26 | `cross_account_search`: live IMAP SEARCH fanned out to every account, or the cache's full-text index across accounts, or both | P1 | Open |

## Phases

The stub tools come first: they are small, independent, and each one is
useful on its own. The intelligence builders follow, in dependency order:
anomaly baselines need profiles, and the graph uses profile roles.

| Phase | Version | Content | Needs | Status |
|-------|---------|---------|-------|--------|
| P1 | 0.11.0 | `get_thread`, `get_attachments`, `export_message`, `cross_account_search`, plus REST routes for any that lack one; `list_messages` `thread_id` made consistent with the cache | D23–D26 | Planned |
| P2 | 0.12.0 | Sender-profile builder: subscribes to sync/enrichment events, fills `senders` (counts, first/last seen, sent-to count, average reply time from Sent, role). Initial build over existing data | D19, D20 | Planned |
| P3 | 0.13.0 | Knowledge-graph builder: entities and relationships, with `valid_from`/`valid_to`. Initial build over existing data | D19, D21 | Planned |
| P4 | 0.14.0 | Anomaly detector: compares new mail with profile baselines, writes `anomalies`, publishes `anomaly.detected` (and so webhooks and SSE). Resolution through MCP and REST | D19, D22 | Planned |
| P5 | after P4 | Docs and skill: examples.md "Not there yet" list removed or reduced, new agent workflows that use profiles, graph and anomalies, companion skill update and community PR, known-limitations and context file updated | P1–P4 | Planned |

Every phase follows the usual rules:
- tests for all new logic (Tested=Yes);
- a live check against a real account (Validated=Yes), with counts and status
  codes only, never mailbox content;
- CHANGELOG, `config.example.yaml` for any new setting, and the context file;
- a version bump before push;
- a database backup before any migration.

## Constraints carried from earlier decisions

- Reads never set `\Seen` (`BODY.PEEK`).
- Mailbox-derived output goes only to the working-dir sandbox, never the repo.
- Webhook payloads carry identifiers and counts, not addresses, subjects or
  bodies. `anomaly.detected` follows the same rule.
- Any LLM use goes through the existing enrichment providers and gates
  (rate limits, quiet hours, GPU yield, datawatch capacity).
- No hard-coded configuration: every new threshold and switch is a config
  field with an `IMAP_MCP_*` override and a health or stats entry.
