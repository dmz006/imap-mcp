# Plan: assistant features: reply tracking, learning, smarter spam, unsubscribe, screener

- **Date:** 2026-10-10
- **Starting version:** 0.15.4
- **Status:** In progress. Q0 done (0.15.5). Each phase's open decisions are put to
  the operator (DIP, one at a time) when that phase starts, and recorded in
  AGENT.md § Recorded Decisions and in the table below.
- **Source:** a survey of open-source email assistants and spam tools (Inbox
  Zero, mAIl, Rspamd's GPT plugin, Dovecot IMAPSieve training, HEY's screener,
  RFC 8058 unsubscribe tools, LLM phishing research). The operator chose three
  recommendations; each becomes one or two phases here.

## Scope

| Phase | Feature | Recommendation | Version |
|---|---|---|---|
| Q0 | Parity catch-up for 0.15 (context doc, config exposure) | prerequisite | 0.15.5 |
| Q1 | Reply tracking: mail you owe a reply, replies you are waiting for | 1 | 0.16.0 |
| Q2 | Learn from moves: rule suggestions, trust from un-junking | 1 | 0.17.0 |
| Q3 | Second opinion for borderline mail + payment/credential-request alerts | 2 | 0.18.0 |
| Q4 | Safe one-click unsubscribe (RFC 8058) | 3 | 0.19.0 |
| Q5 | Screener: approve held senders from the digest, routed to inbox/feed/receipts | 3 | 0.20.0 |

Each phase is a minor release, live-validated before the next one starts
(D12). Q4 and Q5 are independent of each other; Q2 and Q3 both feed the
digest, so Q1 adds the digest sections they extend.

Out of scope:
- training a server-side filter (Rspamd/SpamAssassin): imap-mcp has no access
  to the mail server's filter; Q2 learns into imap-mcp's own rules and trust;
- running a model on every message (Q3 is borderline mail only);
- rules written in plain English evaluated by a model per message (rejected in
  the survey: cost and debuggability); an agent can already turn a sentence
  into an ordinary rule.

## Rules every phase follows

These restate AGENT.md for this plan; AGENT.md wins on any conflict.

**Development**
- Pre-execution: load IMAP-MCP-CONTEXT.md and the relevant AGENT.md sections;
  flag conflicts before writing code.
- DIP for every open decision below, one question at a time, with a
  recommendation; record each answer as a D-number in AGENT.md and here.
- New subsystems publish/subscribe through `internal/bus` (Event Bus rule) and
  sit behind interfaces (Architecture-for-Option-4), so a plugin or another
  classifier can replace them.
- No hard-coded configuration: every new setting is in `config.yaml`, has an
  `IMAP_MCP_*` override, is exposed in `/api/health` or a stats endpoint, and
  is in `config.example.yaml` with a comment.
- Security: no credentials or mail content in logs or git; webhooks carry
  identifiers and counts only (D16); examples use `example.com`. Nothing
  derived from a live mailbox is committed.
- New dependencies: MIT-compatible, named in the commit message, `go mod tidy`.

**Testing**
- Tested=Yes: functional Go tests for all new logic (in-memory IMAP server,
  temporary databases), `go test ./...` green, every commit builds alone.
- Validated=Yes: live run against the production accounts, observed with
  counts and status codes only (no mail content in the transcript). Anything
  that moves or sends mail is previewed (dry run) first, then confirmed with
  the operator.

**Release** (per phase)
1. Back up `imap.db` (service stopped) before any state migration.
2. Bump `config.Version`; write the version's CHANGELOG section.
3. Commit per logical change (`vX.Y.Z: type(scope): …`), push, tag
   `vX.Y.Z`: the release workflow checks the version, runs the tests, builds
   the binaries and publishes the release with the CHANGELOG notes.
4. Deploy: build `imap-mcp.new`, keep the previous binary as a rollback,
   atomic `mv`, restart; never during the hourly run at :00.
5. Live validation; mark the phase Done here with the version.

**Context parity** (per phase, in the same release)
- `IMAP-MCP-CONTEXT.md`: current version, MCP tools table and count, REST
  endpoints, events, schema summary, open items.
- `README.md` tool list; `CHANGELOG.md`; `config.example.yaml`.
- Topic docs: `docs/rules.md`, `docs/rest-api.md`, `docs/webhooks.md`,
  `docs/intelligence.md`, `docs/examples.md` (a worked example per feature),
  `docs/known-limitations.md` where relevant.
- MCP tool definitions and scopes (`definitions.go`, `scopes.go`) match the
  REST routes and docs.
- Skill: `skills/imap-mcp/SKILL.md` (minor skill bump), then a
  dmz006/datawatch-community PR with the same content, merged on the
  operator's go-ahead, then `skills_registry_connect` + `skills_registry_sync`.
- `docs/plans/README.md` and this file's status.

---

## Q0 — parity catch-up for 0.15 (0.15.5)

The 0.15 releases (new-sender hold, digest) missed two parity items:

- IMAP-MCP-CONTEXT.md still says 0.14.2 and lacks `new_sender`,
  `held_messages`, `senders.trusted`, `hold.digest`, the `rules:` block and the
  `run-rules` cache change.
- `rules.hold_digest` and `rules.hold_digest_hour` are not exposed in
  `/api/health` or a stats endpoint (Configuration rule).

Work: update the context doc; add a `rules` block to health (digest on/off,
hour, held awaiting digest, last digest time; counts only). Tests for the
health fields. No decisions needed.

---

## Q1 — reply tracking (0.16.0)

Two lists: **needs reply** (a conversation whose latest message is someone
else's, addressed to you, not answered after N days) and **awaiting reply**
(you sent the latest message and nobody answered after N days).

Builds on: the sync cache (`messages.thread_id`, INBOX and `\Sent` are synced)
and the D28 index's reply pairing; sender roles (skip newsletters, bots).

Design:
- `needs_reply` and `awaiting_reply` MCP tools (read) and
  `GET /api/replies/needed`, `GET /api/replies/awaiting` (read): account,
  `older_than_days` (default 2), limit. Each item: thread id, last message
  ref, counterpart address, last date, days waiting, subject.
- Filters: only conversation/personal halls or senders with role personal or
  colleague; never bots, newsletters or mail held by a rule; a thread you
  archived or flagged done can be dismissed.
- Digest: a "Waiting on you" section in the daily summary (D31), so the
  digest is useful even on days nothing was held.
- Bus: `reply.overdue` is **not** added (the digest covers it) unless D33
  says otherwise.

Open decisions:
- **D32 — data source.** (1) The sync cache (30-day window; no migration;
  older threads drop off). (2) A conversation hash column in the D28 index
  (full history; needs a header rescan like 0.13). *Recommendation: (1) —
  reply tracking is about recent mail, and it ships with no migration.*
- **D33 — dismissing.** (1) Moving the message out of INBOX or flagging it
  `\Answered`/a keyword dismisses it. (2) A `dismiss_reply` tool storing
  dismissals in `imap.db`. *Recommendation: (1) — uses mail-client actions the
  owner already takes.*

Validation: on production, the lists' counts and a spot check of a few thread
ids against Sent; the digest section appears.

---

## Q2 — learn from moves (0.17.0)

When the owner keeps trashing or junking a sender's mail by hand, suggest a
rule; when the owner pulls mail out of Junk or the holding folder, trust the
sender (as the hold's release already does).

Builds on: the header scanner (it already sees a message reappear in another
folder as a duplicate of the D28 index), `held_messages` release detection,
the rule engine and the dry-run preview.

Design:
- **Observe moves.** The scanner records where each indexed message was last
  seen; a message that turns up in Trash/Junk/a configured "unwanted" folder
  after INBOX is a *discard*; one that turns up in INBOX after Junk or the
  holding folder is a *rescue*. Moves made by rules are recorded by the rule
  engine and excluded, so only the owner's own actions count.
- **Today's scan does not see the discard folders:** it skips `\Junk` (and
  `\Drafts`), and on Gmail it reads only All Mail, which excludes Trash and
  Spam. Q2 adds a *location-only* pass over Trash/Junk (and Gmail's
  Trash/Spam): Message-ID hashes and folder, no profile counts, no graph
  edges, no anomaly checks, so spam never pollutes sender statistics.
- **Per-sender feedback counts** in `senders` (discarded, rescued).
- **Suggestions.** A sender (or domain, when several addresses share it) with
  N discards and no rescues becomes a suggested rule: the exact rule
  (`from`, action trash or move), its dry-run match count and the evidence.
  `suggest_rules` tool (read) and `GET /api/rules/suggestions`; accepting
  one is an ordinary `create_rule` (inactive first, previewed).
- **Digest:** a "Suggested rules" section.
- **Rescues** set `senders.trusted` and close open anomalies for that sender.
- Event: `rule.suggested` with counts only, if D35 keeps events.

Open decisions:
- **D34 — how moves are observed.** (1) A location column in the D28 index,
  updated by the scanner (catches moves on every scan, all folders). (2) The
  sync cache's message.deleted/message.synced events (INBOX/Sent only,
  30-day window). *Recommendation: (1) — durable, and with the location-only pass it
  covers Trash/Junk, which the cache does not sync.*
- **D35 — suggestion threshold and form.** (1) Fixed: 3 discards, no rescues,
  within 90 days, configurable. (2) Ratio-based (discarded / received ≥ 0.8
  with a minimum). *Recommendation: (1) — predictable and easy to explain in
  the digest.*
- **D36 — automatic rules.** (1) Suggestions only; a rule is created only when
  the owner or an agent accepts one. (2) Auto-create inactive rules. (3)
  Auto-create active rules. *Recommendation: (1) — the owner stays in charge;
  the digest makes accepting cheap.*

Validation: count discards/rescues observed after a scan; suggestions with
match counts on production; accept one and confirm the preview matches.

---

## Q3 — second opinion for borderline mail, and payment-request alerts (0.18.0)

Today a new sender's message with exactly one bulk/scam signal defers to the
general classification hall. Replace that with a dedicated model check, and
use the same check for the backlog's "asks for payment" anomaly (business
email compromise: changed bank details, gift cards, credential requests).

Builds on: `ClassifyWhenIdle` (gated, never delays new-mail enrichment), the
anomaly detector (D22), the hold (D30), Rspamd's lesson that a model must be
one signal among several (its false-positive rate alone was >5%), and the
ChatSpamDetector finding that models judge better with authentication
results in the prompt.

Design:
- **Verdict job (daemon).** For INBOX mail from new or unusual senders that
  scores exactly one signal, or whose body the extraction step flags as asking
  for money or credentials, the scanner asks the classify model with a fixed
  prompt: From/Reply-To/Return-Path domains, Authentication-Results summary,
  subject, the header signals already found, and a short quoted-reply-stripped
  body excerpt. The body is untrusted; the answer is parsed into a fixed
  schema only: `{verdict: legitimate|bulk|scam, asks: none|payment|credentials|gift_card|bank_change, confidence 0-1, reasons[]}`.
- **Stored per message** in `imap.db` (`message_verdicts`: message hash,
  verdict, asks, confidence, model, time; no content), so `run-rules` (no
  model) reads it.
- **Hold:** score 1 + verdict scam/bulk with confidence ≥ threshold → held,
  reason "model: …"; legitimate → stays; no verdict yet → stays, judged next
  run.
- **Anomaly:** `asks` ≠ none from a sender who is new, or whose usual mail
  never asked → `payment_request` / `credential_request` anomaly (severity
  high when combined with a lookalike domain, Reply-To mismatch or auth
  failure), published as `anomaly.detected` (D16 fields only).
- Budget: per-tick cap (`intelligence.verdicts_per_tick`), gated like other
  optional model work.

Open decisions:
- **D37 — what the model may decide.** (1) Tie-breaker only: score-1 messages
  and anomaly confirmation. (2) It may also release score-2 messages it
  judges legitimate. *Recommendation: (1) — headers decide the clear cases,
  the model only breaks ties (the Rspamd lesson).*
- **D38 — body access.** (1) Cached body excerpt only (recent mail; nothing
  fetched). (2) Fetch the body over IMAP (PEEK) when not cached. *Recommendation:
  (1) — new mail is cached within minutes; no extra fetches of untrusted
  content.*

Validation: run the verdict job over the current borderline set on
production; compare verdicts with the operator's judgement on a sample
(sender + verdict only); check anomaly counts; no message moved until the
operator has seen the preview.

---

## Q4 — safe one-click unsubscribe (0.19.0)

Unsubscribe from bulk senders the RFC 8058 way, then archive their mail.

Builds on: `detect_subscriptions`, sender profiles (`list_count`), the
subscription-audit workflow, rules.

Design:
- `unsubscribe` tool (send scope or a new `unsubscribe` scope, see D39) and
  `POST /api/unsubscribe`: account, sender (or message uid), `dry_run`
  (default true), `archive_to`.
- **Only RFC 8058 one-click:** the newest message from the sender must carry
  `List-Unsubscribe` with an `https:` URI and `List-Unsubscribe-Post:
  List-Unsubscribe=One-Click`, and a DKIM signature that verifies and covers
  both headers. Then one HTTPS POST with body `List-Unsubscribe=One-Click`, no
  cookies, no auth, no redirects to other hosts, a short timeout, and an
  outbound-address guard (no private/loopback addresses).
- **Never** follows links in the body, never fetches pages, never opens
  tracking URLs.
- Dry run shows the method that would be used, or why it can't
  (no header, mailto only, DKIM fails).
- After a 2xx: optional archive of that sender's mail (existing `move_bulk`
  path) and an inactive "watch" rule that reports if mail keeps arriving.
- Audit: every attempt recorded in `imap.db` (sender, method, status code,
  time) and an `unsubscribe.done` event with counts/status only.

Open decisions:
- **D39 — permission model.** (1) A new `unsubscribe` token scope; tool hidden
  without it; each call needs `confirm: true` after a dry run. (2) Reuse the
  `send` scope. *Recommendation: (1) — it is outbound network activity on the
  owner's behalf and deserves its own grant.*
- **D40 — mailto-only senders.** (1) Not supported (report "mailto only").
  (2) Send the unsubscribe email via `send_message` after explicit approval.
  *Recommendation: (1) first; mailto can follow later under the existing
  "no outbound mail without approval" rule.*
- **D41 — DKIM verification.** (1) Verify in imap-mcp with
  `github.com/emersion/go-msgauth` (MIT; new dependency; needs DNS). (2) Trust
  the receiving server's Authentication-Results. *Recommendation: (1) — the
  header must be covered by the signature, which Authentication-Results does
  not say.*

Validation: dry runs against several real newsletters on production (method
and reason only); one real unsubscribe from a sender the operator picks;
confirm no further mail after a few days.

---

## Q5 — screener (0.20.0)

Approve or block held senders from the daily digest, without opening any
folder, and route approved senders the HEY way: inbox, feed (newsletters) or
paper trail (receipts).

Builds on: the hold (D30), the digest (D31), sender trust, rules, and the
trust-gated inbound command channel (`internal/trust`: allowlist, DKIM/DMARC,
HMAC envelope, nonce).

Design:
- The digest numbers each held sender (`[3] promo@shop.example — bulk mail
  …`) and carries a per-digest token.
- **Commands:** `approve 3`, `approve 3 feed`, `approve 5 receipts`,
  `block 7`, `release all`. Approve = trust the sender, move its held mail to
  the chosen destination, and (for feed/receipts) create a move rule;
  block = a trash rule for the sender (or domain), previewed in the reply.
- **Destinations** are config: `rules.screener.feed_folder`,
  `rules.screener.receipts_folder`.
- The same commands as an MCP tool (`screen_senders`, write) and REST
  (`POST /api/held/{account}/decide`), so an agent or a dashboard can drive it.
- Result: a short confirmation message APPENDed to INBOX; a
  `screener.decided` event with counts only.

Open decisions:
- **D42 — how a reply is trusted.** (1) Reply from the account's own address,
  passing DKIM/DMARC (Authentication-Results), quoting the digest's token
  (single use, expires with the next digest); no HMAC. (2) The full inbound
  envelope with HMAC (not practical from a phone). (3) No email commands;
  MCP/REST only. *Recommendation: (1) — the token plus own-address DKIM is
  strong enough for actions that only move mail and create rules, and it
  works from any mail client; document the trade-off.*
- **D43 — blocking scope.** (1) Block the address. (2) Block the domain unless
  it is freemail. *Recommendation: (2) with the preview showing the match
  count before it is applied.*

Validation: on production, decide a few held senders by replying to a real
digest; confirm the moves, rules and trust; a forged reply (wrong token, or
not from the owner) is rejected and logged.

---

## Decisions

| ID | Phase | Topic | Status |
|---|---|---|---|
| D32 | Q1 | Reply-tracking data source | Open |
| D33 | Q1 | Dismissing a reply item | Open |
| D34 | Q2 | Observing moves | Open |
| D35 | Q2 | Suggestion threshold | Open |
| D36 | Q2 | Automatic rules | Open |
| D37 | Q3 | What the model may decide | Open |
| D38 | Q3 | Body access for verdicts | Open |
| D39 | Q4 | Unsubscribe permission model | Open |
| D40 | Q4 | mailto-only senders | Open |
| D41 | Q4 | DKIM verification | Open |
| D42 | Q5 | Trusting digest replies | Open |
| D43 | Q5 | Block scope | Open |

## Phases

| Phase | Version | Status | Tested | Validated |
|---|---|---|---|---|
| Q0 | 0.15.5 | Done | Yes | Yes |
| Q1 | 0.16.0 | Planned | | |
| Q2 | 0.17.0 | Planned | | |
| Q3 | 0.18.0 | Planned | | |
| Q4 | 0.19.0 | Planned | | |
| Q5 | 0.20.0 | Planned | | |

## Risks

- **False holds/suggestions** cost the owner's trust in the system: every
  automatic move is previewed first, reversible (move back), and explained in
  the digest; suggestions are never applied automatically (D36).
- **Model output** is untrusted: fixed schema, confidence threshold, and the
  model never decides alone (D37).
- **Outbound requests** (Q4) are new attack surface: one-click POST only, DKIM
  covering the header, address guard, own scope, dry run by default.
- **Email as a control channel** (Q5): single-use tokens, own-address
  DKIM/DMARC, default-deny, and every rejected command logged.
- **Digest length**: sections are capped (top N each) with counts for the rest.
