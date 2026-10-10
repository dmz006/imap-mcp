# AGENT.md — imap-mcp

Operating rules for Claude when working on this codebase.
Adapted from `dmz006/datawatch` AGENT.md. Datawatch-specific rules (mobile parity,
localization, containers, PWA) are intentionally omitted.

---

## Pre-Execution Rule

Before any code changes, new features, or bug fixes:

1. **Load IMAP-MCP-CONTEXT.md** — read `IMAP-MCP-CONTEXT.md` at the repo root
2. **Re-read relevant AGENT.md sections** for the task
3. **Verify compliance** — ensure planned approach follows all applicable rules
4. **Flag conflicts** — if a prompt conflicts with a rule, notify user before proceeding

---

## Prime Rule

**The user makes all decisions.** When a design or implementation decision is not covered
by an existing rule, stop and run the Decision Interview Protocol (DIP) before writing code.

---

## Decision Interview Protocol (DIP)

When any implementation step requires an unresolved design decision:

1. **Context** — one paragraph: what decision is needed and why it matters now
2. **Options** — numbered list, each with: what it does + trade-offs
3. **Recommendation** — agent's pick + one-sentence rationale
4. **One interview question** — phrased as "Which option do you prefer?"
5. **Wait** for answer before proceeding

Ask **one question at a time**. Never batch multiple questions.

DIP applies to: architectural choices, API shape, data model, UX, scope ambiguity.
DIP does NOT apply to: bug fixes with a clear answer, cosmetic changes.

After user decides: record the decision as a rule in the relevant AGENT.md section.

---

## Scope Constraints

- Work only within this repo
- Do not read, write, or execute files outside this repo unless explicitly instructed
- Do not modify system files or install packages without user confirmation

---

## Code Quality Rules

- All Go code must compile with `go build ./...` — no exceptions
- All new packages must have a package-level comment explaining purpose
- The `Authenticator`, `Bus`, and `Pool` interfaces must remain stable
- Do not remove existing API endpoints or MCP tools without user approval
- All new config fields must appear in `config.example.yaml` with comments
- Target close to 100% test coverage for new/changed logic; tests must be functional, not skeletons

---

## Testing Rules

Two levels required for every feature:

1. **Unit/integration tests** — Go `_test.go` files; run with `go test ./...`
2. **Live connection tests** — actually connect to a real IMAP server, verify the MCP tool
   or API endpoint works end-to-end. Document: server tested against, what was observed.

- **Tested=Yes** — Go tests exist and pass
- **Validated=Yes** — live IMAP connection confirmed the feature works

Do not mark Validated=Yes from unit tests alone.

---

## Git Discipline

- Every logical change gets its own commit with a conventional commit message
- Format: `type(scope): description` — e.g. `feat(sync): add UID range fetch`
- Types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`
- Do not squash history. Each commit must be meaningful and reversible
- Do not force-push to `main`
- Include version in commit message: `v0.2.0: feat(mcp): implement list_messages`

---

## Versioning

Version lives in **one place**: `cmd/imap-mcp/main.go` `var Version` (also `internal/config/config.go` `var Version`).
Both must match on every commit.

- **patch** — bug fixes, docs, config changes: `0.1.0` → `0.1.1`
- **minor** — new features, new MCP tools, new API endpoints: `0.1.0` → `0.2.0`
- **major** — breaking changes (user must explicitly request): `0.1.0` → `1.0.0`

Never reuse a version. Bump before every push.

**Releases.** Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`: it
checks the tag matches `config.Version`, runs the tests, builds
linux/darwin × amd64/arm64 binaries with `SHA256SUMS`, and publishes a GitHub
release whose notes are the version's `CHANGELOG.md` section
(`scripts/release-notes.sh`). So every tagged version needs its CHANGELOG
section before the tag is pushed. For a tag pushed earlier, run the workflow by
hand with its `tag` input.

---

## Dependency Rules

- Do not add new Go module dependencies without noting them in a commit message
- Prefer standard library over third-party for simple tasks
- All new dependencies must be MIT-compatible
- Run `go mod tidy` after any dependency change

---

## Configuration Rule

**No configuration may ever be hard-coded.** Every value must be:
1. Settable in `config.yaml`
2. Overridable via `IMAP_MCP_*` environment variable
3. Exposed in `GET /api/health` or a dedicated stats endpoint
4. Referenced in `config.example.yaml` with an explanatory comment

Credentials must always use `${ENV_VAR}` references in YAML, never plaintext values.

---

## Security Rules

- Never log or commit credentials, tokens, OAuth secrets, or email content
- Never expose raw email bodies in logs — log UIDs and subjects only
- The `auth.password`, `auth.client_secret`, and `auth.token_file` fields must
  never appear in log output
- No local-environment leaks in git: use `example.com`, `user@example.com` in docs/examples

### No inbox data in the repository

**Any file derived from a live mailbox must never be committed to git.** This includes:
- Subscription scan results, sender lists, subject lines, email addresses from real accounts
- Search result exports, message summaries, analytics output
- Any file whose content was generated by querying a live IMAP account

Output files from mail operations go to a directory outside the repo or
a user-specified path. Never to `docs/`, `tmp/`, or anywhere inside the repo tree.

This rule exists because a violation was caught on 2026-05-31: subscription scan results
containing real sender names and email addresses were committed and pushed to the public
GitHub repo before being caught and purged.

---

## Planning Rules

For any work touching 3+ files or non-trivial architecture:

1. Create `docs/plans/YYYY-MM-DD-<slug>.md` with: date, version, scope, phases, status
2. Mark phases Planned / In Progress / Done as work proceeds
3. After implementation, update plan status and note the version it shipped in

---

## Documentation Rules

Every commit adding or changing behavior must update docs:

1. `CHANGELOG.md` under `[Unreleased]`
2. `config.example.yaml` for any new config fields
3. `IMAP-MCP-CONTEXT.md` if architecture, tools, or API surface changes significantly
4. `docs/plans/README.md` for bugs and backlog

---

## Work Tracking

Multi-step requests: show a plan checklist before starting and update as tasks complete:

```
## Plan
- [ ] Task 1
- [~] Task 2 (in progress)
- [x] Task 3
```

Single-task requests: skip the checklist, just do the work.

---

## Rate Limit Handling

If Claude hits an API rate limit:
- Output: `DATAWATCH_RATE_LIMITED: resets at <time>`
- Write `PAUSED.md` with current context
- Resume cleanly when limit resets

---

## User Input Tracking During Active Work

1. Note the input immediately — add to task tracking
2. Do not ignore — acknowledge and note when it will be handled
3. Update the plan
4. Design decisions: run DIP before proceeding

---

## Background Shell Cleanup

After every build+test cycle, kill lingering poll-watcher bash processes:

```bash
pgrep -a -u "$USER" bash | grep 'shell-snapshots/snapshot-bash-'
# kill each watcher found; keep only interactive login shells
```

---

## RTK Integration

Always prefix commands with `rtk` — it reduces token output 60-90%:

```bash
rtk go build && rtk go test ./...
rtk git status && rtk git diff
rtk git log
```

---

## Event Bus Rule

All new subsystems must publish and subscribe through `internal/bus` — never via
direct function calls or shared state. This is the foundation for future autonomous
agents, federation, streaming API, and the plugin system.

New event types go in `internal/bus/bus.go` as `EventType` constants.

---

## Architecture-for-Option-4 Rule

Every new component must be designed with the following future capabilities in mind:
- **Autonomous agents** — rules that trigger actions without human input
- **Federation** — multiple imap-mcp instances sharing KG/enrichment data
- **Streaming event bus** — `/api/events` SSE stream for real-time consumers
- **Plugin system** — third-party enrichment, classifiers, custom rule engines

Design guideline: new subsystems expose an interface, not a concrete type, so
they can be replaced or extended without touching call sites.

---

## Recorded Decisions

- **2026-10-08 — D1 (sync cache encryption):** the optional at-rest encryption of the
  sync cache is **whole-database** (SQLCipher-style), not field-level. When it's on,
  every column, the FTS index, and the vectors live inside the encrypted file.
  See `docs/plans/2026-10-08-sync-cache.md`.
- **2026-10-08 — D1a (encryption library):** use `github.com/ncruces/go-sqlite3`
  with the `vfs/adiantum` VFS (pure Go, no cgo). Do not add cgo SQLite drivers.
- **2026-10-08 — D1b (DB split):** mail cache (`cache.db`, disposable) and state
  (`imap.db`: rules, webhooks, nonces) are separate files. Each one independently
  supports encryption, selected by the operator. Encryption is never mandatory.
- **2026-10-08 — D5 (key source):** encryption keys come only from `${secret:name}`
  or `${ENV}` passphrase references (Argon2id). Never auto-generate a key, and fail
  closed when the key is missing.
- **2026-10-08 — D6 (cache content):** the cache stores headers, bodies, vectors and
  enrichment details whether or not it is encrypted. Encryption is a security
  setting and must never gate features.
- **2026-10-08 — D7 (cleaning):** cache cleaning = auto window purge + orphan cleanup,
  on-demand `cache_sweep` (dry_run defaults true, counts before deleting), and an
  optional `\Flagged` exemption. Cleaning never touches the mailbox. Content cleaning
  before enrichment is planned for iteration 3.
- **2026-10-08 — D8 (window):** the cache window is based on IMAP INTERNALDATE, with
  `window_days` set globally and overridable per account and per folder. Resizing
  the window purges or backfills; new mail always comes first.
- **2026-10-08 — D9 (change detection):** sync detects changes with a per-cycle UID
  diff inside the window, plus CONDSTORE `CHANGEDSINCE` for flags where available.
  A UIDVALIDITY change rebuilds the folder. Keep one code path for all servers.
- **2026-10-08 — D10 (folders):** synced folders are set in config: SPECIAL-USE
  tokens or literal names, default INBOX + `\Sent`, per-account override. Enrichment
  is de-duplicated by Message-ID.
- **2026-10-08 — D11a (LLM routing):** enrichment uses a provider interface per call
  type. Embeddings go direct to Ollama; classification goes to direct Ollama (the
  default) or through the datawatch `/api/proxy/llm/<name>` proxy.
- **2026-10-08 — D11b (load):** enrichment has two priority lanes (new mail before
  backfill), concurrency and rate caps with backoff, backfill that yields to
  datawatch capacity and Ollama load, and optional quiet hours for backfill.
  New-mail enrichment is never paused.
- **2026-10-08 — D12 (release):** iteration 2 ships storage first, then one minor
  release per phase (0.6.0 storage, 0.7.0 sync, 0.8.0 cleaning, 0.9.0 load,
  0.10.0 REST). Each release is live-validated before the next phase starts.
- **2026-10-08 — D13 (REST surface):** REST is the full platform. MCP tool logic
  lives in an interface-based service layer that both MCP and REST call (no
  duplicated handler logic). All declared routes get implemented, including
  mailbox writes, rules CRUD/test, enrichment trigger, webhook delivery and the
  `/api/query` DSL. Webhook delivery and the DSL each get their own DIP before
  implementation. Write routes ship only behind the auth chosen in D13a.
- **2026-10-08 — D13a (local auth):** `/api` and `/mcp` use named bearer tokens
  with scopes (`read`, `write`, `send`, `admin`), configured as
  `server.auth.tokens` with `${secret:name}` / `${ENV}` references only. Every
  REST route and MCP tool declares its required scope; enforcement is one
  middleware with constant-time comparison. `/api/health` stays open. Log the
  token name, never the value. Each client gets least privilege (datawatch's
  `imap_mcp` backend: `read` + `send`). Tokens are provisioned as datawatch
  secrets so scheduled jobs and the datawatch backend keep working. Changes to
  datawatch code or local datawatch config go through the datawatch agent, which
  owns datawatch; never edit the datawatch repo directly.
- **2026-10-08 — D13a-1 (auth timing):** token auth ships first, as its own
  security patch release 0.5.3 before P1. Security fixes may ship as patch
  releases even when they add config, and they are never bundled into a
  storage/migration release.
- **2026-10-08 — D13a-2 (auth enforcement):** HTTP `serve` requires at least one
  token. The only way around this is an explicit `server.auth.disabled: true`,
  which logs a warning at every startup and shows `auth: disabled` in
  `/api/health`. A token reference that cannot be resolved always fails closed.
  Roll out client support (datawatch backend, `~/.mcp.json`) before enforcing.
- **2026-10-09 — D14 (encrypting an existing DB):** conversion is only ever an
  explicit operator command (`imap-mcp db encrypt`), never automatic at
  startup. It refuses while another process has the file open, verifies the
  encrypted copy (key opens it, integrity check, identical per-table row counts
  and digests) before replacing the original, and removes the plaintext
  original only after that verification. Other plaintext copies (backups) are
  listed for the operator, never deleted by the tool.
- **2026-10-09 — D15 (datawatch secrets path):** `${secret:name}` resolves
  only through datawatch's external-service endpoint
  `GET /api/external/secrets/{name}` (datawatch ≥ v8.75.0), authenticated with
  the imap-mcp service token, which reaches imap-mcp through an `${ENV}`
  reference only. **Amended 2026-10-09:** the operator authorized the agent to
  mint the token and set the scoped secrets via its datawatch access. Secret
  values must never be printed, logged, committed or otherwise placed in a
  transcript: they go straight from a generator into datawatch or into a 0600
  file outside every repo. Secrets are scoped
  `service:imap-mcp`. There is no fallback to the agent endpoint.
- **2026-10-09 — D15a (datawatch TLS trust):** datawatch's self-signed
  certificate is pinned with `datawatch.ca_file`, added to the system roots
  for every imap-mcp → datawatch call. TLS verification is never disabled, and
  there is no skip-verify option. A missing or unusable `ca_file` fails closed.
- **2026-10-09 — D16 (webhook delivery):** webhooks use a durable outbox in
  `imap.db` with at-least-once delivery and a unique delivery id. Payloads
  are metadata only: identifiers, counts and flags, never subject, sender,
  addresses, body or error text. Every request is HMAC-SHA256 signed with a
  per-webhook secret that imap-mcp generates and shows once. Only https URLs,
  or http to loopback, are allowed, and redirects are never followed.
  Registration needs the `admin` scope.
- **2026-10-09 — D17 (`/api/query`):** the query endpoint takes structured
  JSON over fixed views (`messages`, `senders`, `anomalies`, `kg`) with
  allowlisted fields, compiled to parameterized SQL. It never accepts raw SQL.
  Message bodies are returned only when a query names them explicitly.
- **2026-10-09 — D18 (encrypting an existing deployment):** encrypt both `imap.db`
  and `cache.db`. The keys are random values generated straight into datawatch
  secrets (`imap_mcp_state_key`, `imap_mcp_cache_key`, scope
  `service:imap-mcp`), are never printed, and are referenced from the config as
  `${secret:…}`. A scheduled `run-rules` job needs the datawatch service token in
  its environment (e.g. a wrapper that loads a 0600 env file).
- **2026-10-09 — D28 (per-message intelligence index):** `imap.db` keeps one
  compact row per message seen by the header scan. The row holds:
  - a hash of the Message-ID;
  - the date;
  - the sender id;
  - the direction (in or out);
  - a hash of In-Reply-To.

  Rows hold no subjects, bodies or addresses; addresses live only in
  `senders`. The index gives exact de-duplication across folders and labels,
  and reply-time pairing over full history. P3 and P4 reuse it.
- **2026-10-09 — D27 (REST for the P1 tools):** these routes are reads:
  - `GET /api/threads/{thread_id}`;
  - `GET …/messages/{uid}/attachments`;
  - `GET /api/search/cross`.

  Content downloads need the `write` scope, the same as the MCP tools:
  - `GET …/attachments/{part}`;
  - `GET …/messages/{uid}/export.eml`;
  - `POST /api/export`, which returns an `.mbox`.

  Downloads stream the bytes back as `application/octet-stream` with a safe
  filename and write nothing on the server.
- **2026-10-09 — D29 (scan progress per account):** `/api/health` is
  unauthenticated, so it never names accounts. It shows per-account scan
  progress by position only (account 1, 2, … in config order). The same
  breakdown with account names is at `GET /api/intelligence/status` (read
  scope).
- **2026-10-10 — D30 (new-sender hold):** a rule condition `new_sender`
  (window `new_sender_days`, default 30) matches mail from senders with no
  history only when header signals say bulk or scam: brand or own-domain
  impersonation in the display name, not addressed to the owner, bulk headers,
  throwaway domain, owner's address in the subject, Reply-To at another domain.
  A reply to the owner's own mail, or copying someone the owner has written
  to, always stays. Score 1 defers to the message's enrichment hall. It
  refuses to match until the account's history scan is complete. Held mail is
  moved, never deleted, and moving it back trusts the sender. Authentication
  alone was rejected: sampled spam passed DKIM or had no result.
- **2026-10-10 — D31 (held-mail digest):** once a day a full rule run sends a
  digest of newly held mail two ways: a summary message APPENDed to the
  account's INBOX (no mail is sent) and a `hold.digest` event carrying only the
  account and count. A datawatch dashboard can consume the event once its
  plugin mode exists.
- **2026-10-10 — D32 (reply-tracking data):** reply tracking (Q1) uses the
  full history, not the 30-day cache: the D28 index gains a conversation hash
  per message (and the header scan restarts once to fill it). The aim is the
  complete record needed to respond well, across all of the owner's mail.
- **2026-10-10 — D34 (observing moves, Q2):** the D28 index records where each
  message was last seen; a location-only pass reads Trash and Junk (Gmail
  Trash and Spam): hashes and folder only, no profile counts, graph edges or
  anomaly checks. Rule moves are recorded by the rule engine and never count
  as the owner's. Discard = a message in Trash or Junk (special-use, else
  common names) or a configured discard folder; rescue = a message from Junk
  or a hold folder seen in any other non-discard folder. A rescue trusts the
  sender and resolves their open anomalies.
- **2026-10-10 — D35 (suggestion trigger, Q2):** ratio-based over all
  history: the owner discarded at least `ratio` (0.8) of a sender's received
  mail, with at least `min_discards` (3). A domain rule is suggested when
  `domain_min_addresses` (2) or more addresses at the domain qualify;
  otherwise one rule per address. The learned rule mirrors the owner's action:
  mostly trashed means a trash rule, mostly junked means move to Junk.
- **2026-10-10 — D36 (automatic rules, Q2):** configurable `rules.learn.mode`:
  `suggest` (default; a rule exists only when accepted), `inactive`
  (auto-create inactive rules) or `active` (auto-create active rules). Never
  suggested or ruled: anyone the owner has written to or replied to, trusted
  senders, the owner's own addresses and domains; a domain rule is skipped
  when any such sender is at the domain. Settings: `rules.learn: {mode, ratio,
  min_discards, domain_min_addresses, discard_folders}` with env overrides.
- **2026-10-10 — D46 (webhook payload setting; amends D16):** each webhook has
  a payload setting: `metadata` (D16's identifiers and counts), `full` (the
  whole event payload) or a list of fields. Existing webhooks stay
  `metadata`; a new webhook subscribed to `rule.suggested` defaults to
  `full`. `rule.suggested` carries full suggestion details (address or
  domain, action, counts, ratio, match count, rule id).
- **2026-10-10 — D45 (Q1 implementation review):** the 0.16.0 choices made
  without DIP were reviewed with the operator one at a time. Kept: storing
  subject, counterpart, Message-ID, folder and UID per conversation in
  `reply_threads`; the rescan keeps the hold running; vendor/unknown senders
  count only if written to or classified conversation/personal; `\Answered`
  checked live on listing; a newer message re-opens a dismissed conversation;
  `thread_id` as the item id; the combined digest (sent when only replies
  wait, 25 items); counterpart = first recipient, no notes to self, list mail
  never a thread's latest. Changed: rescan progress is shown in
  `/api/health`; the default lookback (`within_days`) is 90 days for the
  tools and the digest. Changes ship as 0.16.1.
- **2026-10-10 — D44 (1.0.0 milestone):** when the current path is complete
  (assistant features Q0–Q5), that release is the official 1.0.0. imap-mcp
  versions independently of datawatch.
- **2026-10-10 — D33 (dismissing reply items):** a reply item clears when the
  owner replies (an outgoing message in the index answers it, or the message
  carries `\Answered`) or is explicitly dismissed (`dismiss_reply` tool and
  API route; dismissals stored in `imap.db`). Folder location never clears an
  item, since rules file real conversations out of INBOX.
- **2026-10-09 — D19 (intelligence store):** `senders`, `kg_*` and
  `anomalies` move to the durable state store `imap.db`, through a state
  migration with a backup first. History comes from two sources:
  - a one-off, resumable, rate-limited header-only backfill of all folders
    (envelope fields only, never bodies, PEEK);
  - incremental updates as mail syncs.

  The cache is an input too. For recent mail it supplies the enriched signals
  (classification tags, embeddings, bodies, attachment metadata). The builders
  persist what they derive from them into `imap.db`, so it survives the window
  purge and cache rebuilds. Message-level detail stays in the cache.
- **2026-10-09 — D20 (sender roles):** profile counts are header
  statistics: first and last seen, received, sent-to, and average reply time
  from Sent. Roles come from signals first:
  - `List-Id`/`List-Unsubscribe` or `Precedence: bulk` → newsletter;
  - noreply or `Auto-Submitted` → bot;
  - you have sent to them → personal, or colleague when on the account's own
    domain;
  - otherwise, the majority of the cached classification tags.

  Only senders still `unknown` go to the classify LLM, with a few recent
  subjects, through the existing enrichment gates. The header backfill
  therefore fetches those header fields (`HEADER.FIELDS`, PEEK), not just the
  envelope.
- **2026-10-09 — D21 (knowledge graph):** two sources feed the graph.
  - **Deterministic, from headers and existing tags.** People, organizations
    (by domain), threads and subscriptions come from headers. Edges are
    `belongs_to`, `corresponds_with`, `cc_with`, `is_subscription` and
    `participates_in`. Project and topic entities come from the wing/room
    classification tags.
  - **LLM body extraction.** The local classify model reads the cached bodies
    of recent conversation and personal mail and extracts richer relations
    (`manages`, `works_on`, organizations and deadlines mentioned). It runs
    through the enrichment gates, sends bodies only to the configured classify
  model, and stores
    LLM-derived edges with a confidence below 1.0 so they can be filtered.
- **2026-10-09 — D22 (anomalies):** detection runs at two points.
  - **As each message syncs:**
    - `new_sender`, only for conversation and personal mail;
    - `auth_failure`, a known sender that used to pass DKIM/DMARC and now
      fails;
    - `lookalike_domain`, a near-spelling of a domain you correspond with;
    - `reply_to_mismatch`, a known sender whose Reply-To is on another
      domain.
  - **Periodic sweep:** `silence` (a regular correspondent goes quiet) and
    `volume_spike`.

  Thresholds are configurable. Findings are written to `anomalies` and
  published as `anomaly.detected` with identifiers only. Sync and backfill
  capture `Authentication-Results` and `Reply-To`. LLM detection of
  "asks for payment" goes to the backlog.
- **2026-10-09 — D23 (`get_thread`):** answer from the cache first: every
  cached folder, Sent included, ordered by date. If the thread has messages
  outside the sync window, or none are cached, fall back to a live IMAP search
  by Message-ID/References (Gmail: `X-GM-THRID`). Live `list_messages` uses
  the same `thread_id` derivation as sync, so its IDs work with `get_thread`.
- **2026-10-09 — D24 (`get_attachments`):** listing returns metadata only
  (read scope), from the cache or live BODYSTRUCTURE. Fetching a part saves it
  to the working-dir sandbox and needs the write scope. `text/*` parts under a
  configurable size cap are also returned inline, decoded. Binary content is
  never returned inline.
- **2026-10-09 — D25 (`export_message`):** exports go to the working-dir
  sandbox (write scope), never inline. A single message exports as `.eml`, the
  raw RFC 822 fetched with PEEK. A batch exports as one `.mbox`, selected by a
  list of UIDs, a `thread_id` (the D23 lookup, including the live fallback) or
  a sender. Batches are capped by configurable message and byte limits.
- **2026-10-09 — D26 (`cross_account_search`):** searches the cache's
  full-text index by default, across every account and cached folder, merged
  by date. With `live: true`, it runs IMAP SEARCH in parallel on each account
  (`folder` param, default INBOX) to cover full history. Each result names its
  source. A failing account gets its own error entry and doesn't stop the
  others.

---

*Prime rule: the user makes all decisions. When in doubt, run DIP before writing code.*


# Memory & Knowledge (datawatch)

Use the datawatch memory system proactively during this session.

## Before starting work
- Use `memory_recall` to check if similar work has been done
- Use `kg_query` to understand entity relationships
- Use `research_sessions` for deep cross-session search

## During work
- Use `memory_remember` to save key decisions and patterns
- Use `kg_add` to record relationships

## When asked about project history
Always check memory first with `memory_recall` before answering from training data.

## Available tools
| Tool | Purpose |
|------|---------|
| `memory_recall` | Semantic search across project memories |
| `memory_remember` | Save decisions, patterns, context |
| `kg_query` | Entity relationship queries |
| `kg_add` | Record new relationships |
| `research_sessions` | Cross-session research |
| `copy_response` | Last LLM response from any session |
| `get_prompt` | Last user prompt from any session |

<!-- rtk-instructions -->
# RTK (Rust Token Killer) - Token-Optimized Commands

**Always prefix commands with `rtk`**. If RTK has a dedicated filter, it uses it.
If not, it passes through unchanged. This means RTK is always safe to use.

```bash
# Always use rtk prefix, even in chains:
rtk go build && rtk go test ./...
rtk cargo build
rtk git status && rtk git diff
rtk git log
```

**Key savings:** Build 80-90%, Test 90-99%, Git 59-80%, Files 60-75%.
Run `rtk gain` to view token savings statistics.
<!-- /rtk-instructions -->