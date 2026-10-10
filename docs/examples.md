# Examples: what you can do with imap-mcp

Short, runnable recipes for imap-mcp's features: scoped tokens, the sync
cache, `/api/query`, rules, webhooks, semantic search and the event stream
(sections 2–9); threads, attachments, export and cross-account search
(section 11); sender profiles, the knowledge graph and anomalies (sections 12
and 13). Each one links to the reference page with the details.
[Section 10](#10-agent-workflows) puts them together: multi-step workflows an
agent runs for you.

All addresses are `example.com` placeholders and every output is illustrative.
The recipes assume `serve` is running on `127.0.0.1:8765` and that these
environment variables hold tokens with the scopes named
([auth-tokens.md](auth-tokens.md)):

```bash
export IMAP_MCP=http://127.0.0.1:8765
export READ_TOKEN=...    # scopes: read
export WRITE_TOKEN=...   # scopes: read, write  (downloads, exports, resolving anomalies)
export ADMIN_TOKEN=...   # scopes: read, write, admin
```

Every POST/PUT/DELETE needs `Content-Type: application/json` and a JSON body,
even if it is only `{}` ([rest-api.md](rest-api.md#request-guard)).

---

## 1. Ask your agent

With imap-mcp attached to Claude Code (or a datawatch session) you can just
ask. These map to the MCP tools listed in the [README](../README.md#mcp-tools-45):

| You say | Tools the agent uses |
|---------|----------------------|
| "Who sends me the most mail in INBOX? Group by domain." | `top_senders` |
| "Move everything from `deals.example.com` to Trash." | `purge_sender` (confirm the count first with `search_messages`) |
| "Label all mail from `billing@example.com` as Finance." | `label_bulk` |
| "Create a rule that archives `newsletter.example.com` every hour, and show me what it would match first." | `create_rule`, then `run_rules` with `dry_run: true` |
| "Find mail similar to the invoice from yesterday." | `semantic_search` with `reference_uid` |
| "Is enrichment caught up?" | `enrichment_status` |
| "Forget the cached copies of Archive older than 90 days." | `cache_sweep` (dry run first) |
| "Who is billing@example.com to me? Do I ever answer them?" | `get_sender_profile` (role, counts each way, average reply time) |
| "Who works with Ann, and who does she report to?" | `kg_query` with `entity: "ann@example.com"` |
| "Show me the whole conversation this came from." | `get_thread` with the message's `thread_id` |
| "Search all my accounts for anything from example.net about the renewal." | `cross_account_search` (`live: true` for older mail) |
| "Save the PDF from that invoice." | `get_attachments` to list, then with `part` to save |
| "Export that thread so I can forward it to legal." | `export_message` with `thread_id` → `.mbox` |

The [inbox-cleanup cookbook](cookbook-inbox-cleanup.md) walks through a full
cleanup session end to end, and [section 10](#10-agent-workflows) has fifteen
more multi-step workflows.

---

## 2. Mailbox snapshot with `/api/query`

`/api/query` answers questions about the cache with JSON, not SQL
([query.md](query.md)). It needs the `admin` scope. It covers the synced window
(default 30 days).

```bash
q() { curl -sS -X POST "$IMAP_MCP/api/query" \
        -H "Authorization: Bearer $ADMIN_TOKEN" \
        -H "Content-Type: application/json" -d "$1"; }
```

**Unread mail per folder:**

```bash
q '{"view":"messages",
    "where":[{"field":"seen","op":"eq","value":false}],
    "group_by":["account","folder"],
    "aggregate":[{"fn":"count","as":"unread"}],
    "order_by":[{"field":"unread","desc":true}]}'
```

```json
{"view":"messages","columns":["account","folder","unread"],
 "rows":[["work","INBOX",37],["work","Archive",4]],"count":2,"truncated":false}
```

**Top sender domains this week:**

```bash
q '{"view":"messages",
    "where":[{"field":"date","op":"gte","value":"-7d"}],
    "group_by":["from_domain"],
    "aggregate":[{"fn":"count","as":"n"},{"fn":"max","field":"date","as":"latest"}],
    "order_by":[{"field":"n","desc":true}],"limit":10}'
```

**Large attachments (over 5 MB) in the last 30 days:**

```bash
q '{"view":"messages",
    "fields":["account","folder","uid","date","from_addr","size"],
    "where":[{"field":"has_attachments","op":"eq","value":true},
             {"field":"size","op":"gt","value":5000000},
             {"field":"date","op":"gte","value":"-30d"}],
    "order_by":[{"field":"size","desc":true}],"limit":20}'
```

**What enrichment has classified, by hall:**

```bash
q '{"view":"messages",
    "where":[{"field":"enrichment_status","op":"eq","value":"done"}],
    "group_by":["hall"],
    "aggregate":[{"fn":"count","as":"n"}],
    "order_by":[{"field":"n","desc":true}]}'
```

**Enrichment backlog (pending, done, error):**

```bash
q '{"view":"messages","group_by":["enrichment_status"],
    "aggregate":[{"fn":"count","as":"n"}]}'
```

Bodies are never returned unless you name `body_text` or `body_html` in
`fields`.

---

## 3. A daily digest from cron

Combine queries into a plain-text digest with `jq`, and run it from cron or a
datawatch scheduled job ([deployment.md](deployment.md)).

```bash
#!/bin/sh
# digest.sh: unread counts per folder and the top domains of the last day.
set -eu
q() { curl -sS -X POST "$IMAP_MCP/api/query" -H "Authorization: Bearer $ADMIN_TOKEN" \
        -H "Content-Type: application/json" -d "$1"; }

echo "Unread by folder"
q '{"view":"messages","where":[{"field":"seen","op":"eq","value":false}],
    "group_by":["account","folder"],"aggregate":[{"fn":"count","as":"n"}],
    "order_by":[{"field":"n","desc":true}]}' \
  | jq -r '.rows[] | "  \(.[0])/\(.[1]): \(.[2])"'

echo "Top domains, last 24h"
q '{"view":"messages","where":[{"field":"date","op":"gte","value":"-24h"}],
    "group_by":["from_domain"],"aggregate":[{"fn":"count","as":"n"}],
    "order_by":[{"field":"n","desc":true}],"limit":5}' \
  | jq -r '.rows[] | "  \(.[0]): \(.[1])"'
```

```text
Unread by folder
  work/INBOX: 37
  work/Archive: 4
Top domains, last 24h
  newsletter.example.com: 12
  github.example.com: 7
```

---

## 4. Automate cleanup with a rule

Rules run IMAP searches and apply actions ([rules.md](rules.md)). Create a
rule, test it, then let the hourly `run-rules` job apply it.

```bash
curl -sS -X POST "$IMAP_MCP/api/rules" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"archive-newsletters",
       "conditions":{"folder":"INBOX","from":"newsletter.example.com","older_than_days":3},
       "actions":[{"type":"move","dest":"Archive"}]}'
# → 201 {"id": 12, "name": "archive-newsletters"}

# Dry run: counts matches and changes nothing.
curl -sS -X POST "$IMAP_MCP/api/rules/12/test" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" -d '{}'
# → {"id":12,"name":"archive-newsletters","matched":42,"action":"move"}
```

Apply all active rules once, the way the scheduled job does:

```bash
imap-mcp run-rules --config /path/to/config.yaml --dry-run   # preview
imap-mcp run-rules --config /path/to/config.yaml             # apply
```

Gmail matches search terms as whole tokens, so use the full domain in `from`
([rules.md](rules.md#gotcha-whole-token-matching)).

---

## 5. Get notified when a rule moves mail

Webhooks POST signed, metadata-only events ([webhooks.md](webhooks.md)). This
receiver verifies the signature and forwards `rule.fired` to a push service
(here an `ntfy`-style topic URL; any HTTP endpoint works). It uses only the
Python standard library.

```python
# receiver.py: python3 receiver.py  (listens on 127.0.0.1:9000)
import hashlib, hmac, json, os, time, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET = os.environ["WEBHOOK_SECRET"]          # returned once by POST /api/webhooks
PUSH_URL = os.environ["PUSH_URL"]              # e.g. https://ntfy.example.com/mail-rules

def verify(header, body, max_age=300):
    parts = dict(p.split("=", 1) for p in header.split(","))
    if abs(time.time() - int(parts["t"])) > max_age:
        return False
    mac = hmac.new(SECRET.encode(), f'{parts["t"]}.'.encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, parts["v1"])

class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        if not verify(self.headers.get("X-Imap-Mcp-Signature", ""), body):
            self.send_response(401); self.end_headers(); return
        ev = json.loads(body)
        if ev["event"] == "rule.fired":
            d = ev["data"]
            msg = f'rule {d["rule_id"]}: {d["action"]} x{d["matched"]} ({ev["account"]})'
            urllib.request.urlopen(urllib.request.Request(PUSH_URL, data=msg.encode()))
        self.send_response(204); self.end_headers()   # any 2xx = delivered

HTTPServer(("127.0.0.1", 9000), Hook).serve_forever()
```

Register it (loopback `http://` is allowed; anything else must be `https://`):

```bash
curl -sS -X POST "$IMAP_MCP/api/webhooks" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"http://127.0.0.1:9000/hook","events":["rule.fired"]}'
# → 201 {"id": 3, ..., "secret": "..."}   save the secret: it is shown only once

curl -sS -X POST "$IMAP_MCP/api/webhooks/3/test" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" -d '{}'
```

The payload carries rule id, action and count, never senders or subjects. To
show the rule's name, look it up with `GET /api/rules`. Failed deliveries are
retried with backoff; `GET /api/webhooks/3/deliveries` shows their status.

---

## 6. Find similar mail

Semantic search ranks enriched messages by embedding similarity
([rest-api.md](rest-api.md#post-apisearchsemantic-read)).

```bash
# By free text.
curl -sS -X POST "$IMAP_MCP/api/search/semantic" \
  -H "Authorization: Bearer $READ_TOKEN" -H "Content-Type: application/json" \
  -d '{"query":"invoice overdue payment reminder","limit":5}'

# "More like this one": use a cached message's vector.
curl -sS -X POST "$IMAP_MCP/api/search/semantic" \
  -H "Authorization: Bearer $READ_TOKEN" -H "Content-Type: application/json" \
  -d '{"reference_uid":4211,"folder":"INBOX","limit":5,"threshold":0.75}'
```

```json
{"hits":[{"account":"work","folder":"INBOX","uid":4198,"subject":"...",
          "from":"billing@example.com","date":"2026-10-02T08:00:00Z",
          "hall":"transactional","score":0.86}],
 "searched":790}
```

Only messages that enrichment has embedded are searched. `searched: 0` means
enrichment hasn't run yet ([enrichment.md](enrichment.md)).

---

## 7. Watch events live

`/api/events` is a Server-Sent Events stream of everything happening: syncs,
enrichment, rules, cleaning ([rest-api.md](rest-api.md#event-stream)).

```bash
curl -sS -N -H "Authorization: Bearer $READ_TOKEN" "$IMAP_MCP/api/events"
```

```text
: heartbeat

data: {"type":"sync.complete","account":"work","payload":{...}}

data: {"type":"rule.fired","account":"work","payload":{"rule_id":12,"action":"move","matched":3}}
```

datawatch's `imap_mcp` backend consumes this stream
([datawatch-integration.md](datawatch-integration.md)).

---

## 8. Keep the cache small

The cache holds a window of recent mail ([sync-cache.md](sync-cache.md)).
Give big, rarely read folders a shorter window, and keep flagged mail
regardless:

```yaml
sync:
  window_days: 30
  keep_flagged: true
  folder_window_days:
    Archive: 7          # key = the entry as written in `folders`
```

Or drop cached copies on demand (the mailbox is never touched). It's a dry run
unless you say otherwise:

```bash
curl -sS -X POST "$IMAP_MCP/api/cache/sweep" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"folder":"Archive","older_than_days":7}'          # dry run: counts only
curl -sS -X POST "$IMAP_MCP/api/cache/sweep" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"folder":"Archive","older_than_days":7,"dry_run":false}'
```

---

## 9. Encrypt an existing install

Keys live in datawatch secrets (or environment variables), never in the
config ([encryption.md](encryption.md)):

```bash
datawatch secrets set imap_mcp_state_key "$(openssl rand -base64 48)" --scope service:imap-mcp
datawatch secrets set imap_mcp_cache_key "$(openssl rand -base64 48)" --scope service:imap-mcp
```

```yaml
db:
  path: ~/.local/share/imap-mcp/imap.db
  encryption_key: ${secret:imap_mcp_state_key}
  cache:
    encryption_key: ${secret:imap_mcp_cache_key}
```

```bash
systemctl --user stop imap-mcp
imap-mcp db encrypt --config /path/to/config.yaml   # verifies, then replaces
systemctl --user start imap-mcp
curl -sS "$IMAP_MCP/api/health" | jq .storage
# → {"cache_encrypted": true, "state_encrypted": true}
```

`db encrypt` lists any plaintext backups it finds. Delete them yourself once
you're satisfied.

---

## 10. Agent workflows

Sections 1–9 show single features. This section shows what an agent can do
when it chains the tools together under the
[companion skill](../skills/imap-mcp/SKILL.md). Each workflow gives the prompt
you type, the tool calls the agent makes, and what you get back. They work in
Claude Code with imap-mcp attached, or in a datawatch session that loads the
`imap-mcp` skill from the community registry.

The same rules apply to every workflow:

- **Count before you change anything.** The agent searches or dry-runs first,
  shows you the number, and waits for a yes before it moves, trashes or sends.
- **Reading is safe.** Reads use `BODY.PEEK`, so the agent looking at a
  message never marks it read.
- **Reports go to `working_dir`.** `write_file` is the only way the agent saves
  to disk, and it can't write outside that directory. Keep mailbox-derived
  reports out of git.
- **Scope the token to the job.** A reporting agent needs only `read`. Its
  `tools/list` then hides every tool that changes mail, so it can't change
  mail even by mistake ([auth-tokens.md](auth-tokens.md)).

Outputs below are illustrative, with `example.com` senders.

### 10.1 Morning briefing

> "Give me a briefing on what came in since yesterday: what needs me, what's
> noise, and anything from someone I haven't heard from before. Save it as
> `briefings/today.md`."

1. `search_messages` with `since` and `flags: "Unseen"` lists the new mail.
2. `top_senders` with `group_by: "domain"` separates the bulk senders from the
   people.
3. `get_sender_profile` on each unfamiliar sender. A 404, or a `first_seen`
   within the last day, means a first-time sender. The profile covers all of
   your history, not just the cache window.
4. `get_message` only on the messages that look like they need a person, so the
   agent can say what each one asks for.
5. `write_file` saves the briefing.

```markdown
## Needs you (3)
- alice@example.com: contract redline, wants comments by Friday
- billing@example.net: card on file expires this month
- school@example.org: permission slip for the 14th

## First-time senders (1)
- partnerships@example.io: cold outreach, no prior history

## Noise (41): 6 domains, all newsletters or promos
```

Pair it with a token that has only the `read` scope and the briefing agent can
look at everything and change nothing.

### 10.2 Subscription audit with a keep/kill list

> "Find every mailing list I'm on, tell me which ones I actually read, and give
> me a table to decide what to keep."

1. `detect_subscriptions` finds senders with a `List-Unsubscribe` header and
   their unsubscribe links.
2. `get_sender_profile` on each candidate gives its all-history volume, its
   role, and whether you have ever written to it (`sent_count`). For "how many
   did I open", the agent counts with two `/api/query` calls on the
   `messages` view (`group_by: ["from_addr"]`, one with `seen` `eq true`;
   needs an `admin` token).
3. `write_file` saves `subscriptions.md`:

```markdown
| Sender                  | Last 90 days | Opened | Suggest     | Unsubscribe |
|-------------------------|-------------:|-------:|-------------|-------------|
| news@deals.example.com  |           88 |     0% | purge+rule  | https://... |
| digest@example.org      |           12 |    92% | keep        | https://... |
| promo@shop.example.net  |           30 |     3% | unsubscribe | mailto:...  |
```

4. You edit the "Suggest" column and say "do it". The agent re-reads the file
   with `read_file`, then for each row runs `purge_sender` (after a
   `search_messages` count) and `create_rule` so the sender doesn't come back.

imap-mcp never follows unsubscribe links on its own. The agent gives you the
link, and you decide whether to open it.

### 10.3 Teach by example

> "These two messages are receipts (UIDs 4411 and 4502 in INBOX). Find
> everything else like them, file them under Receipts, and keep doing that."

1. `semantic_search` with `folder: "INBOX"`, `reference_uid: 4411`,
   `threshold: 0.8`, then again for 4502. The matches are mail that *means* the same thing, even when the
   wording and senders differ.
2. The agent groups the matches by sender and shows you the groups with
   counts. You drop the false positives.
3. `label_bulk` (or `move_bulk` into `Receipts`) per sender.
4. Rules match text, not meaning, so the agent turns each confirmed sender
   into a `create_rule` with `action: "move"` and `dest: "Receipts"`, then
   runs `run_rules` with `dry_run: true` to show what they'd catch.

Semantic search finds the pattern once. Rules then apply it every hour at no
cost. This needs enrichment to have embedded the folder: check
`enrichment_status` first.

### 10.4 Rule review

> "Review my rules. Which ones haven't matched anything lately, which ones
> overlap, and would any of them trash something I flagged?"

1. `list_rules` gets every rule with its matchers and actions.
2. `run_rules` with `dry_run: true` gets each rule's current match count.
3. For the risky ones, `search_messages` with the rule's `from` and
   `flags: "Flagged"` checks whether they'd hit flagged mail.
4. The agent reports:
   - dead rules (zero matches);
   - overlapping rules (one sender caught by two rules);
   - rules whose dry run includes flagged mail;
   - and a proposed `delete_rule` list for you to approve.

A long-running rule set picks up dead and contradictory rules. This keeps the
hourly job lean ([rules.md](rules.md)).

### 10.5 "Did I drop anything?"

> "Find mail from real people in the last two weeks that I never answered."

The agent queries the cache through the REST API (`/api/query` needs the
`admin` scope). Classification adds a `hall` tag to each message, and
`answered` tracks the `\Answered` flag:

```bash
curl -sS -X POST "$IMAP_MCP/api/query" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"view":"messages",
       "where":[{"field":"date","op":"gte","value":"-14d"},
                {"field":"hall","op":"in","value":["personal","conversation"]},
                {"field":"answered","op":"eq","value":false}],
       "fields":["uid","folder","date","from_addr","subject"],
       "order_by":[{"field":"date","desc":true}]}'
```

It then reads the top few with `get_message` and drafts replies (10.7). Mail
classified before tagging worked has an empty `hall`. If the list looks thin,
drop that filter and let the agent judge from the sender.

### 10.6 Is this one phishing?

> "The 'password reset' mail in INBOX, UID 9120: is it real?"

1. `get_headers` returns `Authentication-Results` (SPF, DKIM, DMARC),
   `Return-Path`, `Reply-To` and the `Received` chain.
2. `get_anomalies` with `sender` set to the address may already have flagged
   it (`auth_failure`, `lookalike_domain`, `reply_to_mismatch`).
   `get_sender_profile` shows whether this address has written before, over
   all history, and how its mail usually authenticates. A sender whose mail
   has always passed DMARC (`dmarc_pass` high, `dmarc_fail` 0) but whose
   message now fails it is a strong warning.
3. `semantic_search` with `folder: "INBOX"`, `reference_uid: 9120` compares it with the real
   resets from that service, if any are in the cache.
4. `get_attachments` without `part` lists any attachments by name and type,
   without opening them. An `.html`, `.htm` or `.iso` on a "password reset" is
   a red flag on its own.
5. The agent gives a verdict with reasons, for example:
   - DMARC failed;
   - `Reply-To` goes to a different domain;
   - first message from this address in the cache;
   - the link host doesn't match the brand;
   - an unexpected HTML attachment.

   On a "yes, it's bad", it moves the message to Junk with `move_message`.

The agent reads the link text but never follows links. That's the point of
asking it.

### 10.7 Draft replies you send yourself

> "Draft replies to the three unanswered messages from 10.5. Don't send them."

For each one the agent first reads the conversation with `get_thread` (pass
the message's `thread_id`), so the draft answers the latest point, not just
the first message. It writes each reply as a raw RFC 2822 message, with
`In-Reply-To` and `References` set from the thread, and uses
`append_message` with `folder: "Drafts"` (`[Gmail]/Drafts` on Gmail) and
`flags: "Draft"`. The drafts then show up in your normal mail client, ready to
edit and send. No `send` scope is needed and nothing leaves the server.

If you do want the agent to send, give it a token with the `send` scope and an
account with an `smtp:` block. It then calls `send_message`, and the skill
tells it to show you the final text and wait for your go-ahead.

### 10.8 Build a dossier

> "Pull together everything about my October trip: flights, hotel,
> car, confirmation numbers, into one page."

1. `semantic_search` with queries like "flight confirmation", "hotel
   reservation" and "rental car", in all accounts (omit `account`).
2. `cross_account_search` with `text: "confirmation"` and a `since` date
   catches the keyword matches that meaning-based search ranks lower.
3. `get_message` on each hit pulls out dates, times and confirmation numbers.
4. `get_attachments` lists each hit's attachments. The agent saves the
   boarding passes and booking PDFs with `part` (they land in
   `attachments/<account>/…`). A small `.ics` invite comes back inline, so the
   agent can read the times straight from it.
5. `write_file` saves `trips/october.md`: one dated itinerary that links each
   entry to its source message (folder and UID) and its saved attachment.

The same pattern works for:
- "every invoice from example.net this year, with totals";
- "all the mail about the kitchen remodel, as a timeline";
- "what did legal@example.com and I agree on, and when?".

### 10.9 A scheduled agent, not just a scheduled job

`imap-mcp run-rules` on a schedule handles the deterministic part
([cookbook phase 5](cookbook-inbox-cleanup.md)). For the judgment calls, have
datawatch schedule an **agent session** that loads the `imap-mcp` skill, with
a prompt such as:

> "Run the morning briefing (10.1) and save it. Then look at senders that
> landed in INBOX 5+ times this week and weren't opened, and draft rules for
> them, but don't create them. Write the proposals to `proposals/rules.md`."

You review `proposals/rules.md` when convenient and say "create the ones I
kept". The agent proposes, you decide, the hourly rules job applies.

Give the scheduled session a token with only the scopes it needs. A briefing
needs `read`. Proposing rules needs `read` as well, because it only writes
files.

### 10.10 Email as a remote control

With the inbound command channel enabled
([datawatch-integration.md § 3b](datawatch-integration.md)), imap-mcp watches a
folder for signed command envelopes. Every gate you configure has to pass:
- sender allowlist;
- DKIM and DMARC;
- HMAC;
- replay window.

Verified commands go to datawatch's `imap_mcp` backend, which decides what each
verb may do and replies through imap-mcp.

For example, you send a signed `status` command from your phone's mail app. A
minute later the reply lands with datawatch's status. A `mail.archive` command
archives a sender's mail without you opening a laptop. Ordinary mail is never
touched. Anything that fails a gate is logged as `inbound.rejected` and does
nothing.

### 10.11 Close the loop with events

Rules and agents can feed each other:

- A rule fires, and the `rule.fired` webhook (section 5) posts to a receiver.
  That receiver can push a notification, or queue a follow-up task for an
  agent: "a new sender hit the catch-all rule; decide whether it deserves its
  own".
- A dashboard or script listens on `/api/events` (section 7) for
  `enrichment.done`, so new mail shows up in semantic search as soon as it's
  embedded.

### 10.12 Hand over a conversation

> "Legal needs the whole thread with example.net about the renewal, with
> attachments, as files I can forward."

1. `cross_account_search` with `from: "example.net"` and `subject: "renewal"`
   finds the conversation in whichever account it lives in. Each hit carries
   its `thread_id`.
2. `get_thread` returns every message, Sent replies included. If the thread
   started before the sync window, the tool searches the server for the older
   part on its own (`live_search: true` in the result).
3. The agent shows you the count and date range, and asks before exporting.
4. `export_message` with `thread_id` writes one `.mbox` that any mail client
   can import. `get_attachments` with `part` saves each attachment next to it.
5. `write_file` adds a short `README.md` that lists the messages and files.

Over REST, a script can do the same in one call:

```bash
curl -sS -X POST "$IMAP_MCP/api/export" -H "Authorization: Bearer $WRITE_TOKEN" \
  -H "Content-Type: application/json" -d '{"thread_id":"<root-id@example.net>"}' -o renewal.mbox
```

Exports are capped (`tools.export_max_messages`, `tools.export_max_mb`). A
selection over a cap is refused with a clear message, never cut short.

### 10.13 Who's who on a project

> "Who's involved in the Apollo work, who runs it, and is anything due?"

1. `kg_query` with `entity: "apollo"` and `predicate: "works_on"` lists the
   people whose mail is tagged with the project, strongest first.
2. For the top few, `kg_query` with each address (no predicate) shows their
   organization (`belongs_to`), who they're usually copied with (`cc_with`),
   and any `manages` / `reports_to` the model read in recent mail.
3. `kg_query` with `predicate: "deadline"` finds threads with stated due
   dates. `properties.due` holds the date, and the subject is the thread ID,
   so `get_thread` opens the conversation.
4. The agent writes `projects/apollo.md`: people and roles, who reports to
   whom, open deadlines with links back to their threads.

Edges the model extracted have `confidence: 0.6`. The agent says which facts
came from headers and tags (certain) and which from reading mail (inferred).
`current: false` marks relationships with no evidence in the last year.

### 10.14 Who did I lose touch with?

> "Who did I used to write to a lot but haven't heard from in a while?"

`kg_query` with `predicate: "corresponds_with"`. The agent keeps the heavy
relationships (`weight` high) whose `last_seen` is old, or that are no longer
`current`. Then `get_sender_profile` on each gives the reply history. The
answer is a short list with "last contact" dates, not a mailbox dump.

### 10.15 Morning security check

> "Anything suspicious in my mail since yesterday?"

1. `get_anomalies` with `severity: "high"`: `auth_failure` (a sender whose
   mail used to pass DMARC now fails) and `lookalike_domain` (a domain one
   character away from one you write to). Each has `folder`, `uid` and
   `message_ref`.
2. For each one, the agent runs the phishing check (10.6) on that message:
   headers, sender profile and attachment list.
3. `get_anomalies` with `severity: "medium"`: `reply_to_mismatch` and
   `volume_spike`, summarised in one line each.
4. The agent reports what it found and asks before acting: move to Junk,
   or `resolve_anomaly` with a note on why it's fine (a known sender whose
   mailing service fails DKIM, say).

Run it from a scheduled session (10.9) with a `read` token: it can look at
everything and report, and it can't resolve or move anything. To get the
alert pushed instead, subscribe a webhook to `anomaly.detected` (section 5).
The payload is just `{id, type, severity}`, so the receiver fetches the
details with its own token.


---

## 11. Threads, attachments and export from a script

The same features the agent uses in 10.12, over REST
([rest-api.md](rest-api.md#get-apithreadsthread_id-read)). Reads need `read`.
Downloads need `write`, return the raw bytes as `application/octet-stream`,
and write nothing on the server.

**Search every account at once**, then follow a hit into its conversation.
Without `live=true` the search covers the cache (the sync window); with it,
each account's INBOX over full history:

```bash
curl -sS -G "$IMAP_MCP/api/search/cross" -H "Authorization: Bearer $READ_TOKEN" \
  --data-urlencode "from=example.net" --data-urlencode "subject=renewal" | jq '.hits[] | {account, folder, uid, date, thread_id}'

TID=$(curl -sS -G "$IMAP_MCP/api/search/cross" -H "Authorization: Bearer $READ_TOKEN" \
  --data-urlencode "from=example.net" --data-urlencode "subject=renewal" | jq -r '.hits[0].thread_id')
curl -sS "$IMAP_MCP/api/threads/$(jq -rn --arg t "$TID" '$t|@uri')" -H "Authorization: Bearer $READ_TOKEN" \
  | jq '{count, live_search, messages: [.messages[] | {date, from, folder, uid, source}]}'
```

`live_search: true` means part of the thread was older than the cache, so the
server was searched for it.

**List a message's attachments, then download one:**

```bash
MSG="$IMAP_MCP/api/accounts/work/folders/INBOX/messages/4211"
curl -sS "$MSG/attachments" -H "Authorization: Bearer $READ_TOKEN" | jq '.attachments[] | {part, filename, mime, size_bytes}'
curl -sS -OJ "$MSG/attachments/2" -H "Authorization: Bearer $WRITE_TOKEN"   # saves under the sanitised filename
```

Folder names with `/` (such as `[Gmail]/All Mail`) go in the path
URL-encoded: `%5BGmail%5D%2FAll%20Mail`.

**Export one message, or a whole thread, as files any mail client opens:**

```bash
curl -sS -o 4211.eml "$MSG/export.eml" -H "Authorization: Bearer $WRITE_TOKEN"
curl -sS -D - -o thread.mbox -X POST "$IMAP_MCP/api/export" \
  -H "Authorization: Bearer $WRITE_TOKEN" -H "Content-Type: application/json" \
  -d "$(jq -n --arg t "$TID" '{thread_id: $t}')" | grep -i x-export-count
```

A thread export searches the server, so it still works when a rule has just
moved part of the thread. Messages that are gone are skipped and counted in
`X-Export-Missing`. `POST /api/export` also takes
`{"folder": "INBOX", "uids": [4211, 4212]}` or
`{"from": "billing@example.com"}`. A selection over the `tools.export_max_*`
caps gets 422 before anything is downloaded.

---

## 12. Profiles and the knowledge graph from a script

Sender profiles and the graph cover all of your history, not just the cache
([intelligence.md](intelligence.md)).

**Is the first history scan done?** Profiles and the graph are partial until
it is, and anomaly detection waits for it:

```bash
curl -sS "$IMAP_MCP/api/health" | jq '.intelligence | {backfill_complete, folders_complete, folders, messages_indexed, senders, kg_relationships}'
```

**One sender's profile**, including their graph edges and open anomalies:

```bash
curl -sS "$IMAP_MCP/api/senders/$(jq -rn '"ann@example.com"|@uri')" -H "Authorization: Bearer $READ_TOKEN" \
  | jq '{role, role_source, message_count, sent_count, avg_reply_seconds, dmarc_pass, dmarc_fail,
         scan_complete, top_links: [.relationships[:5][] | "\(.predicate) \(.object) x\(.weight)"]}'
```

**The graph around a person, a domain or a project**, strongest first:

```bash
curl -sS -G "$IMAP_MCP/api/kg" -H "Authorization: Bearer $READ_TOKEN" --data-urlencode "entity=example.com" \
  | jq '.relationships[] | "\(.subject) \(.predicate) \(.object) (x\(.weight), current: \(.current))"'
curl -sS -G "$IMAP_MCP/api/kg" -H "Authorization: Bearer $READ_TOKEN" --data-urlencode "predicate=deadline" \
  | jq '.relationships[] | {thread: .subject, what: .object, due: .properties.due, confidence}'
```

`confidence` is 1 for edges read from headers and tags, 0.6 for edges the
model read from a message body.

**Questions across all senders** use `/api/query` (admin), as in section 2:

```bash
q() { curl -sS -X POST "$IMAP_MCP/api/query" -H "Authorization: Bearer $ADMIN_TOKEN" \
        -H "Content-Type: application/json" -d "$1"; }

# How your senders break down by role, and how each role was decided.
q '{"view":"senders","group_by":["role","role_source"],"aggregate":[{"fn":"count","as":"n"}],
    "order_by":[{"field":"n","desc":true}]}'

# People who write a lot and whom you answer slowest.
q '{"view":"senders","fields":["address","message_count","reply_count","avg_reply_time"],
    "where":[{"field":"reply_count","op":"gte","value":3}],
    "order_by":[{"field":"avg_reply_time","desc":true}],"limit":10}'

# Your strongest correspondents, and whether the relationship is current.
q '{"view":"kg","fields":["subject","weight","last_seen","current"],
    "where":[{"field":"predicate","op":"eq","value":"corresponds_with"}],
    "order_by":[{"field":"weight","desc":true}],"limit":20}'
```

---

## 13. Anomaly alerts

Anomaly detection runs once the history scan is complete and checks new mail
([intelligence.md](intelligence.md#anomalies)).

**What's open, worst first:**

```bash
curl -sS -G "$IMAP_MCP/api/anomalies" -H "Authorization: Bearer $READ_TOKEN" --data-urlencode "severity=high" \
  | jq '.anomalies[] | {id, type, sender, description, folder, uid, message_ref}'

# Counts by type and severity, including resolved ones (q is defined in section 12).
q '{"view":"anomalies","group_by":["type","severity","resolved"],"aggregate":[{"fn":"count","as":"n"}]}'
```

**Mark one reviewed** (it stays in the log with `resolved: true`):

```bash
curl -sS -X POST "$IMAP_MCP/api/anomalies/17/resolve" \
  -H "Authorization: Bearer $WRITE_TOKEN" -H "Content-Type: application/json" -d '{}'
```

**Push high-severity findings to your phone.** `anomaly.detected` webhooks
carry `{id, type, severity}` only, never the sender. This receiver extends the
one in section 5: it verifies the signature, fetches the finding with its own
read token, and pushes `high` ones.

```python
# anomaly_receiver.py: python3 anomaly_receiver.py  (listens on 127.0.0.1:9001)
import hashlib, hmac, json, os, time, urllib.parse, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET = os.environ["WEBHOOK_SECRET"]
PUSH_URL = os.environ["PUSH_URL"]
API, TOKEN = os.environ["IMAP_MCP"], os.environ["READ_TOKEN"]

def verify(header, body, max_age=300):
    parts = dict(p.split("=", 1) for p in header.split(","))
    if abs(time.time() - int(parts["t"])) > max_age:
        return False
    mac = hmac.new(SECRET.encode(), f'{parts["t"]}.'.encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, parts["v1"])

def finding(account, typ, fid):
    q = urllib.parse.urlencode({"account": account, "type": typ, "limit": 50})
    req = urllib.request.Request(f"{API}/api/anomalies?{q}", headers={"Authorization": f"Bearer {TOKEN}"})
    for a in json.load(urllib.request.urlopen(req))["anomalies"]:
        if a["id"] == fid:
            return a

class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        if not verify(self.headers.get("X-Imap-Mcp-Signature", ""), body):
            self.send_response(401); self.end_headers(); return
        ev = json.loads(body)
        d = ev["data"]
        if ev["event"] == "anomaly.detected" and d["severity"] == "high":
            a = finding(ev["account"], d["type"], d["id"])
            if a:
                msg = f'{a["type"]}: {a["description"]} ({a.get("folder", "")} uid {a.get("uid", "")})'
                urllib.request.urlopen(urllib.request.Request(PUSH_URL, data=msg.encode()))
        self.send_response(204); self.end_headers()

HTTPServer(("127.0.0.1", 9001), Hook).serve_forever()
```

```bash
curl -sS -X POST "$IMAP_MCP/api/webhooks" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"http://127.0.0.1:9001/hook","events":["anomaly.detected"]}'
```

The push message names the finding and where the message is, so you can open
it in your mail client. The sender's address travels only from imap-mcp to
the receiver on your machine, not in the webhook itself.

---

## 14. Hold first-time senders that look like spam

Some spam uses a new domain for almost every message, so no `from` rule keeps
up. A `new_sender` rule holds mail from senders with no history, but only
when its headers also look like bulk mail or a scam: a borrowed brand name,
not addressed to you, bulk headers, a throwaway domain. Real first contacts,
replies to your own mail and introductions from people you know stay in the
inbox ([rules.md](rules.md#the-new-sender-hold)).

```bash
curl -sS -X POST "$IMAP_MCP/api/rules" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"hold-new-senders","active":false,
       "conditions":{"account":"work","new_sender":true},
       "actions":[{"type":"move","dest":"Held"}]}'
# → 201 {"id": 42, "name": "hold-new-senders"}

# Preview who would be held and why, before turning it on.
curl -sS -X POST "$IMAP_MCP/api/rules/42/test" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" -d '{}'
# → {"id":42,"matched":2,"action":"move","preview":[
#     {"sender":"info@shop5x.example","reasons":"display name borrows a brand or agency; throwaway-looking domain"},
#     {"sender":"deals@promo.example","reasons":"bulk mail you never signed up for; classified as newsletter"}]}
```

Turn it on by recreating it with `"active": true` (or `PUT /api/rules/42`).
After that you never need to open the holding folder: once a day a
"Held for review" summary appears in the inbox, and a `hold.digest` webhook
carries the count for a dashboard. Move anything you want back to the inbox
and that sender is never held again.

## 15. What am I behind on?

`needs_reply` lists conversations where someone wrote to you and you have
not replied; `awaiting_reply` lists the ones where you wrote last. Both look
at your whole history, wherever the mail is filed
([intelligence.md](intelligence.md#reply-tracking)).

```bash
# Waiting on you for 3+ days, in the last two weeks.
curl -sS "$IMAP_MCP/api/replies/needed?account=work&older_than_days=3&within_days=14" \
  -H "Authorization: Bearer $READ_TOKEN"
# → {"count":1,"history_complete":true,"items":[
#     {"account":"work","thread_id":"contract-1@example.com","counterpart":"jane@example.com","name":"Jane Roe",
#      "subject":"Contract review","folder":"Clients","uid":812,
#      "message_ref":"abc@example.com","last_date":"2026-10-06T14:02:11Z","days_waiting":4}]}

# Not going to answer that one: drop it until Jane writes again.
curl -sS -X POST "$IMAP_MCP/api/replies/dismiss" \
  -H "Authorization: Bearer $WRITE_TOKEN" -H "Content-Type: application/json" \
  -d '{"account":"work","thread_id":"contract-1@example.com"}'
```

From an agent: "What do I owe people a reply on?" → `needs_reply`, then
`get_thread` on an item's `thread_id` to draft an answer. The daily digest
in your inbox carries the same list under "Waiting on you".

## 16. Rules from what you already do

If you keep moving a sender's mail to Trash or Junk yourself, imap-mcp
notices and suggests a rule ([rules.md](rules.md#learning-from-your-moves)).

```bash
curl -sS "$IMAP_MCP/api/rules/suggestions?account=work" -H "Authorization: Bearer $READ_TOKEN"
# → {"mode":"suggest","count":1,"history_complete":true,"suggestions":[
#     {"account":"work","target":"@deals.example","kind":"domain",
#      "addresses":["offers@deals.example","promo@deals.example"],
#      "received":12,"discarded":11,"ratio":0.91,"action":"trash","matches":2,"status":"suggested",
#      "rule":{"name":"learned: @deals.example","active":false,
#              "conditions":{"account":"work","from":"@deals.example"},"actions":[{"type":"trash"}]}}]}

# Accept: create the suggested rule (inactive), test it, then turn it on.
curl -sS -X POST "$IMAP_MCP/api/rules" -H "Authorization: Bearer $WRITE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"learned: @deals.example","active":false,
       "conditions":{"account":"work","from":"@deals.example"},"actions":[{"type":"trash"}]}'

# Or say no for good.
curl -sS -X POST "$IMAP_MCP/api/rules/suggestions/dismiss" -H "Authorization: Bearer $WRITE_TOKEN" \
  -H "Content-Type: application/json" -d '{"account":"work","target":"@deals.example"}'
```

The daily digest lists open suggestions under "Suggested rules". A webhook
on `rule.suggested` gets each new one with full details; register it with
`"payload": "suggested,created"` if you only want the counts
([webhooks.md](webhooks.md#payload)).
