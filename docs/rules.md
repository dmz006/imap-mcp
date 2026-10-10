# Rules

A rule is a saved IMAP search plus one or more actions. Rules live in the
state database (`imap.db`). They run when you call them, or on a schedule
through the `imap-mcp run-rules` CLI. Use them for recurring mail you always
handle the same way, such as newsletters you file or promotions you trash, so
you don't have to redo the cleanup by hand.

Rules act on the **mailbox**, not the local cache. Every run issues a live
`UID SEARCH` against the server.

## The rule model

```json
{
  "id": 3,
  "name": "promo-trash",
  "description": "Weekly promotions",
  "conditions": {
    "account": "personal",
    "folder": "INBOX",
    "from": "deals.example.com",
    "subject": "",
    "text": "",
    "older_than_days": 7
  },
  "actions": [{"type": "trash"}],
  "active": true,
  "priority": 100,
  "run_count": 42
}
```

### Conditions

All non-empty conditions are combined with AND into one IMAP `UID SEARCH`.

| Field | IMAP criterion | Notes |
|-------|----------------|-------|
| `from` | `HEADER From <value>` | |
| `subject` | `HEADER Subject <value>` | |
| `text` | `BODY <value>` | |
| `older_than_days` | `BEFORE <today − N days>` | |
| `new_sender` | `SINCE <today − new_sender_days>`, then a per-message check | Mail from first-time senders that looks like bulk mail or a scam. See [The new-sender hold](#the-new-sender-hold). |
| `new_sender_days` | Window for `new_sender` | Default 30. Needs `new_sender`. |
| `folder` | Folder to search and act in | Default `INBOX`. Use the real mailbox name, for example `[Gmail]/Spam`. |
| `account` | Account to use | Default: the default account |

`folder` and `account` choose where the rule runs. They do not filter
messages.

### Actions

| `type` | Extra field | Effect on every matched message |
|--------|-------------|---------------------------------|
| `trash` | | Moves to the account's Trash. The `\Trash` SPECIAL-USE folder is tried first, then `[Gmail]/Trash`, `Trash` or `*/Trash`. |
| `move` | `dest` (required) | Moves to `dest` |
| `flag` | `flags` (required) | Adds flags, comma-separated. The backslash is optional: `flagged` or `\Flagged`. |
| `seen` | | Adds `\Seen` |

Moves use `MOVE` when the server supports it. Otherwise the server copies the
messages and then deletes exactly those UIDs. A folder-wide `EXPUNGE` is never
issued if it would remove other `\Deleted` mail.

Actions run in order on the same UID set. Put `flag` and `seen` **before**
`move` and `trash`. After a move, the UIDs are no longer in the folder, so any
action after it fails. Results report only the first action's type.

### Other fields

| Field | Default | Notes |
|-------|---------|-------|
| `name` | | Required and unique |
| `active` | `true` | Inactive rules are skipped by `run_rules` and `run-rules` |
| `priority` | `100` | Lower runs first. Ties run in id order. `0` is stored as `100`. |
| `run_count` | `0` | Read-only. Counts the messages actioned across real runs. |

## Validation

The server rejects a rule with 400 (REST) or a tool error (MCP) when:

- `name` is empty or whitespace.
- `actions` is empty.
- An action `type` is not `trash`, `move`, `flag` or `seen`.
- A `move` action has no `dest`, or a `flag` action has no `flags`.
- None of `from`, `subject`, `text`, `older_than_days` or `new_sender` is
  set. Without one of them, the rule would match the whole folder.
- `new_sender_days` is set without `new_sender`, or is negative.
- Another rule already has the same `name`.

The server does not check that `account`, `folder` or `dest` exist. A wrong
value shows up as an `error` in the run results.

## MCP tools

| Tool | Scope | Arguments |
|------|-------|-----------|
| `create_rule` | `write` | `name`, `action` (required); `from`, `subject`, `text`, `older_than_days`, `new_sender`, `new_sender_days`, `dest`, `flags`, `account`, `folder`, `description`, `active` (default true) |
| `list_rules` | `read` | none. Returns `{count, rules}`. |
| `delete_rule` | `write` | `id` |
| `run_rules` | `write` | `id` (optional), `dry_run` (default false) |

`create_rule` takes one action. To create a rule with several actions, or to
set a priority, use the REST API. The MCP side has no update or test tool.

## REST routes

| Route | Scope | |
|-------|-------|--|
| `GET /api/rules` | `read` | `{count, rules}` |
| `POST /api/rules` | `write` | Create. Returns 201 `{id, name}`. |
| `PUT /api/rules/{id}` | `write` | Full replacement. `run_count` is kept. |
| `DELETE /api/rules/{id}` | `write` | |
| `POST /api/rules/{id}/test` | `write` | Dry run of one rule |

The REST API has no "run all" route. Use the `run_rules` tool or the CLI.
Every POST, PUT or DELETE needs `Content-Type: application/json`. See
[rest-api.md](rest-api.md#conventions).

```bash
curl -sS -X POST http://127.0.0.1:8765/api/rules \
  -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{
    "name": "receipts-archive",
    "conditions": {"from": "receipts.example.com"},
    "actions": [{"type": "seen"}, {"type": "move", "dest": "Receipts"}]
  }'
```

## Test, dry run and run

| Operation | Rules | Changes mail | Updates `run_count` and fires `rule.fired` |
|-----------|-------|--------------|------------------------------------------|
| `POST /api/rules/{id}/test` | That one rule, active or not | No | No |
| `run_rules` with `dry_run: true`, or `run-rules --dry-run` | All active rules (or `id`) | No | No |
| `run_rules` | All active rules | Yes | Yes, when `matched > 0` |
| `run_rules` with `id` | That rule, **even if inactive** | Yes | Yes, when `matched > 0` |

A dry run searches the server and reports `matched` without touching
anything. Each result looks like this:

```json
{"id": 3, "name": "promo-trash", "matched": 17, "action": "trash"}
```

If a rule fails (for example an unknown folder, a missing account or a server
error), its result includes `error`, and the other rules still run. If the
search succeeded and an action failed, `matched` is the number of messages
the search found. If the folder select or the search failed, it is 0.

Always dry-run a new rule first, and check the count against what you expect.

## The `run-rules` CLI

```
imap-mcp run-rules [--dry-run] [--config PATH]
```

The CLI connects to every account, applies all active rules once, prints a
summary and exits. It opens only the state database, never the mail cache,
so it needs only `db.encryption_key` when encryption is on. It does not use
`server.auth`. It has a 15-minute timeout.

```
  promo-trash            trash  x17
  receipts-archive       seen   x3
  old-alerts             trash  x0  ERROR: select Alerts: ...
run-rules: 3 active rules, 20 messages actioned (dry_run=false)
```

The CLI exits 0 even when single rules report errors. It exits non-zero only
when the config, the database or the rules list cannot be loaded. If an
account fails to connect, the CLI logs the failure and carries on. Check the
output for `ERROR:` lines.

### Scheduling

Run it hourly from cron, a systemd timer or a datawatch schedule. If the
config uses `${secret:...}` or `${ENV}` references, the scheduled environment
must provide them. [deployment.md](deployment.md) shows a wrapper script and
the schedule.

```cron
0 * * * * /path/to/imap-mcp run-rules --config ~/.config/imap-mcp/config.yaml >> ~/.local/share/imap-mcp/run-rules.log 2>&1
```

The CLI can run while `imap-mcp serve` is running. Both use their own IMAP
connections.

## The new-sender hold

Domain and display-name rules cannot keep up with spam that uses a new domain
for nearly every message. What that mail has in common is a sender with no
history. A `new_sender` rule moves it to a holding folder, but only when its
headers also look like bulk mail or a scam. Real first contacts stay in the
inbox (AGENT.md D30).

```
create_rule { name: "hold-new-senders", account: "work", new_sender: true,
              action: "move", dest: "Held" }
```

**A sender is new** when none of these is true:

- it is one of the account's own addresses;
- you have sent mail to it, or replied to it;
- it wrote to you before the window (`new_sender_days`, default 30);
- you released one of its messages from the hold before;
- it is at a domain you have written to (shared mail providers such as
  gmail.com do not count).

This comes from the intelligence scan's sender profiles
([intelligence.md](intelligence.md)), across all accounts. Until the account's
history scan is complete the rule matches nothing and reports why, because
every sender would look new.

**A new sender's message is held** when its header score is 2 or more:

| Signal | Points |
|---|---|
| The display name borrows a brand, a government agency, an address or your own domain, and the mail comes from an unrelated domain ("QuickBooks" from `shop.example`) | 2 |
| Not addressed to you: no recipients, only the sender, or only other people's freemail addresses | 1 |
| Bulk headers: `List-Unsubscribe`, `List-Id` or `Precedence: bulk/list/junk` | 1 |
| A throwaway-looking domain: digits mixed into the name, or a low-cost TLD such as `.shop` | 1 |
| Your address in the subject | 1 |
| `Reply-To` at a different domain | 1 |

With exactly 1 point the classify model decides: the message's enrichment
hall. `conversation` or `personal` stays; any other hall is held. A message
not classified yet stays, and the next run judges it again.

**These always stay**, whatever the score:

- a reply to mail you sent (`In-Reply-To` or `References` matches a message in
  your Sent history);
- a message that copies someone you have written to, such as an
  introduction.

Only headers are read (with PEEK, so nothing is marked read). Authentication
results are not used: spam from throwaway domains usually passes DKIM.

**Previewing.** A dry run lists who would be held and why, without moving
anything:

```
run_rules { id: 42, dry_run: true }
→ {"results":[{"id":42,"matched":3,"preview":[{"sender":"info@shop5x.example",
   "reasons":"bulk mail you never signed up for; throwaway-looking domain"}, …]}]}
```

**Releasing.** Held mail is moved, never deleted. To release a message, move
it back to the inbox. On the next run the server sees it there, marks the
sender as trusted, and never holds that sender again.

### The daily digest

So you do not have to keep checking the holding folder, the first full rule
run (the hourly `run-rules` job) at or after `rules.hold_digest_hour` (local
time, default 8) sends a digest of everything held since the last one
(AGENT.md D31). Nothing is sent when nothing was held.

- **A summary message in the account's INBOX.** It is APPENDed over IMAP, not
  sent: from and to the account's own address, listing each held message's
  sender, subject, time and reasons, and how to release one.
- **A `hold.digest` event**, `{"held": 3}` with the event's `account`, for
  webhooks and dashboards. It never carries addresses or subjects.

```yaml
rules:
  hold_digest: true      # default true
  hold_digest_hour: 8    # 0-23, local time; default 8
```

Environment: `IMAP_MCP_RULES_HOLD_DIGEST`, `IMAP_MCP_RULES_HOLD_DIGEST_HOUR`.
Held-message records are kept for 90 days.

## The `rule.fired` event

After a real run of a rule that matched at least one message, the server
publishes `rule.fired`:

```json
{"type": "rule.fired", "account": "personal", "payload": {"rule_id": 3, "action": "trash", "matched": 17}}
```

`account` is the rule's `account` condition. It is empty, and omitted, for
rules that use the default account. Dry runs and tests never fire the event.

The event goes on the `/api/events` SSE stream and to webhooks subscribed to
`rule.fired`. The webhook payload carries the same metadata:

```json
{
  "delivery_id": "<id>",
  "event": "rule.fired",
  "timestamp": "2026-10-09T12:00:00Z",
  "account": "personal",
  "data": {"rule_id": 3, "action": "trash", "matched": 17}
}
```

The `run-rules` CLI writes `rule.fired` straight into the webhook outbox in
`imap.db` before it exits, and the running `serve` process delivers it. The
CLI's events never reach the SSE stream, because that stream belongs to the
`serve` process. See [webhooks.md](webhooks.md).

## Gotcha: whole-token matching

IMAP leaves the details of `SEARCH` matching to the server. Gmail matches
search terms as **whole tokens**, not substrings. On Gmail, `from: "bigbox"`
can match **0** messages, while `from: "bigbox.example"` (or
`deals.bigbox.example`) matches hundreds. Other servers may do substring
matching.

The rule engine sends your values to the server unchanged, so:

- Use the full domain or full address in `from`, not a fragment of a name.
- If a rule you expect to match reports `matched: 0`, try a longer, complete
  token before you assume the mail is gone.
- Dry-run on each server type you use.

`search_messages` and `GET /api/search` behave the same way.

## Example rules

Trash promotions after a week:

```json
{
  "name": "promo-trash-7d",
  "conditions": {"from": "deals.example.com", "older_than_days": 7},
  "actions": [{"type": "trash"}]
}
```

File receipts as read:

```json
{
  "name": "receipts",
  "conditions": {"from": "billing@example.com", "subject": "receipt"},
  "actions": [{"type": "seen"}, {"type": "move", "dest": "Receipts"}]
}
```

Flag mail from a key contact:

```json
{
  "name": "flag-boss",
  "conditions": {"account": "work", "from": "boss@example.com"},
  "actions": [{"type": "flag", "flags": "flagged"}],
  "priority": 10
}
```

Empty old messages out of a Gmail label:

```json
{
  "name": "old-alerts",
  "conditions": {"folder": "Alerts", "from": "alerts.example.com", "older_than_days": 30},
  "actions": [{"type": "trash"}]
}
```

The same as an MCP call:

```
create_rule { name: "promo-trash-7d", action: "trash", from: "deals.example.com", older_than_days: 7 }
run_rules   { dry_run: true }
```

For a guided cleanup that ends with rules, see
[cookbook-inbox-cleanup.md](cookbook-inbox-cleanup.md).
