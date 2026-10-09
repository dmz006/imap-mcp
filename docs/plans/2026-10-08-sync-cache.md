# Iteration 2 — Sync cache (windowed, optionally encrypted)

| Field | Value |
|-------|-------|
| Date | 2026-10-08 |
| Target version | 0.6.0+ (see D12) |
| Status | **Planned — decisions pending** (DIP in progress) |
| Supersedes | "Iteration 2 (message CRUD)" bullets in `IMAP-MCP-CONTEXT.md` / `README.md` roadmap |

## Problem

`internal/sync/syncer.go` `syncFolder` is a scaffold: it SELECTs the folder and stamps
`sync_state.last_synced` but fetches nothing, so `messages` stays empty. Everything
downstream (enrichment, FTS, `semantic_search`, sender profiles, KG, anomalies) is
starved. Live tools and the rules engine query IMAP directly and are unaffected.

## Requirements (operator-decided, 2026-10-08)

- **R1 Folders** — sync INBOX and Sent by default; operator can select other folders.
- **R2 Window** — rolling 30-day cache, adjustable. Changing it purges or backfills
  to match.
- **R3 Encryption option** — at-rest encryption of the cache. **Bodies are cached
  only when encryption is on.**
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
- Two nodes: a local GPU node (RTX 5090 32 GB, Ollama) and the datawatch node (B200
  128 GB, Ollama + OpenAI-compatible endpoint, `max_concurrent_sessions: 1`).
- The datawatch `llm-research` PRD recommends keeping `nomic-embed-text` on the local
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
| D1a | Library for whole-DB encryption | See DIP (pending) | Open |
| D2 | Which fields are encrypted | Moot under D1 (whole-DB: all columns, FTS and vectors are inside the encrypted file) | **Resolved by D1** |
| D3 | Search over encrypted content | Moot under D1 (whole-DB: all columns, FTS and vectors are inside the encrypted file) | **Resolved by D1** |
| D4 | Embeddings when encrypted | Moot under D1 (whole-DB: all columns, FTS and vectors are inside the encrypted file) | **Resolved by D1** |
| D5 | Key source | `${secret:...}` / `${ENV}` passphrase → Argon2id, else an auto-generated 0600 keyfile. Unattended under systemd | Open |
| D6 | Unencrypted mode and enrichment | Body fetched transiently for enrichment and never persisted. Headers plus a short snippet? (see options) | Open |
| D7 | Meaning of "cleaning" | Retention sweep à la datawatch (dry-run, counts) plus optional content normalization before the LLM | Open |
| D8 | Window semantics | INTERNALDATE, cache-only purge, global default with per-account override | Open |
| D9 | Change detection | CONDSTORE where available plus a per-cycle UID-set diff in the window. IDLE deferred to iteration 4 | Open |
| D10 | Folder selection config | `sync.folders: [INBOX, "\\Sent"]` with special-use tokens, per-account override | Open |
| D11 | LLM routing and load | Provider interface: embeddings → local Ollama, classification → configurable (local or datawatch proxy). Throttle with concurrency cap and new-mail-first priority | Open |
| D12 | Release cadence | One minor release per phase (0.6.0, 0.7.0, …) | Open |
| D13 | REST surface | Mirror the MCP read/search tools 1:1 for messages, folders, search | Open |

## Phases

| Phase | Scope | Depends on | Status |
|-------|-------|------------|--------|
| P1 | Sync engine: folder resolution, window, UID diff, flags, expunge, UIDVALIDITY, bus events, headers only | D8, D9, D10 | Planned |
| P2 | Storage modes: crypto package, encrypted columns, bodies + MIME decode (go-message), key mgmt, migrate/rotate | D1–D6 | Planned |
| P3 | Cleaning: retention sweep (dry-run), window-change purge/backfill, content normalization | D7, D8 | Planned |
| P4 | Load and enrichment: provider interface (Ollama / datawatch proxy), throttling, priority, status endpoint | D11 | Planned |
| P5 | REST endpoints for messages, folders, search | D13 | Planned |
| P6 | Docs + release: CHANGELOG, config.example.yaml, IMAP-MCP-CONTEXT.md, README roadmap, live validation | all | Planned |

Each phase follows AGENT.md:
- `go build ./...` and `go test ./...` pass, with functional tests (Tested=Yes).
- Live validation against both a Dovecot and a Gmail account (Validated=Yes,
  documented here without mailbox data).
- New config fields go in `config.example.yaml`, with `IMAP_MCP_*` overrides and
  exposure in `/api/health` or a stats endpoint.
- Version bump, CHANGELOG entry, conventional commits.

## Out of scope

- Iteration 3 intelligence (sender profiles, KG, anomalies): iterates in parallel.
- IMAP IDLE / `watch_folder` (iteration 4).
- Any modification of the mailbox by the sync engine. The sync is read-only against
  the server, and purges touch the cache only.
