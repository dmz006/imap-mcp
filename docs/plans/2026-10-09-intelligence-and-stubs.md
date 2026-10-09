# Plan: intelligence builders and the remaining stub tools

- **Date:** 2026-10-09
- **Starting version:** 0.10.5
- **Status:** P1 done (0.11.0); P2 next. Decisions D19–D27 decided 2026-10-09 (DIP, one at a
  time); see the table below and AGENT.md § Recorded Decisions.

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

## Decisions

All decided 2026-10-09, one question at a time.

| # | Decision | Blocks | Status |
|---|----------|--------|--------|
| D27 | REST shape for P1 | P1 | **Decided** 2026-10-09: read routes `GET /api/threads/{thread_id}`, `GET …/messages/{uid}/attachments`, `GET /api/search/cross`; content downloads `GET …/attachments/{part}`, `GET …/messages/{uid}/export.eml`, `POST /api/export` (.mbox) stream bytes as `application/octet-stream` with a safe filename, write nothing server-side, and need the `write` scope like MCP |
| D19 | Intelligence store and history | P2–P4 | **Decided** 2026-10-09: tables move to `imap.db` (state migration, backup first); seeded by a one-off resumable, rate-limited, header-only backfill of all folders (never bodies, PEEK), then updated incrementally as mail syncs. The cache also feeds the builders with enriched signals for recent mail (classification tags, embeddings, bodies, attachment metadata); derived results are persisted in `imap.db` so they outlive the window. `/api/query` views `senders`/`kg`/`anomalies` read from `imap.db` |
| D20 | Sender roles | P2 | **Decided** 2026-10-09: counts from headers (first/last seen, received, sent-to, avg reply time from Sent). Role from signals first (List-Id/List-Unsubscribe/Precedence → newsletter; noreply/Auto-Submitted → bot; sent-to → personal, or colleague on the account's domain; else majority cached classification tag). Only remaining `unknown` senders go to the classify LLM through the enrichment gates. Backfill fetches those header fields (`HEADER.FIELDS`, PEEK) |
| D21 | Knowledge-graph source | P3 | **Decided** 2026-10-09: both. Deterministic: people, organizations (domain), threads, subscriptions from headers (edges `belongs_to`, `corresponds_with`, `cc_with`, `is_subscription`, `participates_in`); projects/topics from wing/room tags. Plus LLM extraction from cached bodies of recent conversation/personal mail (`manages`, `works_on`, mentioned organizations, deadlines) via the configured classify model and enrichment gates (bodies go nowhere else); LLM edges stored with confidence < 1.0 |
| D22 | Anomaly set | P4 | **Decided** 2026-10-09: behavior + security. Per message: `new_sender` (conversation/personal only), `auth_failure` (known sender's DKIM/DMARC pass → fail), `lookalike_domain`, `reply_to_mismatch`. Periodic: `silence`, `volume_spike`. Configurable thresholds; `anomaly.detected` with IDs only; sync and backfill capture `Authentication-Results` and `Reply-To`. LLM "asks for payment" → backlog |
| D23 | `get_thread` source | P1 | **Decided** 2026-10-09: cache first (all cached folders incl. Sent, by date), live IMAP fallback by Message-ID/References (Gmail `X-GM-THRID`) when the thread reaches outside the window or isn't cached. `list_messages` uses the sync `thread_id` derivation |
| D24 | `get_attachments` content | P1 | **Decided** 2026-10-09: list = metadata (read scope, cache or live BODYSTRUCTURE); fetch saves the part to the working-dir sandbox (write scope); `text/*` under a configurable cap also returned inline, decoded; binary never inline |
| D25 | `export_message` shape | P1 | **Decided** 2026-10-09: sandbox only (write scope). Single message → `.eml` (raw, PEEK). Batch → one `.mbox`, selected by UID list, `thread_id` (D23 lookup incl. live fallback) or sender; configurable message and byte caps |
| D26 | `cross_account_search` source | P1 | **Decided** 2026-10-09: cache FTS across all accounts and cached folders by default, merged by date; `live: true` fans out IMAP SEARCH in parallel per account (`folder`, default INBOX) for full history; results carry their source; per-account errors don't fail the call |

## Phases

The stub tools come first: they are small, independent, and each one is
useful on its own. The intelligence builders follow, in dependency order:
anomaly baselines need profiles, and the graph uses profile roles.

| Phase | Version | Content | Needs | Status |
|-------|---------|---------|-------|--------|
| P1 | 0.11.0 | `get_thread`, `get_attachments`, `export_message`, `cross_account_search`, plus REST routes (D27); `list_messages` `thread_id` made consistent with the cache | D23–D27 | **Done (0.11.0)**: Tested=Yes (service tests against the in-memory IMAP server incl. live fallback, PEEK checks, mboxrd quoting, caps, FTS-literal input, per-account errors; MCP scope and sandbox tests; REST route and 403 tests). Validated=Yes on a side instance against two live accounts: threads from cache and with live fallback, attachment list/download (octet-stream, nosniff, read token 403), `.eml` byte-exact, thread `.mbox` counts match, cross-account search live and cache, server UNSEEN counts unchanged, MCP tool lists per scope |
| P2 | 0.12.0 | State migration moving the intelligence tables to `imap.db`; header backfill (resumable, rate-limited). Sender-profile builder: subscribes to sync/enrichment events, fills `senders` (counts, first/last seen, sent-to count, average reply time from Sent, role). Initial build over existing data | D19, D20 | Planned |
| P3 | 0.13.0 | Knowledge-graph builder: deterministic entities and relationships with `valid_from`/`valid_to`, initial build over backfilled data; then the gated LLM body-extraction lane for recent conversation/personal mail | D19, D21 | Planned |
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
