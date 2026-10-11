---
# --- PAI-compatible base fields ---
name: imap-mcp
description: Manage email over IMAP — triage an inbox, find and unsubscribe from senders, audit a sender's history, bulk-archive, purge, label, search across accounts, follow threads, save attachments, export mail, track replies you owe and are owed, run cleanup rules and send mail — using the imap-mcp MCP server.
version: "0.17.0"
tags:
  - email
  - imap
  - mail
  - triage
  - productivity

# --- Community required fields ---
author: dmz006
author_url: https://github.com/dmz006
contributor_notes: "Companion skill for the imap-mcp MCP server (https://github.com/dmz006/imap-mcp). Teaches an agent the safe workflows for managing a mailbox over IMAP. Instructions only — it bundles no tools and opens no connection; the operator decides if and when imap-mcp is attached to a session."
license: MIT
category: comms
datawatch_min_version: "8.0.0"

# --- datawatch optional extensions ---
compatible_with: [datawatch>=8.0.0]
requires: []
applies_to:
  agents: [claude-code]
  session_types: []          # empty = any
  comm_channels: []          # empty = any
cost_hint: low
guardrail_profile: email-mutations
---

# Using imap-mcp

This skill teaches you how to drive the **imap-mcp** MCP server to manage email
over IMAP. It is *instructions only*: it bundles no tools and connects to
nothing. The operator decides whether and when imap-mcp is attached to a
session; loading this skill does not open any mail connection.

## Prerequisite

The `imap-mcp` MCP server must be connected to this session. The operator wires
it in `.mcp.json`, either as a stdio command or as an HTTP URL such as
`http://localhost:8765/mcp`. With the server key `imap-mcp`, tools appear as
`mcp__imap-mcp__<tool>`. If the imap-mcp tools are not available, stop and tell
the operator the server is not attached. Do not try to connect it yourself.

**Access is token-scoped over HTTP.** Each token carries scopes: `read`,
`write` (mailbox, rules and sandbox changes), `send` and `admin`. You only see
the tools your token allows. If a tool you need is missing, or a call fails
with `forbidden: tool "..." requires scope "..."`, tell the operator which
scope is missing. Do not work around it, and never look for, read or reuse
another client's token. Over stdio there is no token and every tool is
available.

Start with `list_accounts`. It returns the configured accounts and whether
each is connected. Most tools take an optional `account`; omit it to use the
default account.

## Safety rules (read before any mutation)

1. **Destructive operations require explicit confirmation.** `delete_message`,
   `purge_sender`, `empty_trash`, `move_message`/`move_bulk` to Trash,
   `delete_folder`, `set_flags`/`flag_bulk` with `\Deleted`, rules with
   action `trash`, and `run_rules` without `dry_run` all change the live
   mailbox. State exactly what will change (account, folder, how many
   messages, matched how) and get a yes first. Never delete speculatively.
2. **Irreversible tools need a second, specific yes.** `empty_trash`,
   `purge_sender` with `permanent: true` and `delete_message` with
   `permanent: true` expunge mail for good.
3. **Prefer reversible moves over deletes.** When the user says "remove from my
   inbox", default to moving to Archive, not deleting, unless they say delete.
4. **Preview before every bulk action.** Run `search_messages` with the same
   sender/subject first and show the count plus a few examples. `run_rules`
   defaults to `dry_run: false`, so pass `dry_run: true` first.
5. **All file output goes through the working-directory sandbox.** Use
   `write_file` / `read_file` / `list_files` / `delete_file`. The server
   rejects absolute paths and `..`. Never write mailbox data anywhere else.
6. **Never echo credentials or full message bodies into shared logs.**
   Summaries reference senders, subjects and counts.
7. **Sending mail needs explicit approval of the exact recipients, subject
   and body.** `send_message` sends immediately.

## Core workflows

### 1. Triage an inbox

```
list_accounts                                         → confirm connection
list_folders { account }                              → find INBOX / Archive names (Gmail uses [Gmail]/…)
top_senders { account, folder: "INBOX", top: 30 }     → biggest senders across the whole folder
list_messages { account, folder: "INBOX", limit: 50 } → newest first, with total count
```

Group by sender and intent, propose actions (keep / archive / unsubscribe /
purge) and let the user approve before mutating. `top_senders` with
`group_by: "domain"` groups by sender domain.

### 2. Find subscriptions and unsubscribe

```
detect_subscriptions { account, folder: "INBOX" }   → senders with List-Unsubscribe, grouped
```

Omit `account` to scan every account. Write a summary table (sender, count,
unsubscribe method) with `write_file` so the user can review. For senders the
user wants gone:

- Surface the `List-Unsubscribe` link. The user performs web unsubscribes; do
  not open external URLs yourself.
- Then clear existing mail with `move_bulk` to Archive, or `purge_sender` to
  Trash on explicit instruction.

### 3. Audit a sender

```
get_sender_profile { address: "noreply@example.com" }                       → all history: role, first/last contact, counts each way, reply time, DKIM/DMARC
search_messages { account, folder: "INBOX", from: "noreply@example.com" }   → live, with total_matches
get_sender_history { address: "noreply@example.com" }                       → cached mail only (sync window): subjects
```

Summarize the role (and `role_source`), volume, first/last contact, whether
the user ever replies, and typical subjects. If `scan_complete` is false, say
the counts are still partial.

### 4. Bulk archive or purge

```
search_messages { account, folder: "INBOX", from: "news@example.com" }   → preview count + samples
# show the count and samples, get approval, THEN one of:
move_bulk { account, folder: "INBOX", query: "news@example.com", destination: "Archive", limit: 500 }
purge_sender { account, folder: "INBOX", from: "news@example.com" }      → all matches to Trash
```

`move_bulk` moves at most `limit` messages (default 100), the newest matches
first; repeat until "no messages matched". `purge_sender` loops until the
folder has no matches and finds Trash automatically.

### 5. Label (Gmail)

```
label_message { account, folder: "INBOX", uid: 4242, label: "Receipts" }
label_bulk { account, folder: "INBOX", from: "billing@example.com", label: "Receipts" }
```

Labels are applied by IMAP COPY into the label mailbox; the original stays in
place. The label is created if missing unless `create: false`.

### 6. Recurring cleanup with rules

```
create_rule { name: "old-newsletters", from: "news@example.com", older_than_days: 30, action: "trash" }
run_rules { id: <id>, dry_run: true }    → match counts only
run_rules { id: <id> }                   → apply (after approval)
list_rules                               → rules with run counts
delete_rule { id: <id> }
```

A rule needs at least one condition (`from`, `subject`, `text`,
`older_than_days`, `new_sender`). Actions: `trash`, `move` (needs `dest`),
`flag` (needs `flags`), `seen`. `folder` defaults to `INBOX`. Omitting `id`
runs every active rule. The operator can schedule the same rules with
`imap-mcp run-rules`.

**Spam that changes domain every message** (imap-mcp 0.15+): use a
`new_sender` rule instead of chasing domains. It holds mail from senders with
no history only when the headers also look like bulk mail or a scam (a
borrowed brand name, not addressed to the owner, bulk headers, a throwaway
domain). Replies to the owner's mail and introductions from known contacts
always stay.

```
create_rule { name: "hold-new-senders", account: "<acct>", new_sender: true,
              action: "move", dest: "<holding folder>", active: false }
run_rules { id: <id>, dry_run: true }   → matched + preview: [{sender, reasons}]
```

Show the operator the preview and ask which senders to keep before
activating. Held mail is moved, never deleted; moving a message back to the
inbox trusts its sender for good. A daily "Held for review" summary appears in
the inbox, so nobody has to watch the folder. Until the account's history scan
is complete the rule matches nothing and says why.

Rules learned from the user's own moves:

```
suggest_rules { }                         → senders/domains the user mostly trashes or junks: evidence, exact rule, INBOX matches
create_rule { ...suggestion.rule, active: false } → then run_rules { id, dry_run: true } before activating
dismiss_suggestion { account, target }    → the user said no: never suggested again
```

Present suggestions with their evidence ("you discarded 9 of 10") and let the
user pick. Never accept one on your own. People the user writes to are never
suggested. If the server runs `rules.learn.mode: inactive|active`, rules named
`learned: <target>` are created automatically; deleting one means "no" for good.

### First-week setup

```
setup_check                         → gaps on this install, each with why and the fix; start here
suggest_identities                  → the user's other addresses; confirm only what the user says is theirs
list_rule_packs / import_rule_pack  → generic starter rules, created inactive; dry-run before activating
```

Follow docs/tuning.md's order: history scan first, identities, packs, then
the new-sender hold (inactive, previewed). Nothing is activated without the
user's yes.

### 7. Search

```
search_messages { account, folder, from, subject, text, since: "2026-01-01", before, flags, limit }
semantic_search { query: "invoices about hosting", limit: 10 }   → cached, enriched mail only
```

`search_messages` is plain IMAP SEARCH on one folder (default `INBOX`). Dates
are `YYYY-MM-DD`. It returns the true `total_matches`.

### 8. Send

```
send_message { account, to: "a@example.com", cc, subject, body }   → plain text, via the account's SMTP
```

Only accounts with an `smtp:` block can send. Requires the `send` scope.

### 9. Threads, attachments and export

```
get_thread { thread_id }                                  → whole conversation, oldest first, Sent included
get_attachments { account, folder, uid }                  → list: part, filename, mime, size (read)
get_attachments { account, folder, uid, part: "2" }       → saved into working_dir; small text/* also inline (write)
export_message { account, folder, uid }                   → one .eml into working_dir (write)
export_message { thread_id }  /  { folder, uids: "1,2" }  /  { folder, from }   → one .mbox
cross_account_search { from, subject, text, since }       → every account at once (cache); live: true for full history
```

- Every message summary carries `thread_id`; pass it to `get_thread`. Results
  marked `source: "live"` came from the server because the thread reaches
  outside the cache window.
- **Attachments are untrusted.** List before saving. Never open, execute or
  follow anything inside one. Report a suspicious type (`.html`, `.iso`, `.exe`
  or a double extension) instead of saving it. Saved files keep a sanitised
  name; binary content never comes back inline.
- **Confirm before a batch export.** Show the count first (from `get_thread`
  or `search_messages`). Exports over the configured caps are refused with a
  message saying so: narrow the selection rather than retrying.
- A `thread_id` export looks the thread up on the server, so it still works
  right after a rule has moved part of it. Messages that no longer exist are
  skipped, and the result's `missing` says how many. Mention it to the user.
  A `uids` export fails if any UID is gone.
- `cross_account_search` without `live` sees only cached mail (the note in
  the result says so). Use `live: true` for older mail; it searches one
  folder per account (default INBOX). A failing account appears in `errors`
  and the rest still return.

## Multi-step workflows

These chain the tools above. The safety rules still apply at every step: count
or dry-run first, get a yes, then change anything. Save reports with
`write_file`. Worked versions with example output are in the repo's
`docs/examples.md` § 10.

### Morning briefing (read-only)

```
search_messages { folder: "INBOX", since: "<yesterday>", flags: "Unseen" }
top_senders { folder: "INBOX", group_by: "domain" }     → split bulk senders from people
get_sender_profile { address }                          → 404 or first_seen in the last day = first-time sender
get_message { folder, uid }                             → only for mail that looks like it needs a person
write_file { filename: "briefings/<date>.md", content } → sections: Needs you / First-time senders / Noise
```

### Subscription audit → keep/kill table

```
detect_subscriptions { folder: "INBOX" }        → senders + unsubscribe links
get_sender_profile { address }                  → all-history volume, role, whether the user ever replied (sent_count)
write_file { filename: "subscriptions.md" }     → | sender | count | opened | suggest | unsubscribe |
```

The user edits the "suggest" column. Then `read_file` it back. For each row
marked for removal: `search_messages` count → approval → `purge_sender` or
`move_bulk` → `create_rule` so the sender doesn't come back. Never open
unsubscribe links yourself.

### Teach by example

```
semantic_search { folder: "INBOX", reference_uid: <uid>, threshold: 0.8 }   → mail that means the same thing
```

Group the hits by sender and show counts. The user removes false positives.
Then `label_bulk` or `move_bulk` per sender. Rules match text, not meaning, so
turn each confirmed sender into `create_rule { action: "move", dest }` and
show `run_rules { dry_run: true }`. Check `enrichment_status` first: an
unenriched folder gives thin results.

### Rule review

```
list_rules                                     → matchers, actions, run counts
run_rules { dry_run: true }                    → current match counts
search_messages { from: <rule.from>, flags: "Flagged" }   → would a trash rule hit flagged mail?
```

Report:
- dead rules (0 matches);
- overlaps (one sender, two rules);
- risky rules (flagged mail in the dry run);
- a proposed `delete_rule` list. Delete only on approval.

### Phishing check on one message

```
get_headers { folder, uid }                           → Authentication-Results, Return-Path, Reply-To, Received
get_sender_profile { address }                        → new sender? did its mail use to pass DKIM/DMARC?
semantic_search { folder, reference_uid: <uid> }      → compare with genuine mail from the brand
```

A known sender (`message_count` high, `dmarc_pass` high) whose message now
fails DMARC is a strong warning. Give a verdict with reasons: SPF/DKIM/DMARC results, a Reply-To domain that
doesn't match, a new sender, a link host that doesn't match the brand. Never
follow links. On a confirmed "bad", `move_message` it to Junk (with approval).

### Draft replies without sending

Read the conversation first with `get_thread { thread_id }`, so the draft
answers the latest message. Build a raw RFC 2822 reply (`From`, `To`,
`Subject: Re: …`, `In-Reply-To` and `References` from the thread, body), then:

```
append_message { folder: "Drafts", message, flags: "Draft" }   → Gmail: "[Gmail]/Drafts"
```

The user edits and sends from their own client. This needs `write`, not
`send`. Prefer it over `send_message` unless the user asks you to send.

### Dossier / timeline

```
semantic_search { query: "flight confirmation" }   → omit account to search all accounts
semantic_search { query: "hotel reservation" }
cross_account_search { text: "confirmation", since }   → keyword hits semantic search ranks lower
get_message { folder, uid }                        → pull dates, amounts, confirmation numbers
get_attachments { folder, uid } then { part }      → save tickets/PDFs; a small .ics comes back inline
write_file { filename: "trips/<name>.md" }         → one dated table, each row citing folder + uid
```

The same pattern works for invoices with totals, a project's mail as a
timeline, or "what did we agree and when".

### Hand over a conversation

```
cross_account_search { from: "example.net", subject: "renewal" }   → hit with thread_id
get_thread { thread_id }                                            → count + date range; show the user
export_message { thread_id }                                        → one .mbox (after approval)
get_attachments { folder, uid, part }                               → each attachment beside it
write_file { filename: "handover/README.md" }                       → index of messages and files
```

### Knowledge graph

```
kg_query { entity: "ann@example.com" }                       → her organization, correspondents, co-recipients, threads, projects
kg_query { entity: "apollo", predicate: "works_on" }         → who works on a project (wing tag), strongest first
kg_query { predicate: "deadline" }                           → threads with stated deadlines; properties.due; subject = thread_id for get_thread
kg_query { predicate: "corresponds_with" }                   → your correspondents by volume; old last_seen / current:false = lost touch
```

Predicates: `belongs_to`, `corresponds_with`, `cc_with`, `is_subscription`,
`participates_in`, `works_on`, `discusses` come from headers and tags
(confidence 1.0). `manages`, `reports_to`, `works_at`, `deadline` and some
`works_on` come from the model reading recent mail (confidence 0.6). Say which
facts are read from mail rather than certain. Names from model extraction
are written as they appeared, not addresses.

### Replies

```
needs_reply { older_than_days: 2 }                  → conversations waiting on the user, longest first, whole history
awaiting_reply { older_than_days: 5 }               → the user wrote last and nobody answered
get_thread { thread_id }                            → read the conversation before drafting an answer
dismiss_reply { account, thread_id }                → only when the user says they won't reply (write scope)
```

needs_reply lists only people the user has written to (first-time senders
come back once a model second opinion exists), and never invites,
notifications, no-reply senders or bare forwards. If the user's other
addresses (work, Kindle) show up as people, run `suggest_identities` and ask
the user to confirm them (`confirm_identity`); never confirm one yourself.

A reply the user sends, or `\Answered`, clears an item by itself; where the
mail is filed does not. `history_complete: false` means the history scan is
still running and older conversations may be missing. Never send a reply
without the user's approval; draft it and let them send.

### Anomalies

```
get_anomalies { severity: "high" }                  → auth_failure, lookalike_domain: folder, uid, message_ref to open the message
get_anomalies { sender: "billing@example.com" }     → everything flagged for one sender
get_anomalies { type: "silence" }                   → regular correspondents gone quiet
resolve_anomaly { id }                              → mark reviewed (write scope); only after the user agrees
```

Types: `new_sender` (low), `auth_failure` (high), `lookalike_domain` (high),
`reply_to_mismatch` (medium), `silence` (low), `volume_spike` (medium).
Detection runs only after the first history scan finishes, and checks new
mail. Treat high findings as "inspect before trusting": never follow links in
the message, and don't resolve a finding on your own judgement.

### Scheduled (unattended) sessions

When you run on a schedule with nobody watching:
- Read, analyse and write reports or proposals to files: `proposals/rules.md`,
  `briefings/<date>.md`.
- Do **not** mutate the mailbox, create rules or send mail. Leave that for an
  interactive session where the user can approve.
- Deterministic cleanup belongs to the operator's `imap-mcp run-rules` job,
  not to you.

## Tool quick reference

| Group | Tool | Parameters (required in **bold**) | Scope |
|-------|------|-----------------------------------|-------|
| Accounts | `list_accounts` | none | read |
| | `sync_account` | `account` | admin |
| Folders | `list_folders` | `account` | read |
| | `create_folder` | **`path`** (alias `folder`), `account` | write |
| | `delete_folder` | **`path`** (alias `folder`), `account`; folder must be empty | write |
| Read | `list_messages` | `account`, `folder` (INBOX), `limit` (50, max 200), `offset`, `order` (`asc`/`desc`) | read |
| | `get_message` | **`folder`**, **`uid`**, `account`; body is the raw text section, not MIME-decoded | read |
| | `get_headers` | **`folder`**, **`uid`**, `account` | read |
| | `get_thread` | **`thread_id`**, `account` (omit for all), `live`, `folders` (comma-separated), `limit` (100) | read |
| | `get_attachments` | **`folder`**, **`uid`**, `part` (omit to list), `filename`, `account`; list = read, download = write | read / write |
| | `export_message` | one of `uid` (+ `folder`) → .eml, or `uids` (+ `folder`), `thread_id`, `from` → .mbox; `filename`, `account` | write |
| Write | `move_message` / `copy_message` | **`folder`**, **`uid`**, **`destination`**, `account` | write |
| | `delete_message` | **`folder`**, **`uid`**, `account`, `permanent` | write |
| | `set_flags` | **`folder`**, **`uid`**, `add`, `remove` (comma-separated, e.g. `Seen,Flagged`), `account` | write |
| | `append_message` | **`folder`**, **`message`** (raw RFC 2822), `flags`, `account` | write |
| | `move_bulk` | **`folder`**, **`query`** (From substring), **`destination`**, `limit` (100), `account` | write |
| | `flag_bulk` | **`folder`**, **`query`** (From substring), `add`, `remove`, `limit` (100), `account` | write |
| | `purge_sender` | **`from`** (From substring), `folder` (INBOX), `permanent`, `account` | write |
| Labels / Trash | `label_message` | **`uid`**, **`label`**, `folder` (INBOX), `create` (true), `account` | write |
| | `label_bulk` | **`label`**, `from` and/or `subject`, `folder` (INBOX), `create` (true), `account` | write |
| | `empty_trash` | `account`; permanently deletes everything in Trash | write |
| Send | `send_message` | **`to`**, **`subject`**, **`body`**, `cc`, `account` | send |
| Analytics | `top_senders` | `folder` (INBOX), `top` (30), `scan` (all), `group_by` (`address`/`domain`), `account` | read |
| | `summarize_folder` | **`folder`**, `account`; returns only `total` and `recent` counts | read |
| | `detect_subscriptions` | `folder` (INBOX), `limit`, `account` (omit for all accounts) | read |
| | `kg_query` | `entity` (address, domain, thread id, project or topic; exact), `predicate`, `entity_type`, `limit` (50); strongest first, with `weight`, `last_seen`, `current`, `confidence` | read |
| | `get_anomalies` | `severity`, `type`, `sender`, `account`, `unresolved_only` (true), `limit` (20) | read |
| | `resolve_anomaly` | **`id`** | write |
| Replies | `needs_reply` | `account` (all), `older_than_days` (2), `within_days` (90; 3650 = all history), `limit` (20); items: `thread_id`, `counterpart`, `name`, `subject`, `folder`, `uid`, `message_ref`, `days_waiting`; plus `history_complete` | read |
| | `awaiting_reply` | same as `needs_reply`; you wrote last, `counterpart` = your first recipient | read |
| | `dismiss_reply` | **`account`**, **`thread_id`**; back when a newer message arrives | write |
| Identity | `suggest_identities` | none; `{candidates: [{address, evidence}], known}` | read |
| | `confirm_identity` / `reject_identity` | **`address`** (address or `@domain`); only on the user's word | write |
| | `get_sender_profile` | **`address`**; all history: `role`, `role_source`, `first_seen`/`last_seen`, `message_count`, `sent_count`, `reply_count`, `avg_reply_seconds`, list/bulk/auto and DKIM/DMARC counts, `scan_complete` | read |
| | `get_sender_history` | **`address`**, `limit` (100), `account` (omit for all); cache only | read |
| Rules | `create_rule` | **`name`**, **`action`** (`trash`/`move`/`flag`/`seen`), `from`, `subject`, `text`, `older_than_days`, `new_sender`, `new_sender_days` (30), `dest`, `flags`, `folder`, `account`, `description`, `active` (true) | write |
| | `list_rules` | none | read |
| | `delete_rule` | **`id`** | write |
| | `run_rules` | `id` (all active), `dry_run` (false) | write |
| | `suggest_rules` | `account` (all), `limit` (50); `{mode, suggestions: [{target, kind, received, discarded, ratio, action, dest, matches, status, rule}], history_complete}` | read |
| | `dismiss_suggestion` | **`account`**, **`target`** (address or `@domain`) | write |
| | `list_rule_packs` | none; built-in packs and their rules | read |
| | `import_rule_pack` | **`name`**, `account`, `dest`; rules created inactive | write |
| Setup | `setup_check` | none; `{findings: [{id, account, severity, what, why, fix}]}` | read |
| Search | `search_messages` | `folder` (INBOX), `from`, `subject`, `text`, `since`, `before`, `flags`, `limit` (50), `account` | read |
| | `semantic_search` | `query` or `reference_uid`, `folder`, `limit` (10), `threshold` (0.7), `account` (omit for all) | read |
| | `cross_account_search` | `from`, `subject`, `text`, `since`, `before`, `limit` (20 per account), `live`, `folder` (live; INBOX) | read |
| Enrichment | `enrichment_status` | `account` (accepted, ignored; reports all) | read |
| | `trigger_enrichment` | `limit` (50); `account` accepted, ignored | admin |
| Cache | `cache_sweep` | `account`, `folder`, `older_than_days`, `errors_only`, `all`, `dry_run` (true) | admin |
| Sandbox | `write_file` | **`filename`**, **`content`** | write |
| | `read_file` | **`filename`** | read |
| | `list_files` | `subdir` | read |
| | `delete_file` | **`filename`** | write |

`search_messages` accepts `hall`, `wing` and `room` but ignores them.

## Cache and enrichment

imap-mcp keeps a local cache of recent mail: by default INBOX and Sent for the
last 30 days, as configured by the operator. Sync is read-only against the
server. `sync_account` (admin) refreshes it now and reports per folder how many
messages are cached, new or removed. `semantic_search` and
`get_sender_history` only see mail inside that window; for older mail use the
live tools (`search_messages`, `list_messages`).

Enrichment (embeddings and classification) runs in the background. New mail is
processed first; older backfill mail is rate-limited and pauses while other
work uses the GPU. `enrichment_status` shows queue depth, throughput and any
pause or backoff reason; use it to explain incomplete semantic results.
`trigger_enrichment` (admin) forces a run.

`cache_sweep` (admin) only touches the local cache, never the mailbox. Run it
with the default `dry_run` first and show the operator the counts.

## REST-only features (admin token)

- `POST /api/query`: JSON questions over the cache, for example top sender
  domains or unread counts per folder. See `docs/query.md`.
- `/api/webhooks`: push notifications. Payloads carry IDs only; fetch details
  with your own token. See `docs/webhooks.md`.

Prefer the MCP tools when they cover the task.
