# Changelog

All notable changes to imap-mcp are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.18.0] - 2026-10-10

### Added
- **Your other addresses (AGENT.md D50).** `identity.also_me` lists your
  other addresses or `@domains` (work, Kindle, old ones); they count as you
  everywhere. The scan also proposes candidates that use your display name
  or are built from your name: `suggest_identities` (read), the daily
  digest's "Is this you?" section, `confirm_identity` / `reject_identity`
  (write), and `GET /api/identities`, `POST /api/identities/confirm|reject`.
  Confirming rewrites the stored history once: past mail from the address
  becomes yours and its profile is hidden.

### Changed
- **`needs_reply` drops the noise found on production (D51):** calendar
  invitations, automated senders even if you once wrote to them (vendor
  role, no-reply addresses, mostly list/bulk/auto mail, or a message the
  model called transactional, a notification or an alert), and bare
  forwards with no note of their own. Forwards with a note stay.

### Upgrade
- **Back up `imap.db` first.** Two tables are added; no rescan.

## [0.17.2] - 2026-10-10

### Changed
- **`needs_reply` lists only people you have written to or replied to**
  (AGENT.md D49). After 0.17.1, spam that looks exactly like a person still
  passed every header check and the model's label. First-time senders return
  once the planned model second opinion (Q3) can vet them.

### Added
- **New-sender hold: fake replies.** A first-time sender's `Re:` or `Fwd:`
  subject that answers nothing (no In-Reply-To or References) scores 2 and
  is held on its own, like brand impersonation.

## [0.17.1] - 2026-10-10

### Fixed
- **`needs_reply` listed spam** (found validating on production; AGENT.md
  D48). Mail in Trash, Junk or a hold folder no longer needs a reply. A
  first-time sender now counts only when the classify model calls the
  message a conversation or personal mail, its headers pass the new-sender
  hold's checks with no bulk or scam signal (read live), and the sender has
  no open anomaly besides `new_sender`. People you have written to always
  count.
- **Suggested rules for senders a display-name rule already covers.** A
  rule's `from` now counts as covering a sender when it matches the display
  name too, as IMAP SEARCH FROM does.
- `needs_reply` could hang on the single state-database connection when
  checking first contacts; rows are now read before any further query.

## [0.17.0] - 2026-10-10

### Added
- **Learning from your moves (Q2; AGENT.md D34–D36, D47).** A sender whose
  mail you mostly move to Trash or Junk yourself (≥ 80% of everything they
  sent, at least 3) becomes a suggested rule that repeats what you did:
  trash, or move to Junk. When 2 or more addresses at a domain qualify, one
  `@domain` rule (never for webmail domains, never over someone you write
  to). People you have written to, trusted senders, your own addresses,
  senders a rule already covers and held senders are never suggested.
  `suggest_rules` (read) and `GET /api/rules/suggestions` list them with
  the evidence, the exact rule and an INBOX match count; `dismiss_suggestion`
  (write) and `POST /api/rules/suggestions/dismiss` say no for good, as does
  deleting a learned rule.
- **`rules.learn` settings.** `mode: suggest` (default), `inactive` or
  `active` (create the rules automatically), `ratio`, `min_discards`,
  `domain_min_addresses`, `discard_folders`, with env overrides.
- **Rescues.** Moving a message out of Junk or a hold folder marks its
  sender trusted and resolves their open anomalies.
- **`rule.suggested` event**, once per new suggestion or created rule, with
  full details. The daily digest gains "Suggested rules" and "Rules
  created" sections; `hold.digest` gains a `suggested` count.
- **Webhook payload setting (D46, amends D16).** Each webhook has
  `payload`: `metadata` (the D16 form), `full`, or a list of fields. New
  webhooks subscribed to `rule.suggested` or `*` default to `full`; others,
  and every webhook registered before 0.17, stay `metadata`.

### Changed
- The header scan records where each message was last seen, and a
  location-only pass reads Trash and Junk (Gmail Trash and Spam): Message-ID
  hashes and folder only, no profiles, graph edges or anomaly checks.
- Rules remember the messages they move (`rule_moves`), so those never count
  as yours.

### Upgrade
- **Back up `imap.db` first.** On first start the header scan reads all
  history once more to record locations; an unfinished 0.16 rescan is folded
  into it. Everything keeps working meanwhile; `suggest_rules` reports
  `history_complete: false` until it finishes.

## [0.16.1] - 2026-10-10

### Changed
- **Reply tracking looks back 90 days by default** (was 30): `needs_reply`,
  `awaiting_reply`, their REST routes and the digest's "Waiting on you"
  section (D45). Pass `within_days` to narrow or widen it.

### Added
- **Rescan progress in `/api/health`.** `intelligence.reply_history_complete`,
  and per account `rescan_complete` and `rescan_folders_remaining`, for the
  one-time 0.16 rescan that fills reply tracking. Counts only, no names.

### Documentation
- AGENT.md D44 (official 1.0.0 after Q5) and D45 (review of the 0.16.0
  implementation choices with the operator).

## [0.16.0] - 2026-10-10

### Added
- **Reply tracking (Q1; AGENT.md D32, D33).** `needs_reply` lists
  conversations waiting on you: the latest message is someone else's,
  addressed to you, and you have not replied. `awaiting_reply` lists those
  where you wrote last. Both cover your whole history in any folder
  (person-to-person mail only; no newsletters, bots or held mail), take
  `older_than_days` (2), `within_days` (30) and `limit`, and return each
  conversation's `thread_id`, counterpart, subject, folder, UID and days
  waiting. REST: `GET /api/replies/needed`, `GET /api/replies/awaiting`.
- **Clearing an item.** A reply you send clears it once the scan sees it. So
  does the `\Answered` flag, checked live on the listed messages. To clear one
  without replying, use `dismiss_reply` (write scope;
  `POST /api/replies/dismiss`); a newer message in that conversation brings
  it back. Where the mail is filed never clears an item, because rules file
  real conversations out of INBOX.
- **"Waiting on you" in the daily digest.** The digest also lists
  conversations waiting two or more days. It now goes out even on days
  nothing was held, still at most once a day per account. `hold.digest`
  gains a `waiting` count.

### Changed
- `imap.db` gains `reply_threads` (the latest message of each
  person-to-person conversation, including its subject) and `digest_log`.
  `docs/intelligence.md` § Storage and privacy says what is stored.

### Upgrade
- **Back up `imap.db` first.** On first start, the header scan reads all
  history once more to fill reply tracking (`intel_scan.rescan_until`).
  Profiles, anomaly detection and the new-sender hold keep working
  meanwhile, and nothing is counted twice. Until the scan finishes, the reply
  lists report `history_complete: false`.

## [0.15.5] - 2026-10-10

### Added
- **Held-mail digest in `/api/health`.** A `rules` block shows the digest
  settings (`hold_digest`, `hold_digest_hour`) and counts: messages held and
  not released, awaiting the next digest, and the last digest time. Counts
  only; no senders, subjects or account names.

### Documentation
- `IMAP-MCP-CONTEXT.md` brought up to date for 0.15: the `new_sender` hold,
  `held_messages`, `senders.trusted`, `hold.digest`, the `rules:` block and
  `run-rules` opening the cache.

## [0.15.4] - 2026-10-10

### Fixed
- **`hold.digest` never reached webhooks from `run-rules`.** The CLI queued
  only `rule.fired` into the webhook outbox; it now queues `hold.digest` too.

## [0.15.3] - 2026-10-10

### Fixed
- **`run-rules` crashed on a `new_sender` rule.** The CLI opened only the
  state database, and the hold's one-signal fallback read classification halls
  from the cache (nil pointer). `run-rules` now opens the cache too when its
  key resolves (falling back to rules only), and the hold treats a missing
  cache as "not classified yet". The crash also stopped the daily digest.

## [0.15.2] - 2026-10-10

### Fixed
- **Brand check flagged brands' own domains.** A display name such as
  "Amazon Web Services" from `amazonaws.com` or "Microsoft" from
  `microsoftonline.com` counted as impersonation because the domain was not on
  the short allowed list. A domain that carries the brand name no longer
  counts; lookalikes on unrelated domains still do.

## [0.15.1] - 2026-10-10

### Fixed
- **`new_sender` trusted malformed halls.** Halls stored before 0.15.0 could be
  copied placeholders ("project or context", "<newsletter>"), and the hold
  treated them as classifications. Only real halls count now; startup unwraps
  bracketed halls and clears the rest (`unclassified` is kept).

## [0.15.0] - 2026-10-10

### Added
- **New-sender hold** (D30). A rule condition `new_sender` (window
  `new_sender_days`, default 30) matches mail from senders with no history
  (never written to, nothing from them before the window, not at a domain you
  write to) only when header signals say bulk mail or a scam: a display name
  borrowing a brand, an agency or your own domain; not addressed to you; bulk
  headers; a throwaway-looking domain; your address in the subject; `Reply-To`
  at another domain. One signal defers to the message's enrichment hall.
  Replies to your own mail and messages copying someone you write to always
  stay. The rule matches nothing until the account's history scan is complete.
  Dry runs list who would be held and why (`preview`). Moving a held message
  back to the inbox trusts its sender.
- **Held-mail digest** (D31). Once a day, at the first full rule run after
  `rules.hold_digest_hour` (default 8), a summary of newly held mail is
  APPENDed to the account's INBOX (no mail is sent), and a `hold.digest`
  event carries the count.

### Fixed
- **`send_message` accepted bare words as recipients.** A recipient such as
  `ops` went to SMTP, where the server completed it with its own domain and
  bounced it. Recipients must now be `name@domain.tld`, and each send logs the
  account and recipient count.
- **Classification placeholders in tags and the graph.** The classify model
  sometimes copied its prompt's placeholder text into wing/room tags, which
  became knowledge-graph entities. The prompt has no placeholders now, tags
  are sanitised, and startup removes the stored ones.

## [0.14.3] - 2026-10-09

### Fixed
- **`list_folders` failed in MCP clients.** Tool results that are JSON
  arrays were sent as `structuredContent`, which MCP requires to be an object,
  so clients rejected the result. Array results are now text-only; object
  results are unchanged.

## [0.14.2] - 2026-10-09

### Added
- **Per-account scan progress** (D29). The `intelligence` block in
  `/api/health` gains `accounts`: per account, folders and folders complete,
  headers scanned in the current pass, `backfill_complete` and last scan.
  Health is unauthenticated, so accounts are identified by position (`index`,
  config order), never by name. New `GET /api/intelligence/status` (read
  scope) returns the same with account names.

## [0.14.1] - 2026-10-09

### Fixed
- **Thread export failed when part of the thread had just moved.**
  `export_message` / `POST /api/export` with `thread_id` used the cache's
  locations. A message a rule moved since the last sync (up to 15 minutes)
  made the whole export fail with 404. Thread exports now search the server,
  prefer its locations, skip messages that no longer exist, and report them
  (`missing` in the tool result, `X-Export-Missing` over REST). `uids` and
  `from` exports stay strict. Found while checking the new examples against
  a live instance.

### Added
- `docs/examples.md` sections 11–13: threads, attachments, export and
  cross-account search over REST; profiles, the knowledge graph and
  `/api/query` on the intelligence views; anomaly listing, resolving, and a
  webhook receiver that pushes high-severity findings. The intro and token
  setup (`WRITE_TOKEN`) are updated.

## [0.14.0] - 2026-10-09

This completes the intelligence plan: every tool now does real work.

### Added
- **Anomaly detection** (plan P4, D22). See
  [docs/intelligence.md](docs/intelligence.md#anomalies).
  - **Checks on each new message.** They run only after the account's
    history scan is complete, and only for mail from the last
    `anomaly_lookback_days`, so first scans and upgrade rescans flag nothing
    historical:
    - `new_sender` (low): the first person-to-person message from someone
      you've never written to;
    - `auth_failure` (high): DMARC, or DKIM without DMARC, fails for a
      sender whose earlier mail passed;
    - `lookalike_domain` (high): one or two characters away from a domain
      you write to, or the same after folding look-alike characters;
    - `reply_to_mismatch` (medium).
  - **Periodic checks:**
    - `silence` (low): a regular correspondent gone quiet. Reported while
      the silence is new, and resolved when they write again;
    - `volume_spike` (medium).
  - **Each finding** has a description and `details`. Per-message findings
    also have `folder`, `uid` and `message_ref` (Message-ID). Senders get an
    `anomaly_score`.
  - **`anomaly.detected`** is now published with `{id, type, severity}`.
    Webhooks deliver the same three fields, never the sender.
- **`resolve_anomaly`** MCP tool (write) and
  `POST /api/anomalies/{id}/resolve` (write). `get_anomalies` and
  `GET /api/anomalies` gain `type` and `sender` filters.
- Config: `intelligence.anomalies`, `anomaly_lookback_days`,
  `anomaly_auth_min_passes`, `anomaly_silence_min_messages`,
  `anomaly_silence_min_days`, `anomaly_spike_min`, `anomaly_spike_factor`,
  with `IMAP_MCP_INTELLIGENCE_*` overrides.
- The `/api/query` `anomalies` view gains `folder`, `uid` and `message_ref`.
- Companion skill 0.12.0 (anomalies). Example 10.15 "Morning security
  check".

### Changed
- `imap.db`: `anomalies` gains four columns, added in place. No rescan is
  needed. Back up `imap.db` first.

## [0.13.0] - 2026-10-09

### Added
- **Knowledge graph** (plan P3, D21). `kg_query`, `GET /api/kg`, the
  `/api/query` `kg` view and each profile's `relationships` now return data.
  Entities: people (each account's address stands for you), organizations
  (domains, never webmail), threads, projects and topics. See
  [docs/intelligence.md](docs/intelligence.md#knowledge-graph).
  - **From the header scan**, once per message: `belongs_to`,
    `corresponds_with` (person-to-person mail), `cc_with` (at most 8 people
    per message), `is_subscription` (list, bulk or automated mail) and
    `participates_in` (thread roots; the scan now also reads `References`).
  - **From cached enrichment tags:** `works_on` (wing → project) and
    `discusses` (room → topic).
  - **From the classify model** reading recent cached conversation and
    personal mail (quoted text removed, 1,500 characters, enrichment gates,
    each message once): `manages`, `reports_to`, `works_at`, `works_on` and
    `deadline` with a due date. The model's answer is untrusted: only these
    predicates are kept, names are cleaned and capped, and confidence is 0.6.
    Turn it off with `intelligence.kg_llm: false`.
  - **Edges carry** `weight`, `valid_from`, `last_seen` and `confidence`.
    After `intelligence.kg_stale_days` (365) without evidence, `valid_to`
    is set and `current` is false. `kg_query` lists the strongest first.
- Config: `intelligence.kg`, `kg_stale_days`, `kg_llm`, `kg_llm_per_tick`,
  each with an `IMAP_MCP_INTELLIGENCE_*` override. `/api/health` adds
  `kg_entities`, `kg_relationships` and `kg_model_messages`. The `kg` query
  view gains `weight` and `last_seen`.
- Companion skill 0.11.0 (knowledge-graph usage). Examples 10.13 "Who's who
  on a project" and 10.14 "Who did I lose touch with?".

### Changed
- **Upgrade:** `imap.db` gains columns in place (back it up first). On the
  first start the header scan runs again over all history to build the
  graph, once per message. Profile counts don't change.

## [0.12.0] - 2026-10-09

### Added
- **Sender profiles** (plan P2; D19, D20, D28). A background scanner reads the
  headers of every folder and builds a profile per sender in `imap.db`. On
  Gmail it reads All Mail only; elsewhere it skips `\Junk`, `\Drafts` and
  `intelligence.exclude_folders`. Each profile holds:
  - first and last contact;
  - messages received and sent;
  - list, bulk and auto-submitted counts;
  - DKIM and DMARC pass/fail counts, from the receiving server's
    `Authentication-Results` only;
  - reply count and average reply time;
  - a role.

  The scanner opens folders read-only and fetches with `PEEK`. It never
  reads bodies or subjects and never marks mail read. It is resumable and
  rate-limited. The first pass covers all history, and roles and reply times
  fill in every 5,000 messages; later ticks read only new UIDs.
  `get_sender_profile`, `GET /api/senders` and the `/api/query` `senders`
  view now return data. See [docs/intelligence.md](docs/intelligence.md).
- **Roles** come from header signals, then the majority enrichment tag of the
  sender's cached mail, then the classify model for senders still unknown.
  The model sees only the address, the name and a few subjects, and runs
  through the enrichment backfill gates. Each profile records its
  `role_source`.
- **Per-message hash index** (D28). One row per message in `imap.db`: a
  Message-ID hash, the date, the sender id, the direction and an In-Reply-To
  hash. It holds no addresses or content. It keeps a message counted once
  across folders, labels and rescans after a UIDVALIDITY change, and pairs
  replies over full history.
- **`intelligence:` config block** (`enabled`, `scan_interval_minutes`,
  `backfill_per_minute`, `batch_size`, `exclude_folders`, `llm_roles`,
  `llm_roles_per_tick`) with `IMAP_MCP_INTELLIGENCE_*` overrides.
  `/api/health` gains an `intelligence` block with scan progress and role
  counts (no addresses). Sender profiles gain `scan_complete`.
- `/api/query` `senders` view: new fields `role_source`, `reply_count`,
  `list_count`, `bulk_count`, `auto_count`, `dkim_pass`, `dkim_fail`,
  `dmarc_pass`, `dmarc_fail`.
- Companion skill 0.10.0: the sender audit, briefing, subscription and
  phishing workflows use `get_sender_profile`.

### Changed
- **The intelligence tables moved from `cache.db` to `imap.db`** (`senders`,
  `kg_entities`, `kg_relationships`, `anomalies`). They were never filled in
  `cache.db`, so the empty copies are dropped at open and the cache is kept
  (no rebuild). Back up `imap.db` before upgrading.

## [0.11.0] - 2026-10-09

The last four stub tools are implemented, so every registered tool now does
real work. `get_sender_profile`, `kg_query` and `get_anomalies` still return
empty data until the intelligence builders land (plan P2–P4).

### Added
- **`get_thread`** (D23). Returns a whole conversation, oldest first, from
  the cache (every cached folder, Sent included). When the thread's root
  message isn't cached, because it is older than the sync window, the tool
  searches the server live by Message-ID, References and In-Reply-To. The
  live scope is Gmail's All Mail or another `\All` mailbox, else INBOX,
  `\Sent` and `\Archive`. Each message says whether it came from `cache` or
  `live`. REST: `GET /api/threads/{thread_id}` (read).
- **`get_attachments`** (D24). Without `part`, lists attachments (part,
  filename, type, size) from the live BODYSTRUCTURE (read scope). With
  `part`, it decodes that attachment and saves it into `working_dir` under a
  sanitised name (write scope, via a new argument-dependent scope rule).
  `text/*` attachments up to `tools.attachment_inline_kb` are also returned
  inline; binary content never is. REST: `GET …/attachments` (read) and
  `GET …/attachments/{part}` (write; a download).
- **`export_message`** (D25). `uid` exports one raw `.eml`; `uids`, `thread_id`
  or `from` exports one mboxrd `.mbox`, written into `working_dir`. Caps are
  checked from RFC822.SIZE before anything is downloaded, and an over-cap
  selection is refused, never truncated. REST: `GET …/export.eml` and
  `POST /api/export` (write; downloads).
- **`cross_account_search`** (D26). Searches every account at once. By
  default it uses the cache's full-text index across all cached folders; with
  `live: true` it runs IMAP SEARCH on each account in parallel for full
  history. Hits carry `account`, `source` and `thread_id`. An account that
  fails is reported in `errors` without failing the search. REST:
  `GET /api/search/cross` (read).
- **REST content downloads** (D27). Attachments and exports stream back as
  `application/octet-stream` with `nosniff` and a sanitised filename. Nothing is
  written on the server. They need the `write` scope and have a 5-minute
  timeout.
- **`tools:` config block**: `attachment_inline_kb` (64),
  `attachment_max_mb` (25), `export_max_messages` (500), `export_max_mb`
  (100), each with an `IMAP_MCP_TOOLS_*` override, shown in `/api/health`.
- **Companion skill 0.9.0**: the new tools, an attachment safety rule, and a
  "hand over a conversation" workflow. `docs/examples.md` gains workflow
  10.12 and uses the new tools in the briefing, phishing and dossier
  workflows.

### Fixed
- **Live `thread_id` now matches the cache.** `list_messages`, search,
  `get_message` and `get_headers` built `thread_id` by joining In-Reply-To, so
  it never matched the cache's (the References root). Every header fetch now
  PEEKs the References field and uses the same derivation. Summaries also
  carry `message_id`.

## [0.10.5] - 2026-10-09

### Added
- **Companion skill 0.8.0.** A "Multi-step workflows" section: morning briefing,
  subscription audit, teach by example, rule review, phishing check, drafts
  without sending, dossiers, and rules for unattended scheduled sessions
  (report and propose, never change the mailbox).
- **Agent workflows in `docs/examples.md` (section 10).** Eleven multi-step
  workflows an agent runs with the tools and the companion skill: morning
  briefing, subscription audit, teach-by-example filing, rule review, unanswered
  mail, phishing check, drafts to the Drafts folder, dossiers, scheduled agent
  sessions, the inbound command channel and event loops. A "Not there yet"
  list names the empty and stub tools.
- **`docs/examples.md`.** Runnable recipes for the 0.5–0.10 features: agent
  prompts, `/api/query` snapshots, a cron digest, rules, a signed-webhook
  receiver that forwards `rule.fired` to a push service, semantic search, the
  SSE stream, cache tuning and encryption. Queries were checked against a live
  instance and the webhook receiver against signed and forged requests.

## [0.10.4] - 2026-10-09

### Fixed
- **Reading mail no longer marks it read (v0.10.4).** `get_message`,
  `get_headers` (and `GET /api/messages/{uid}` and its headers route),
  `detect_subscriptions` and the inbound command watcher fetched body sections
  without `PEEK`, so the server set `\Seen` on everything they read. The
  watcher marked every unread message in its folder as read, not only command
  attempts. All now use `BODY.PEEK`; only command attempts are marked `\Seen`,
  explicitly.
- **Microsoft 365 / Outlook OAuth works (v0.10.4).** `xoauth2` always requested
  Google's `https://mail.google.com/` scope, so the Microsoft endpoint rejected
  every authorization. Microsoft now requests
  `https://outlook.office.com/IMAP.AccessAsUser.All` and `offline_access` (for a
  refresh token); Google is unchanged.
- **`auth-setup` callback hardened (v0.10.4).** The OAuth `state` is now random
  and checked (a callback with a wrong or missing state is rejected and does not
  end the flow), provider errors (`error=access_denied`) are reported, and the
  callback listens on loopback only (127.0.0.1 and ::1) instead of all
  interfaces.
- **Provider auto-detect no longer panics (v0.10.4)** on short non-Gmail
  addresses (11–13 characters), and only `@gmail.com` / `@googlemail.com`
  count as Gmail (not e.g. `@notgmail.com`).

### Changed
- **Documentation pass (P7, v0.10.4).** README rewritten for the current feature set; new
  guides for token auth, encryption, deployment, the REST API, rules, the sync
  cache, enrichment and known limitations, plus a docs index; existing guides,
  example configs, the agent context file and the skill corrected against the code.

## [0.10.3] - 2026-10-09

### Fixed
- **Clean stop with open streams (v0.10.3).** `serve` exited with status 1
  ("context deadline exceeded") on every stop while a client held a long-lived
  stream open (the datawatch `/api/events` SSE consumer, MCP streamable GET).
  Requests now inherit the server context, so streams end as soon as shutdown
  starts; any handler that still lingers past the 5 s grace period has its
  connection closed instead of failing the stop.

## [0.10.2] - 2026-10-09

### Changed
- **More personal details scrubbed (v0.10.2).** Removed mailbox-derived counts
  from the inbox-cleanup cookbook, README and IMAP-MCP-CONTEXT.md (now illustrative
  numbers), installed model sizes, an internal commit note, and the account count
  in the architecture diagram. No code changes.

## [0.10.1] - 2026-10-09

### Changed
- **Docs scrubbed of personal and internal details (v0.10.1).** Public docs, plans
  and tests no longer contain local home paths, a personal mail domain or account
  name, hardware specifics, or stats derived from live mailboxes. Examples use
  `example.com` and `/path/to/imap-mcp`. No code changes.

## [0.10.0] - 2026-10-09

### Added
- **`/api/query` JSON query DSL (v0.10.0, D17).** `POST /api/query` answers ad-hoc
  questions over fixed, read-only views of the cache: `messages`, `senders`,
  `anomalies` and `kg`.
  - A query can use allowlisted fields, filters
    (`eq`/`ne`/`lt`/`lte`/`gt`/`gte`/`in`/`nin`/`contains`/`prefix`/null
    checks), `group_by` with
    `count`/`count_distinct`/`min`/`max`/`sum`/`avg`, `order_by`, `limit`
    (at most 1000) and `offset`.
  - It is compiled to parameterized SQL; raw SQL is never accepted.
  - Message bodies are returned only when named.
  - Queries run read-only with a 10-second timeout and need the `admin`
    scope.
  - `GET /api/query` describes the views. See `docs/query.md`.
- **Webhook delivery (v0.10.0, D16).** `POST /api/webhooks {url, events}` registers an
  endpoint and returns its signing secret. The secret is shown only once.
  - **Durable outbox.** Deliveries are stored in `imap.db`
    (`webhook_deliveries`). They are retried with exponential backoff (30 s up
    to 1 h, 12 attempts) across restarts.
  - **At least once.** Each delivery carries a unique `delivery_id` that
    receivers can use to drop duplicates.
  - **Metadata only.** Payloads carry identifiers, counts and flags, never
    subject, sender, addresses, body or error text.
  - **Signed.** Requests are HMAC-SHA256 signed
    (`X-Imap-Mcp-Signature: t=<unix>,v1=<hex>`).
  - **URL rules.** URLs must be https, or http to loopback only. Redirects are
    not followed.
  - **Auto-disable.** After 100 consecutive failed attempts a webhook is
    disabled; its pending deliveries are kept and resume when it is
    re-enabled.
  - **New routes:** `POST /api/webhooks/{id}/enable`, `POST .../test` (a
    `webhook.test` ping) and `GET .../deliveries`. All webhook routes need the
    `admin` scope.
  - **`rule.fired` is now published** (rule id, action, match count) by
    `run_rules`, the REST API and the hourly `run-rules` CLI. The CLI queues
    its events and `serve` delivers them.
  - The outbox prunes delivered rows after 7 days and failed rows after
    30 days.
- **`datawatch.ca_file` (v0.10.0, D15a).** Pins datawatch's self-signed TLS certificate
  (e.g. `~/.datawatch/tls/server/cert.pem`) for every call imap-mcp makes to
  datawatch: secrets, the capacity gate and the LLM proxy. It is trusted in
  addition to the system roots. Verification is never disabled, and a missing
  or invalid file fails startup.
- **`imap-mcp db encrypt` (v0.10.0, D14).** Encrypts an existing plaintext `imap.db`
  and/or `cache.db` in place with the configured keys (`--only state|cache`).
  - It refuses while another process has the file open, so stop the service
    first.
  - It writes an encrypted copy and verifies it: the key opens it, the
    integrity check passes, and every table's row count and content digest
    match. Only then does it replace the plaintext original.
  - It lists other plaintext copies, such as the pre-0.6.0 backup, but never
    deletes them.
  - Re-running is safe: already-encrypted files are skipped.
- **REST platform (v0.10.0, D13).**
  - **Shared service layer.** Every operation is implemented once in
    `internal/service`, and both MCP tools and REST call it. Existing MCP
    output shapes are unchanged.
  - **New REST routes:**
    - Folders: `GET /api/accounts/{a}/folders`.
    - Messages: paged `GET .../messages` (`limit`, `offset`, `order`),
      `GET`/`DELETE .../messages/{uid}` (`?permanent=true`),
      `PUT .../flags` and `POST .../move`.
    - `GET /api/search`, `POST /api/accounts/{a}/sync`,
      `GET /api/accounts/{a}/stats`.
    - Rules CRUD: `GET`, `POST`, `PUT /api/rules/{id}` (full replace),
      `DELETE`, and `POST /api/rules/{id}/test` (a dry run).
    - Folder names containing `/` are passed as `%2F`.
    - Errors map to 400, 404, 422, 502 and 503. The send endpoint keeps its
      existing contract.
  - **Semantic search.** The `semantic_search` tool and
    `POST /api/search/semantic` rank cached, enriched mail by similarity to a
    query or a reference message.
  - **Intelligence reads.** `get_sender_profile`, `get_sender_history`,
    `kg_query` and `get_anomalies`, plus `GET /api/senders[/{address}]`,
    `/api/kg` and `/api/anomalies`, read the cache tables. They return data
    once iteration-3 intelligence fills those tables.
  - **Still pending:** webhooks and `/api/query`. Each waits on its own
    decision.

### Changed
- **datawatch secrets come from the external-service endpoint (v0.10.0, D15).**
  `${secret:name}` now resolves via `GET /api/external/secrets/{name}`
  (datawatch v8.75.0 or later) with imap-mcp's service token, minted by the
  operator with `datawatch secrets mint-service-token imap-mcp`. Secrets must
  be scoped `service:imap-mcp`. The old `/api/agents/secrets/` path is no
  longer used. Errors now say what to fix (401: re-mint the token; 403/404:
  missing or unscoped secret).

### Fixed
- **Permanent deletes now remove only the targeted messages (v0.10.0).** Before,
  `delete_message permanent`, `purge_sender permanent` and the move-by-copy
  fallback ran a folder-wide `EXPUNGE`. That also destroyed any other message
  already marked `\Deleted` by a mail client. go-imap's own `Move` fallback
  does the same on servers without UIDPLUS. Now:
  - Servers with UIDPLUS use `UID EXPUNGE` on exactly the target messages.
  - Without UIDPLUS, the operation refuses if any other message is already
    `\Deleted`.
  - All moves go through native MOVE, or COPY plus that same targeted delete.
- **Enrichment ordering (v0.10.0).** Every new-mail item now finishes before
  any backfill item in the same batch starts. Before, the concurrency cap
  could let a backfill item go first.

## [0.9.0] - 2026-10-08

### Added
- **Enrichment load handling (v0.9.0, D11a/D11b).**
  - **Providers per call type.** Embeddings always go straight to Ollama
    (`enrichment.embed.url/model`). Classification uses either `ollama`
    (direct, the default) or `datawatch`, which goes through
    `POST /api/proxy/llm/<datawatch_llm>` for LLM-registry routing and failover.
  - **Two queue lanes.** New mail (UIDs above a synced folder's previous
    high-water mark) is always processed before backfill (first sync, window
    growth).
  - **Load caps.**
    - Each provider has a concurrency cap (`concurrency`, default 2).
    - Backfill has a token-bucket rate limit (`backfill_per_minute`, default 30).
    - Transient provider errors (5xx, 429, network) trigger exponential
      backoff (`backoff_max_seconds`). A message is retried up to
      `max_attempts` times, then marked as an error.
    - Rows left `processing` by a restart are requeued.
  - **Yielding.** Backfill pauses while models other than ours exceed
    `yield.max_foreign_resident_gb` on the embed Ollama (`/api/ps`), or while
    any `yield.datawatch_pools` capacity pool is full or has waiters
    (`GET /api/capacity`). Optional `backfill_window` "quiet hours" restrict
    when backfill runs. New mail is never paused by any of these. Unreachable
    sources never block.
  - **Status and triggers.**
    - `enrichment_status` and `trigger_enrichment` are implemented: MCP tools
      plus `GET /api/enrichment/status` and `POST /api/enrichment/trigger`.
      A trigger bypasses gating and the rate limit, but not backoff or caps.
    - `/api/health` reports queue depth per lane, done in the last hour,
      oldest pending age, pause reason and backoff.
  - The cache schema moves to v3 (queue lane). The cache is rebuilt from IMAP
    on first start.

## [0.8.0] - 2026-10-08

### Added
- **Cache cleaning (v0.8.0).** Cleaning touches the cache only, never the
  mailbox.
  - **After every sync cycle:**
    - Folders dropped from config or gone from the server, and accounts
      removed from config, are pruned from the cache. Pruning runs per account,
      and only after that account's folder list was read, so a disconnected
      account is never wiped.
    - Orphaned vectors and queue rows are removed.
    - The FTS5 index is integrity-checked and rebuilt if it's inconsistent.
    - The WAL is checkpointed. VACUUM runs at most every
      `sync.vacuum_interval_hours` (default 24).
  - **`cache_sweep`** (MCP tool, `admin` scope) and **`POST /api/cache/sweep`**:
    - Filter by account, folder, `older_than_days`, `errors_only` or `all`. At
      least one filter is required.
    - `dry_run` defaults to true and returns per-folder counts.
    - A real sweep deletes the cached copies, resets their sync state, cleans
      orphans and runs VACUUM. Mail still inside the window comes back on the
      next sync.
  - **`sync.keep_flagged`** (default off) keeps `\Flagged` mail cached even
    outside the window.
  - A `cache.cleaned` bus event is published for each pass.
  - **Content-cleaning hook:** an `enrichment.Cleaner` interface, no-op by
    default, shapes the text sent to the embedding and classification models.
    The cached body is never changed. Real cleaners are planned for
    iteration 3.
  - New env overrides: `IMAP_MCP_SYNC_KEEP_FLAGGED` and
    `IMAP_MCP_SYNC_VACUUM_INTERVAL_HOURS`.

## [0.7.0] - 2026-10-08

### Added
- **Mail cache sync (v0.7.0).** The background sync now fills `cache.db`.
  Before, it only stamped `sync_state`. Per account and per configured folder:
  - **Folders:** `sync.folders` takes SPECIAL-USE tokens (`\Sent`, `\Archive`,
    …) or literal names. The default is `INBOX` + `\Sent`; an account's
    `sync.folders` replaces the global list.
  - **Window:** `sync.window_days` (default 30) is a rolling window by IMAP
    INTERNALDATE. Overrides go in `sync.folder_window_days`, `accounts[].sync.window_days`
    or `accounts[].sync.folder_window_days`. Shrinking the window purges the
    cache; growing it backfills.
  - **Change detection:** a UID diff each cycle. New mail is fetched newest-first
    in batches (envelope, flags, size, INTERNALDATE, full body via `BODY.PEEK[]`).
    Expunged, moved or aged-out mail is removed from the cache only. Flags
    refresh via CONDSTORE `CHANGEDSINCE` where available, otherwise by
    re-fetching the window. A UIDVALIDITY change rebuilds the folder.
  - **Read-only:** folders are opened with EXAMINE, so no flag changes on the
    server, not even `\Seen`. The connection lock is released between
    batches, so MCP tools stay responsive during a backfill.
  - **MIME:** decoded with go-message (transfer encodings and charsets). The
    first text/plain and text/html parts are kept; attachments are recorded as
    name, type and size only. Messages over `sync.max_message_mb` (default 25)
    are cached headers-only.
  - **Enrichment:** new messages are queued, de-duplicated by Message-ID
    across folders. Thread IDs come from References / In-Reply-To.
  - **Events:** `message.synced`, `message.updated`, `message.deleted`,
    `folder.synced` and `sync.complete` on the bus and `/api/events`.
  - `sync_account` now returns per-folder results: cached, new, removed and
    flag updates, plus CONDSTORE use and any errors.
  - Sync settings appear in `/api/health`. New env overrides:
    `IMAP_MCP_SYNC_WINDOW_DAYS`, `IMAP_MCP_SYNC_MAX_MESSAGE_MB` and
    `IMAP_MCP_SYNC_INTERVAL_MINUTES`.

  The cache schema is versioned (`PRAGMA user_version`). A cache from 0.6.0 is
  dropped and rebuilt from IMAP on first start.

### Fixed
- Database files and their WAL/SHM sidecars are created and kept at mode 0600
  (v0.7.0). Before, the WAL/SHM files followed the umask.
- Enrichment no longer leaves rows with a NULL body, subject or sender name
  stuck in `pending` forever, which could starve the queue (v0.7.0).

## [0.6.0] - 2026-10-08

### Added
- **Optional at-rest encryption per DB file (v0.6.0).** Set
  `db.encryption_key` and/or `db.cache.encryption_key`. This is whole-file
  encryption with the adiantum VFS: the full-text index, vectors and WAL are
  all encrypted.
  - Keys are `${secret:name}` or `${ENV}` passphrase references, run through
    Argon2id. A key is never generated.
  - A missing, unresolvable or wrong key refuses to open the file. A key set on
    an existing plaintext file also refuses to open.
  - `run-rules` opens only the state DB and resolves only its key.
  - Encryption state is reported in `/api/health` under `storage`.
  - New env overrides: `IMAP_MCP_DB_CACHE_PATH`, `IMAP_MCP_DB_ENCRYPTION_KEY`
    and `IMAP_MCP_DB_CACHE_ENCRYPTION_KEY`.

### Changed
- **Storage (v0.6.0).** The SQLite driver is now `github.com/ncruces/go-sqlite3`
  (pure Go, no cgo), replacing `modernc.org/sqlite`. Data now lives in two files:
  - `db.path` (`imap.db`) is the state DB: rules, webhooks and inbound nonces.
    It can't be rebuilt from IMAP, so back it up.
  - `db.cache.path` (default `cache.db`, next to `db.path`) is the mail cache:
    messages, full-text index, vectors, senders, knowledge graph and sync
    state. It's disposable and rebuilt from IMAP.

  **Upgrade:** on first open, a single-file `imap.db` from before 0.6.0 is split
  automatically:
  - It's first backed up with `VACUUM INTO` to
    `imap.db.bak-<timestamp>-pre-0.6.0`, and the backup is verified.
  - Every rule, webhook and nonce is fingerprinted (SHA-256) before and after.
    The cache tables are dropped in one transaction, which rolls back on any
    difference.
  - The split is logged with row counts.
  - It's idempotent and safe if `serve` and `run-rules` start at the same time.

### Fixed
- `Rules.List` no longer fails on rules with NULL description, priority or
  run count (v0.6.0).

## [0.5.3] - 2026-10-08

### Security
- `/api` and `/mcp` now require named, scoped bearer tokens (v0.5.3).
  `browserGuard` (v0.5.2) only stopped browsers: any local process could still
  send mail or call destructive MCP tools without credentials. New
  `server.auth` config:
  - `tokens: [{name, token, scopes}]`, with scopes `read`, `write`, `send` and
    `admin`. Values are `${secret:name}` or `${ENV}` references, at least 32
    characters.
  - Every REST route and MCP tool declares a required scope. MCP `tools/list`
    only shows tools the token may call; a denied call returns a tool error.
    Unknown routes and unmapped tools fail closed.
  - `serve` refuses to start without a token unless `server.auth.disabled: true`
    is set (insecure opt-out, warned at startup, reported in `/api/health` as
    `"auth"`, env `IMAP_MCP_SERVER_AUTH_DISABLED`). Unresolvable token
    references always stop startup.
  - `/api/health` stays open. Token names are logged; values never are.
  - stdio mode and `run-rules` are unaffected and do not need datawatch to
    resolve tokens.

  **Upgrade:** add tokens to the config and to every client before upgrading.
  Claude Code: `"headers": {"Authorization": "Bearer ..."}` on the `imap-mcp`
  HTTP entry. datawatch's `imap_mcp` backend needs a version that sends a token.

### Fixed
- `GET /api/events` (SSE) now sends its 200 headers immediately, instead of at
  the first event or 15 s heartbeat, and is exempt from the 30 s route timeout
  and the 60 s server `WriteTimeout`, which had cut the stream every 30–60 s
  and forced datawatch's `imap_mcp` backend to reconnect (v0.5.3).

## [0.5.2] - 2026-10-08

### Security
- Browser-originated requests against the local HTTP server are now refused
  (v0.5.2). Before, any web page open on the host could POST a `text/plain`
  body to `/api/accounts/{account}/messages/send` without a CORS preflight and
  make imap-mcp send mail, and DNS rebinding could reach `/mcp` tools. The new
  `browserGuard` middleware (`internal/server/guard.go`):
  - accepts only loopback names or the configured `server.host` in `Host`
  - rejects foreign or `null` `Origin` headers
  - requires `Content-Type: application/json` on unsafe `/api` methods

  Non-browser clients (curl, datawatch, Claude Code) are unaffected.

## [0.5.1] - 2026-10-08

### Fixed
- SQLite connection pragmas were never applied (v0.5.1). The DSN used
  mattn-style `_journal`/`_fk`/`_timeout` params, which `modernc.org/sqlite`
  silently ignores, so the DB ran in rollback-journal mode with no busy timeout
  and foreign keys off. Concurrent writers — e.g. `imap-mcp serve` plus the
  hourly `run-rules` CLI on the same file — could fail immediately with
  `database is locked (SQLITE_BUSY)`. Now uses `_pragma=busy_timeout(5000)`,
  `journal_mode(WAL)` and `foreign_keys(1)`; covered by `internal/db/db_test.go`.

## [0.5.0] - 2026-06-15

### Added
- **Enterprise Gmail / Google Workspace OAuth.** `auth.provider: google` selects the
  Google endpoint for custom domains, and `xoauth2_service_account` supports
  domain-wide delegation (headless). See `docs/enterprise-gmail-oauth.md`.

## [0.4.0] - 2026-06-15

### Added
- **`imap-mcp run-rules [--dry-run]`.** Applies active rules once and exits, for
  scheduled automation (cron or a datawatch job). See `docs/rules.md`.

## [0.3.1] - 2026-06-14

### Added
- **`label_bulk`.** Applies a Gmail label to all messages from a sender.

## [0.3.0] - 2026-06-14

Cleanup tooling and automation, built from the friction of a large real-world
inbox cleanup. **42 MCP tools.**

### Added
- `purge_sender` — move ALL mail from a sender to Trash, draining the folder in
  one call; auto-detects the Trash mailbox (`\Trash` special-use / `[Gmail]/Trash`).
- `top_senders` — rank a folder's senders by count (address/domain), scanning the
  whole folder, so bulk/spam clusters surface in one call.
- Rules engine — `create_rule`, `list_rules`, `delete_rule`, `run_rules`. A rule is
  a match (from/subject/text/older_than_days) plus an action (trash/move/flag/seen),
  persisted in the `rules` table; `run_rules` supports `dry_run` to preview counts.
- `label_message` — apply a Gmail label by COPY into the label mailbox (creates it
  if missing).
- `empty_trash` — permanently delete everything in the auto-detected Trash mailbox.
- IMAP keepalive (NOOP every 4 min) with auto-reconnect on dropped connections.

### Changed
- `search_messages` now returns the true `total_matches` (previously capped at the
  page size, which hid real volumes behind "50").
- `/api/accounts` live-probes each connection (NOOP) instead of trusting pool
  membership, so a silently-dropped connection reports as disconnected.
- `create_folder` / `delete_folder` accept `folder` as an alias for `path`.

## [0.2.1] - 2026-06-07

Closes the datawatch comm loop (datawatch#127).

### Added
- `GET /api/events` — SSE event stream; fans out bus events to connected clients
  (15s heartbeats; slow clients dropped, never backpressure the bus). datawatch's
  `imap_mcp` backend consumes verified `inbound.command` events here.
- `POST /api/accounts/{account}/messages/send` — REST send via the account's SMTP
  (`account` may be `_default`); same semantics as the `send_message` MCP tool.

## [0.2.0] - 2026-06-07

datawatch integration across three independent, operator-opt-in layers (no
auto-injection).

### Added
- **Secrets** — `${secret:name}` credential references resolve via the datawatch
  secrets service when a `datawatch:` block is present; otherwise fully standalone
  (`${ENV}`/plain). Agent-scoped token (least privilege); clear startup error if a
  `${secret:}` reference is used without a datawatch block.
- **Skill** — companion usage skill published to the datawatch community registry
  at `skills/comms/imap-mcp`. Instructions only; pull-based; never auto-loaded.
- **Bidirectional comm**
  - Outbound: per-account `smtp:` config + `send_message` tool (header-injection
    guarded). **34 MCP tools.**
  - Inbound command channel: trust boundary with composable, default-deny gates —
    allowlist, DKIM/DMARC (Authentication-Results), HMAC over a fenced command
    envelope, nonce/replay, capability scoping. Emits `inbound.command` (verified)
    / `inbound.rejected` (audited).
  - PGP gate declared but **fails closed** — backlogged.

## [0.1.0]

Initial scaffold: multi-account IMAP connection pool, MCP server (stdio +
Streamable HTTP), REST API shell, SQLite cache (FTS5 + vectors), Ollama-backed
enrichment pipeline, enforced output sandbox, and the first message/folder/search
tools.

[0.3.0]: https://github.com/dmz006/imap-mcp/releases/tag/v0.3.0
[0.2.1]: https://github.com/dmz006/imap-mcp/releases/tag/v0.2.1
[0.2.0]: https://github.com/dmz006/imap-mcp/releases/tag/v0.2.0
