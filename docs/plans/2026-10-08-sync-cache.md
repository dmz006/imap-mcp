# Iteration 2 — Sync cache (windowed, optionally encrypted)

| Field | Value |
|-------|-------|
| Date | 2026-10-08 |
| Target version | 0.6.0 → 0.10.0, one minor per phase (D12) |
| Status | **In progress:** P0–P5 (0.5.3–0.10.0) built and validated. Next: P6 (docs/skills per release) and P7 (full docs pass) |
| Supersedes | "Iteration 2 (message CRUD)" bullets in `IMAP-MCP-CONTEXT.md` / `README.md` roadmap |

## Current status (2026-10-08)

- Decided: D1, D1a, D1b, D5–D10, D11a, D11b, D12, D13 (D2–D4 resolved by D1).
  Each is recorded as a rule in `AGENT.md` § Recorded Decisions.
- D13a decided: named, scoped tokens; D13a-1: ships first as security release
  0.5.3 (P0); D13a-2: mandatory with explicit opt-out. **Next:** P0, coordinated
  with the datawatch agent.
- Then implement P1 (0.6.0, storage) per the phase table and AGENT.md release
  rules.

## Problem

`internal/sync/syncer.go` `syncFolder` is a scaffold: it SELECTs the folder and stamps
`sync_state.last_synced` but fetches nothing, so `messages` stays empty. Everything
downstream (enrichment, FTS, `semantic_search`, sender profiles, KG, anomalies) is
starved. Live tools and the rules engine query IMAP directly and are unaffected.

## Requirements (operator-decided, 2026-10-08)

- **R1 Folders** — sync INBOX and Sent by default; operator can select other folders.
- **R2 Window** — rolling 30-day cache, adjustable. Changing it purges or backfills
  to match.
- **R3 Encryption option** — at-rest encryption of the cache, operator-selectable.
  The cache stores headers, bodies, vectors, and enrichment details **in both modes**.
  Encryption is a security setting, not a feature gate. (Clarified by the operator
  2026-10-08 at D6.)
- **R4 Cleaning option** — modeled on datawatch's cleaning.
- **R5 Load handling** — investigate. Use the datawatch compute nodes, MCP/service,
  skills, and existing integrations where they help.
- **R6** Intelligence (iteration 3) iterates in parallel. Not in this plan.

## Findings that shape the design

**Volume.** On the operator's accounts the 30-day window holds hundreds of messages
per account, about 10² per account (exact figures kept out of the repo per AGENT.md).
Average sizes run from tens of KB (INBOX) to around 1 MB (Sent, attachments). Backfill
at 30 days is minutes of GPU work. Load handling matters when the window grows (365
days is about 10× that) or folders are added.

**Server capabilities.** Dovecot advertises CONDSTORE and QRESYNC. Gmail advertises
CONDSTORE only. Both advertise SPECIAL-USE, so Sent resolves via `\Sent` (Dovecot
`Sent`, Gmail `[Gmail]/Sent Mail`).

**datawatch encryption pattern** (`internal/memory`, `internal/secfile`):
- Field-level XChaCha20-Poly1305, stored as `ENC:` + base64(nonce‖ct). Plaintext
  passes through, so mixed rows work and migration can be incremental.
- The SQLite file itself is not encrypted, and there is no SQLCipher.
- Embeddings and metadata stay plaintext, and there is no FTS over encrypted fields.
- Key: Argon2id(passphrase from env) or a 0600 keyfile.
- Rotation helpers exist but are not wired.
- **License: datawatch is PolyForm Noncommercial and imap-mcp is MIT, so we
  reimplement the pattern on `golang.org/x/crypto` (BSD) and copy no code.**

**datawatch "cleaning"** is retention only, with no content scrubbing:
- Age-based prune with per-kind retention days (configured but never scheduled).
- `memory_sweep_stale`: on demand, `dry_run` defaults to true, counts candidates
  before deleting, pinned rows exempt.
- `tooling_cleanup` removes AI-tool temp files and is unrelated.
- Nothing redacts PII or secrets from stored text.

**Compute and datawatch integration:**
- Two nodes: a local GPU node (Ollama) and the datawatch node (Ollama +
  OpenAI-compatible endpoint).
- The recommendation is to keep `nomic-embed-text` on the local
  node, where the GPU is mostly idle for embeddings, and `qwen3:1.7b` as the
  fast-small classifier. Qwen3-4B-Instruct-2507 is the upgrade candidate, pending
  its benchmark plan.
- datawatch exposes `POST /api/proxy/llm/<name>` (generation only, not embeddings),
  which brings LLM-registry routing and node failover.
- datawatch also offers a durable work queue (`queue_push/claim`), `capacity_status`,
  `ollama_stats`, and secrets (`${secret:name}`, already supported by imap-mcp).

**Dependencies.**
- `emersion/go-message` (MIT) is already an indirect dependency via go-imap v2.
  Using it directly for MIME decoding adds nothing new.
- `golang.org/x/crypto` would be new (BSD). Note it in the commit per the dependency
  rules.

## Proposed design (subject to the decisions below)

```
sync loop (per account, per selected folder, every interval)
  ├─ resolve folder (special-use \Sent etc.) → SELECT (CONDSTORE if available)
  ├─ UIDVALIDITY changed? → drop folder cache, re-backfill
  ├─ window = now - window_days
  ├─ UID SEARCH SINCE window          → server UID set S
  ├─ cached UID set C (in window)
  ├─ new = S - C   → FETCH envelope/flags/size/BODYSTRUCTURE [+ body if encrypted]
  ├─ gone = C - S  → delete cache rows (expunged, moved, or aged out)
  ├─ flags: CHANGEDSINCE modseq (CONDSTORE) else FETCH FLAGS for S
  └─ publish message.synced / message.updated / message.deleted on the bus
retention sweep (after each cycle, or on window change)
  └─ delete rows older than window (cache only, NEVER the mailbox); dry_run supported
enrichment pipeline (existing)
  └─ provider interface → local Ollama / datawatch proxy; throttled, new-mail first
```

## Decisions

Each decision is raised one at a time (DIP). Its status and the operator's choice
are recorded here, and the resulting rule goes into `AGENT.md`.

| # | Decision | Recommendation (pending operator) | Status |
|---|----------|-----------------------------------|--------|
| D1 | Encryption mechanism | Recommended field-level; **operator chose whole-DB encryption (SQLCipher-style)** 2026-10-08 | **Decided** |
| D1a | Library for whole-DB encryption | **`github.com/ncruces/go-sqlite3` + `vfs/adiantum`** (pure Go, MIT, Argon2id, FTS5 via `ext/fts5`). Replaces `modernc.org/sqlite`; one-time migration of the existing DB. Accepted trade-offs: not SQLCipher file format; deterministic block encryption | **Decided** 2026-10-08 |
| D1b | One DB file or split cache/state | **Split:** `cache.db` (messages, vectors, FTS, senders, KG, anomalies, enrichment queue, sync state; disposable, rebuildable from IMAP) and `imap.db` (rules, webhooks, inbound nonces; not rebuildable). **Each file independently supports encryption, operator-selectable, off by default unless enabled.** run-rules needs a key only if `imap.db` is encrypted | **Decided** 2026-10-08 |
| D2 | Which fields are encrypted | Moot under D1 (whole-DB: all columns, FTS and vectors are inside the encrypted file) | **Resolved by D1** |
| D3 | Search over encrypted content | Moot under D1 (whole-DB: all columns, FTS and vectors are inside the encrypted file) | **Resolved by D1** |
| D4 | Embeddings when encrypted | Moot under D1 (whole-DB: all columns, FTS and vectors are inside the encrypted file) | **Resolved by D1** |
| D5 | Key source | **Per-file `encryption.key` = `${secret:name}` (datawatch secrets) or `${ENV}` passphrase → Argon2id (adiantum).** Never auto-generate; fail closed (refuse to open) if the key is missing or wrong. Both files may reference the same secret. systemd unit gains `After=`/`Wants=datawatch.service` when a `${secret:}` key is used. TPM-sealed systemd creds = possible later add-on | **Decided** 2026-10-08 |
| D6 | Content in unencrypted mode | **Same as encrypted: headers + bodies + vectors + enrichment details.** Security posture is the operator's configuration (D1b/D5) | **Decided** 2026-10-08 |
| D7 | Meaning of "cleaning" | **Iteration 2:** (A) automatic window purge + orphan cleanup (vectors/FTS/queue/sender links) + VACUUM/WAL checkpoint, cache-only; (B) on-demand `cache_sweep` MCP+REST tool, `dry_run` default true, counts per account/folder before deleting, sweep by age/account/folder/error state or full rebuild; (C) configurable `\Flagged` exemption (default off) keeps flagged mail cached past the window. **Iteration 3:** (D) content cleaning before enrichment (HTML→text, strip quoted replies/signatures/tracking pixels, redact OTP/card numbers) — planned here, see §Content cleaning | **Decided** 2026-10-08 |
| D8 | Window semantics | **IMAP INTERNALDATE** (server arrival; matches `UID SEARCH SINCE`). `sync.window_days` (default 30) with per-account and per-folder overrides. Shrink → purge on next cycle; grow → background backfill of older mail, new mail always prioritised. Cache-only, never the mailbox | **Decided** 2026-10-08 |
| D9 | Change detection | **Per cycle: `UID SEARCH SINCE <window>` → diff vs cached UIDs (new / gone). Flags via `FETCH CHANGEDSINCE <modseq>` where CONDSTORE is advertised, else re-fetch FLAGS for the window. UIDVALIDITY change → rebuild that folder.** One code path for Dovecot and Gmail. QRESYNC = possible later optimisation behind the same interface; IDLE in iteration 4 | **Decided** 2026-10-08 |
| D10 | Folder selection | **Config only:** `sync.folders` default `[INBOX, "\\Sent"]`; `accounts[].sync.folders` replaces it per account; entries are SPECIAL-USE tokens (`\Sent`, `\Archive`, `\Drafts`, `\Junk`, `\All`, `\Flagged`) or literal names. One cache row per (folder, UID); enrichment de-duplicated by `Message-ID`; warn when `\All` is selected. Runtime folder tools = possible later add-on | **Decided** 2026-10-08 |
| D11a | LLM routing | **Provider interface per call type.** `embed`: direct Ollama URL (either node; datawatch proxy has no embeddings). `classify`: `ollama` (direct, default) or `datawatch` (`POST /api/proxy/llm/<registry-llm-name>`, token via existing `datawatch:` block / `${secret:}`, needs `sessions:input` cap) for registry routing + failover. Models: keep nomic-embed-text + qwen3:1.7b; Qwen3-4B-Instruct-2507 upgrade only after the llm-research benchmark plan | **Decided** 2026-10-08 |
| D11b | Load handling | **(1) Priority + caps:** two lanes (new mail always before backfill), per-provider concurrency cap (default 2), backfill rate limit, exponential backoff on errors/503, queue depth/rate/lag in `/api/health`. **(2) Yield to datawatch:** before each backfill batch read datawatch's capacity ledger + Ollama `/api/ps`; pause backfill when the target node's pool is held/has waiters or a large model is resident; never pauses new mail; degrades to (1) if datawatch is unreachable. **(3) Quiet hours:** optional backfill window (unset = no restriction) | **Decided** 2026-10-08 |
| D12 | Phase order + release cadence | **Storage first, one minor release per phase:** 0.6.0 storage → 0.7.0 sync → 0.8.0 cleaning → 0.9.0 load/enrichment → 0.10.0 REST. Each live-validated, CHANGELOG'd, pushed before the next | **Decided** 2026-10-08 |
| D13 | REST surface | Recommended MCP parity via shared service layer; **operator chose full platform** 2026-10-08: tool logic moves to an interface-based service layer used by both MCP and REST; all 25 stub routes implemented (reads, mailbox writes, rules CRUD/test, enrichment trigger) **plus** webhook delivery and the `/api/query` DSL. Webhook delivery and DSL design are raised as their own DIPs before P5 work on them starts. Write routes require the auth decided in D13a | **Decided** 2026-10-08 |
| D13a | Local auth for `/api` and `/mcp` | **Named tokens with scopes** (`server.auth.tokens: [{name, token: ${secret:…}/${ENV}, scopes}]`; scopes `read`, `write`, `send`, `admin`); every `/api` route and MCP tool maps to a required scope, checked once in middleware, constant-time compare; `/api/health` open; token name logged, value never. Operator directive: provision datawatch secrets for the local agent so the scheduled jobs and datawatch's `imap_mcp` backend keep working; datawatch-side code/config changes are coordinated with the datawatch agent (it owns datawatch), never made directly | **Decided** 2026-10-08 |
| D13a-1 | Auth release timing | **Separate security release 0.5.3 before P1** (v0.5.2 precedent); D12 numbering unchanged | **Decided** 2026-10-08 |
| D13a-2 | Auth enforcement | **Mandatory with explicit insecure opt-out:** `serve` refuses to start without at least one token unless `server.auth.disabled: true` (startup warning every time, `auth: disabled` in `/api/health`). An unresolvable token reference always fails closed, opt-out or not. stdio mode unaffected. Rollout: datawatch backend sends the header first, then imap-mcp enforces | **Decided** 2026-10-08 |
| D14 | Enabling encryption on an existing plaintext DB | **Explicit `imap-mcp db encrypt` command, plaintext removed after verification:** never automatic at startup. Refuses while any other process holds the file (stop the service first). Writes an encrypted copy, verifies it (opens with the key, integrity check, every table's row count and content digest identical), atomically replaces the original, then lists every other plaintext copy it can find (e.g. `*.bak-*`) for the operator to remove; it does not delete those itself | **Decided** 2026-10-09 |
| D15 | How imap-mcp reads datawatch secrets | **External-service endpoint only:** `${secret:name}` resolves via `GET /api/external/secrets/{name}` (datawatch ≥ v8.75.0) with a per-service token the operator mints in a real terminal (`datawatch secrets mint-service-token imap-mcp`), supplied to imap-mcp via `${ENV}` (never in a file in git). Secrets are scoped `service:imap-mcp`. No fallback to `/api/agents/secrets/`; `serve` and `run-rules` share this one path | **Decided** 2026-10-09 |
| D15a | Trusting datawatch's self-signed TLS certificate | **Pin it with `datawatch.ca_file`:** imap-mcp adds that certificate (PEM) to the system roots for every call it makes to datawatch (secrets, capacity gate, LLM proxy). Verification is never disabled; an unreadable file or one without a certificate fails closed at startup | **Decided** 2026-10-09; built with D15, Tested=Yes, Validated against live datawatch v8.75.0 (pinned cert verifies, bogus token → actionable 401; unpinned → x509 error; missing ca_file → startup refused) |
| D16 | Webhook delivery | **Durable outbox, metadata-only payload:** deliveries persist in `imap.db` (`webhook_deliveries`) and are retried with backoff across restarts (at-least-once, unique delivery id for receiver dedup). Payload carries identifiers only (event type, account, folder, UID, cache/rule ids, counts, flags), never subject, sender or body; receivers fetch details over REST with their own scoped token. Each request is HMAC-SHA256 signed with a per-webhook secret generated by imap-mcp and shown once. URLs must be https, or http to loopback only; redirects are not followed. Registration is `admin`-scoped. Repeatedly failing webhooks are auto-disabled and can be re-enabled | **Decided** 2026-10-09 |
| D17 | `/api/query` | **Structured JSON query over fixed views:** a query names a view (`messages`, `senders`, `anomalies`, `kg`), filters on allowlisted fields (eq/in/range/contains), optional `group_by` + aggregate (count/min/max/sum), sort and limit; imap-mcp compiles it to parameterized SQL. Message bodies are excluded unless a query names them. Never raw SQL | **Decided** 2026-10-09 |
| D15-amend | Who provisions datawatch secrets | **Operator authorized the agent** (2026-10-09) to mint the imap-mcp service token and set the scoped secrets through its datawatch access, on condition that keys and data are protected: values go straight from generator to datawatch or a 0600 file and never appear in output, logs, transcripts or git | **Decided** 2026-10-09 |
| D18 | Encrypting an existing deployment's databases | **Encrypt both** `imap.db` and `cache.db` with `imap-mcp db encrypt`. Keys are random, generated straight into datawatch secrets `imap_mcp_state_key` / `imap_mcp_cache_key` (scope `service:imap-mcp`, never printed), referenced as `${secret:…}`. A scheduled `run-rules` job needs the datawatch token in its environment (e.g. a wrapper loading a 0600 env file). Plaintext backups are listed for the operator to delete (D14) | **Decided** 2026-10-09 |

## Phases

| Phase | Release | Scope | Depends on | Status |
|-------|---------|-------|------------|--------|
| P0 | 0.5.3 | Security: scoped token auth middleware for `/api` + `/mcp`, scope table for all routes/tools, config + example, tests; datawatch backend token (via datawatch agent), datawatch secrets, `~/.mcp.json` header | D13a, D13a-1 | **Done (0.5.3)**: code + tests done (Tested=Yes); Validated=Yes on a side instance (401/403 per scope, MCP tool filtering, SSE). |
| P1 | 0.6.0 | Storage: driver swap to ncruces + adiantum; split `cache.db` / `imap.db` with a verified one-time migration of rules, webhooks and nonces (backup first); per-file optional encryption + `${secret:}`/`${ENV}` key resolution, fail closed; systemd ordering after datawatch when needed | D1, D1a, D1b, D5 | **Done (0.6.0)**: Tested=Yes; Validated=Yes on a side instance (migration of a live-DB snapshot: all rules carried over, identical content hash; `run-rules --dry-run` against two live accounts; `serve` with encrypted cache). Converting an existing plaintext DB to encrypted: `imap-mcp db encrypt` (D14); Tested=Yes, Validated=Yes on a scratch copy of the live state DB (refused while held open; all rows verified; `run-rules --dry-run` on two live accounts with the encrypted file: same rules matched, 0 errors; no key fails closed) |
| P2 | 0.7.0 | Sync engine: SPECIAL-USE folder resolution, INTERNALDATE window with 3-level overrides, UID diff + CONDSTORE, UIDVALIDITY rebuild, headers + bodies + MIME decode (go-message), Message-ID de-dup, bus events | D6, D8, D9, D10 | **Done (0.7.0)**: Tested=Yes (fake-source CONDSTORE/UIDVALIDITY/window/dedup tests + real-protocol tests against go-imap's in-memory server). Validated=Yes on a side instance against two live accounts: INBOX + `\Sent` resolved on Dovecot and Gmail, CONDSTORE used on both, a second cycle found only newly arrived mail, server UNSEEN counts unchanged by sync |
| P3 | 0.8.0 | Cleaning: auto purge + orphans + VACUUM, `cache_sweep` (dry-run default), `\Flagged` exemption, window-resize purge/backfill; content-cleaning hook point | D7, D8 | **Done (0.8.0)**: Tested=Yes (sweep/orphan/FTS-rebuild/prune/keep_flagged/vacuum-interval tests). Validated=Yes on a side instance: filterless sweep refused, dry run counts only, real folder sweep removed cached copies and the next sync re-fetched them, server UNSEEN unchanged |
| P4 | 0.9.0 | Load + enrichment: provider interface (Ollama / datawatch proxy), two-lane priority, caps/backoff, datawatch capacity yield, quiet hours, `/api/health` stats | D11a, D11b | **Done (0.9.0)**: Tested=Yes (provider error classes, datawatch proxy contract, window/Ollama/datawatch gates, rate limiter, lanes, backoff/max attempts, concurrency cap, restart requeue, lane assignment). Validated=Yes on a side instance with local Ollama: backfill held at ~30/min with no errors, manual trigger bypassed the limit, admin scope enforced. The datawatch capacity gate and proxy classifier were verified against their API contracts in tests only, because imap-mcp can't authenticate to datawatch yet (datawatch#203) |
| P5 | 0.10.0 | REST full platform: shared service layer (MCP + REST), all stub routes (reads, writes, rules, enrichment), webhook delivery, `/api/query` DSL | D13, D13a | **Done (0.10.0)**: shared service layer, all MCP-mirroring routes, semantic search and intelligence reads done with tests (Tested=Yes). Webhook delivery (D16) done: Tested=Yes; Validated=Yes on a side instance against two live accounts (live deliveries, all HMAC signatures valid, no addresses or content in any payload; `datawatch`-scoped token refused; receiver outage + service restart: all failed deliveries retried and delivered after backoff). `/api/query` JSON DSL (D17) done: Tested=Yes; Validated=Yes on a side instance over the live encrypted cache (queries incl. group/aggregate on all four views, each <2 ms; injection string matched nothing; raw `sql` field rejected 400; read+send token refused 403). **P5 complete: v0.10.0** |
| P6 | each release | Docs + release per phase: CHANGELOG, config.example.yaml, IMAP-MCP-CONTEXT.md, README roadmap, live validation notes; **email community skill** (`skills/imap-mcp/SKILL.md` → datawatch-community `skills/comms/imap-mcp`) updated per release, or new email skills added, covering new tools (`cache_sweep`), auth/token setup, REST routes, sync window and encryption config | all | Ongoing |
| P7 | after 0.10.0 | **Full docs, tutorials and examples pass** (operator request 2026-10-08): once everything is built, expand the documentation with end-to-end tutorials and worked examples across MCP tools, REST (incl. webhooks + `/api/query` DSL), token auth/scopes setup, datawatch integration (secrets, messaging backend, scheduled jobs), sync window/encryption/cleaning config, and the email community skills. Examples use `example.com` placeholders only, never mailbox data. **Docs audit (2026-10-09)** found: README frozen at 0.3.1 (28 tools vs 44, REST listed as 501, single DB, missing auth/scopes, storage split, encryption, sync/cache, enrichment, webhooks, query); IMAP-MCP-CONTEXT.md tool counts/stub list/schema stale; architecture overview frozen at 0.1.0; skill `move_bulk` params wrong and cleanup tools missing; datawatch guide lacks bearer tokens; CHANGELOG has 0.4–0.10 under Unreleased; `.env.example` stale; stubs and empty intelligence views undocumented. New docs needed: token setup, sync/cache config, encryption how-to, enrichment tuning, REST reference, rules engine + scheduling, deployment (systemd), upgrade notes, known limitations | P1–P6 | **Done (0.10.4)**: README rewritten; new docs index, auth-tokens, encryption, deployment, rest-api, rules, sync-cache, enrichment and known-limitations guides; existing guides, example configs, context file and skill (0.7.0) corrected against the code (all 34 REST routes and 44 tools cross-checked; links checked). Code findings from the audit are in the plans backlog |

Each phase follows AGENT.md:
- `go build ./...` and `go test ./...` pass, with functional tests (Tested=Yes).
- Live validation against both a Dovecot and a Gmail account (Validated=Yes,
  documented here without mailbox data).
- New config fields go in `config.example.yaml`, with `IMAP_MCP_*` overrides and
  exposure in `/api/health` or a stats endpoint.
- Version bump, CHANGELOG entry, conventional commits.

## Content cleaning (planned for iteration 3, D7-D)

An optional normalization stage between MIME decode and enrichment. The cached body
stays as received; only the text sent to the embedder or classifier is cleaned:
- HTML→text, dropping tracking pixels and invisible elements
- strip quoted reply chains and signatures
- redact one-time codes, card/account numbers, and password-reset links

Design it as a pluggable `Cleaner` interface (Option-4 rule) so it can be iterated
on alongside the intelligence work. Iteration 2 only needs to leave the hook point
in the enrichment pipeline.

## Out of scope

- Iteration 3 intelligence (sender profiles, KG, anomalies): iterates in parallel.
- IMAP IDLE / `watch_folder` (iteration 4).
- Any modification of the mailbox by the sync engine. The sync is read-only against
  the server, and purges touch the cache only.
