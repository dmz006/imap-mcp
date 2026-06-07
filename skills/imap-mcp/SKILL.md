---
# --- PAI-compatible base fields ---
name: imap-mcp
description: Manage email over IMAP — triage an inbox, find and unsubscribe from senders, audit a sender's history, bulk-archive, search, and export — using the imap-mcp MCP server.
version: "0.1.0"
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
over IMAP. It is *instructions only* — it does not bundle tools or connect to
anything. The operator decides whether and when imap-mcp is attached to a
session; loading this skill does not establish any mail connection.

## Prerequisite

The `imap-mcp` MCP server must be connected to this session (the operator wires
it in `.mcp.json` — either a stdio command or an HTTP URL like
`http://localhost:8765/mcp`). If the imap-mcp tools below are not available,
stop and tell the operator the server is not attached. Do not attempt to
connect it yourself.

Confirm with `list_accounts` before doing anything else — it returns the
configured accounts and whether each is connected. Every tool takes an optional
`account` parameter; omit it to use the default account.

## Safety rules (read before any mutation)

1. **Destructive operations require explicit confirmation.** `delete_message`,
   `move_message`/`move_bulk` to Trash, `delete_folder`, and `set_flags` with
   `\Deleted` change the user's live mailbox. State exactly what will change
   (which account, folder, how many messages, matched how) and get a yes before
   running it. Never delete or expunge speculatively.
2. **Prefer reversible moves over deletes.** Moving to Archive or a review
   folder is recoverable; expunging is not. When the user says "remove from my
   inbox," default to *move to Archive*, not delete, unless they say delete.
3. **All file output goes through the working-directory sandbox.** Use
   `write_file` / `read_file` / `list_files` — never write mailbox-derived data
   anywhere else. The server rejects absolute paths and `..` traversal. Output
   files (summaries, exports) belong in `working_dir`, never in a repo.
4. **Never echo credentials or full message bodies into shared logs.** Summaries
   should reference senders/subjects/counts, not paste raw content.
5. **Scope bulk actions narrowly.** Always preview the match set (count + a few
   example senders/subjects) before a `*_bulk` call. Search by `from` +
   `subject` to keep the set tight.

## Core workflows

### 1. Triage an inbox

```
list_accounts                         → confirm connection
list_folders { account }              → find INBOX / Archive names (Gmail uses [Gmail]/…)
list_messages { account, folder: "INBOX", limit: 50 }   → newest first, with total count
```

Group what you see by sender and intent, then propose actions (keep / archive /
unsubscribe). Present the plan; let the user approve before mutating.

### 2. Find subscriptions and unsubscribe

```
detect_subscriptions { account }      → scans List-Unsubscribe headers, groups by sender
```

Produce a summary table (sender, count, unsubscribe method) with `write_file`
into `working_dir` so the user can review. For senders the user wants gone:
- Follow the `List-Unsubscribe` action where present (note it; the user performs
  web unsubscribes — you surface the link, you don't click external URLs).
- Then clear existing mail with a tightly-scoped `move_bulk` to Archive (or
  Trash only on explicit instruction), per-account (mind Gmail's `[Gmail]/Trash`).

### 3. Audit a sender

```
search_messages { account, from: "noreply@example.com" }
get_sender_history / get_sender_profile   (if enrichment is enabled)
```

Summarize volume over time, first/last seen, and typical subjects. Useful before
deciding to block, unsubscribe, or filter.

### 4. Bulk archive / clean up

```
search_messages { account, from, subject, before: "<date>" }   → preview the set
# show the count + sample, get approval, THEN:
move_bulk { account, from, subject, to: "Archive" }
```

### 5. Search

```
search_messages { account, text|from|subject|since|before|flags }
cross_account_search { ... }          → all accounts at once (when implemented)
```

### 6. Export

```
get_message { account, folder, uid }  → full message
export_message / write_file           → save to working_dir for the record
```

## Tool quick reference

- **Accounts/sync:** `list_accounts`, `sync_account`
- **Folders:** `list_folders`, `create_folder`, `delete_folder`
- **Read:** `list_messages`, `get_message`, `get_headers`, `get_thread`, `get_attachments`, `export_message`
- **Write (mutations — confirm first):** `move_message`, `copy_message`, `delete_message`, `set_flags`, `append_message`, `move_bulk`, `flag_bulk`
- **Search:** `search_messages`, `cross_account_search`, `semantic_search`
- **Intelligence:** `summarize_folder`, `detect_subscriptions`, `get_sender_history`, `get_sender_profile`, `kg_query`, `get_anomalies`, `enrichment_status`, `trigger_enrichment`
- **File output (sandbox):** `write_file`, `read_file`, `list_files`, `delete_file`

Some intelligence tools require the optional Ollama-backed enrichment pipeline.
If one returns "not yet implemented," fall back to the IMAP-level tools
(`search_messages`, `list_messages`) and summarize manually.
