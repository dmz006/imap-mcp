# Plan: intelligence builders and the remaining stub tools

- **Date:** 2026-10-09
- **Starting version:** 0.10.5
- **Status:** P1 (0.11.0), P2 (0.12.0) and P3 (0.13.0) done; P4 next. Decisions D19–D28 decided 2026-10-09 (DIP, one at a
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
| D28 | Per-message index | P2–P4 | **Decided** 2026-10-09: hashed index in `imap.db`: one row per message (Message-ID hash, date, sender id, direction, In-Reply-To hash), no subjects/bodies/addresses; exact de-dup across folders/labels and full-history reply pairing |
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
| P2 | 0.12.0 | State migration moving the intelligence tables to `imap.db`; header backfill (resumable, rate-limited). Sender-profile builder: subscribes to sync/enrichment events, fills `senders` (counts, first/last seen, sent-to count, average reply time from Sent, role). Initial build over existing data | D19, D20 | **Done (0.12.0)**: Tested=Yes (header parsing, hashing, roles, scope, end-to-end scan against the in-memory server incl. duplicates across folders, reply pairing, hall and model roles, gated model, PEEK/`\Seen` checks, UIDVALIDITY rescan, mid-scan role refresh, progress registration, table move without cache rebuild; mutation-checked). Validated=Yes on a side instance against two live accounts (one with an All Mail folder, one with several hundred folders): full first scan with two mid-scan restarts and no double counting (incoming index rows = sum of received counts), replies paired, roles from signals, DKIM/DMARC results captured, REST/query views and scopes, cache tables dropped without rebuild. Found and fixed during validation: roles only at tick end, progress before a folder's first batch, read-to-write transaction upgrade |
| P3 | 0.13.0 | Knowledge-graph builder: deterministic entities and relationships with `valid_from`/`valid_to`, initial build over backfilled data; then the gated LLM body-extraction lane for recent conversation/personal mail | D19, D21 | **Done (0.13.0)**: Tested=Yes (edge rules incl. webmail, cc cap, subscriptions, threads; exactly-once via kg_done across copies, ticks and the upgrade rescan; tag and model edges once per message; untrusted model output filtered; staleness; in-place state migration; mutation-checked). Validated=Yes on a side instance over copies of the production databases (encrypted): in-place migration and rescan of all history built header edges for every indexed message exactly once with profile counts unchanged; tag and model edges from real cached mail; model roles. The real classify model was probed with a synthetic example.com email. Found and fixed: model-role candidates without cached mail starved the pass; deadline date in the wrong field; placeholder names |
| P4 | 0.14.0 | Anomaly detector: compares new mail with profile baselines, writes `anomalies`, publishes `anomaly.detected` (and so webhooks and SSE). Resolution through MCP and REST | D19, D22 | Planned |
| P5 | after P4 | Docs and skill: examples.md "Not there yet" list removed or reduced, new agent workflows that use profiles, graph and anomalies, companion skill update and community PR, known-limitations and context file updated | P1–P4 | Planned |

Every phase follows the usual rules:
- tests for all new logic (Tested=Yes);
- a live check against a real account (Validated=Yes), with counts and status
  codes only, never mailbox content;
- CHANGELOG, `config.example.yaml` for any new setting, and the context file;
- a version bump before push;
- a database backup before any migration.

## P2 design (sender profiles)

- **Storage.** `senders`, `kg_entities`, `kg_relationships` and `anomalies`
  are created in `imap.db`; the empty copies in `cache.db` are dropped at
  open. The cache schema version is not bumped, so the cache is kept, not
  rebuilt. New state tables:
  - `intel_messages`: the D28 index;
  - `intel_scan`: per account and folder, the UIDVALIDITY, the last UID
    scanned, and the time the folder was first completed.

  `get_sender_profile`, `kg_query`, `get_anomalies`, their REST routes and
  the `/api/query` views `senders`/`kg`/`anomalies` read `imap.db`.
- **Header scanner** (`internal/intel`, behind an interface; runs where the
  syncer runs).
  - **Each tick:** for every account and folder, it fetches UIDs above the
    last one scanned, in batches. Each fetch takes the envelope, INTERNALDATE
    and `BODY.PEEK[HEADER.FIELDS (List-Id List-Unsubscribe Precedence
    Auto-Submitted Authentication-Results)]`, so it never fetches bodies or
    sets `\Seen`.
  - **Folder scope:** the `\All` mailbox when the server has one (Gmail);
    otherwise every selectable folder except `\Junk`, `\Drafts` and any
    `intelligence.exclude_folders`.
  - **Rate:** limited to `intelligence.backfill_per_minute`.
  - **Resumable:** progress and aggregates commit in one transaction per
    batch, so a crash never double-counts. A UIDVALIDITY change rescans the
    folder, and the D28 index makes that safe.
- **Profiles.** Incoming mail updates the sender's name, domain, first and
  last seen, message count, and the list, bulk, auto-submitted and
  DKIM/DMARC pass/fail counts; the auth counts serve as P4 baselines.
  Outgoing mail updates each recipient's `sent_count`. After each batch,
  outgoing replies are paired through the In-Reply-To hash with the incoming
  message they answer, which updates that sender's reply count and average
  reply time.
- **Roles (D20).** Signals first; then the majority of the cache's
  classification tags; then, for senders still `unknown` that have cached
  mail, the classify model with a few recent subjects. The model is called a
  few senders per tick, only when the enrichment backfill gates allow it.
  The source of each role is recorded.
- **Config.** An `intelligence:` block with `IMAP_MCP_INTELLIGENCE_*`
  overrides:
  - `enabled`;
  - `scan_interval_minutes`;
  - `backfill_per_minute`;
  - `batch_size`;
  - `exclude_folders`;
  - `llm_roles`;
  - `llm_roles_per_tick`.

  `/api/health` shows the scan progress.

## P3 design (knowledge graph)

- **Entities.**
  - `person`: every address, including each account's own address, which
    stands for the mailbox owner.
  - `organization`: a sender's domain, except shared webmail domains.
  - `thread`: a References root, for conversation mail only, meaning mail
    with no list, bulk or auto-submitted header.
  - `project` and `topic`: from the cache's `wing` and `room` tags.
- **Deterministic edges** (confidence 1.0). Each edge keeps a `weight`
  (number of messages), `valid_from` (first evidence) and `valid_to`. A
  relationship with no evidence for `intelligence.kg_stale_days` (default
  365) gets `valid_to` set to its last evidence; new evidence clears it
  again. The edges:
  - `belongs_to`: person → organization.
  - `corresponds_with`: other person → mailbox owner (either direction of
    mail).
  - `cc_with`: two people in the same message, owner excluded. Stored once
    per pair, only for messages with at most 8 participants, so mailing lists
    don't produce a clique.
  - `is_subscription`: list or bulk sender → mailbox owner.
  - `participates_in`: person → thread (conversation mail only).
  - `works_on`: sender → project, from the cache's `wing` tag.
  - `discusses`: sender → topic, from the cache's `room` tag.
- **Built from the header scan, exactly once per message.** The scan now
  also fetches `References`. Per-message participants are never stored
  (D28). Instead, the D28 index row gains a `kg_done` flag. The first P3
  start resets the scan progress, so every folder is read once more. Profile
  counts skip messages already indexed, as before, while graph edges are
  built for every row with `kg_done = 0`, which is then set. Later ticks do
  both in one pass.
- **From the cache.** `works_on` and `discusses` come from cached, enriched
  mail, recorded once per message through a `kg_tags_done` flag on the same
  index row.
- **Model extraction (D21).** For cached conversation and personal mail, the
  classify model reads the sender, date, subject and up to 1,500 characters
  of body. Quoted replies are stripped first. It returns relations from a
  fixed set:
  - `manages`, `reports_to` (person → person);
  - `works_at` (person → organization);
  - `works_on` (person → project);
  - `deadline` (thread → topic, with the due date in `properties`).

  Anything else in the answer is ignored, and names are trimmed and
  length-capped. The body is untrusted input; the output can only add
  edges, never cause an action. These edges have confidence 0.6. The model
  runs through `ClassifyWhenIdle` (the enrichment gates), handles
  `intelligence.kg_llm_per_tick` messages per tick, and processes each
  message once (`kg_llm_done` flag). It can be turned off with
  `intelligence.kg_llm`.
- **Schema.** `kg_relationships` gains `weight`, and a unique index on
  (subject, predicate, object) for upserts. `intel_messages` gains the
  three flags. Columns are added to an existing `imap.db` in place (backup
  first).
- **Reads.** `kg_query`, `GET /api/kg`, the `kg` query view and a profile's
  `relationships` return data. `/api/health` adds entity and relationship
  counts.

## Constraints carried from earlier decisions

- Reads never set `\Seen` (`BODY.PEEK`).
- Mailbox-derived output goes only to the working-dir sandbox, never the repo.
- Webhook payloads carry identifiers and counts, not addresses, subjects or
  bodies. `anomaly.detected` follows the same rule.
- Any LLM use goes through the existing enrichment providers and gates
  (rate limits, quiet hours, GPU yield, datawatch capacity).
- No hard-coded configuration: every new threshold and switch is a config
  field with an `IMAP_MCP_*` override and a health or stats entry.
