# Examples: what you can do with imap-mcp

Short, runnable recipes for the features added in 0.5–0.10: scoped tokens, the
sync cache, `/api/query`, rules, webhooks, semantic search and the event stream.
Each one links to the reference page with the details. [Section 10](#10-agent-workflows)
puts them together: multi-step workflows an agent runs for you.

All addresses are `example.com` placeholders and every output is illustrative.
The recipes assume `serve` is running on `127.0.0.1:8765` and that these
environment variables hold tokens with the scopes named
([auth-tokens.md](auth-tokens.md)):

```bash
export IMAP_MCP=http://127.0.0.1:8765
export READ_TOKEN=...    # scopes: read
export ADMIN_TOKEN=...   # scopes: read, write, admin
```

Every POST/PUT/DELETE needs `Content-Type: application/json` and a JSON body,
even if it is only `{}` ([rest-api.md](rest-api.md#request-guard)).

---

## 1. Ask your agent

With imap-mcp attached to Claude Code (or a datawatch session) you can just
ask. These map to the MCP tools listed in the [README](../README.md#mcp-tools-44):

| You say | Tools the agent uses |
|---------|----------------------|
| "Who sends me the most mail in INBOX? Group by domain." | `top_senders` |
| "Move everything from `deals.example.com` to Trash." | `purge_sender` (confirm the count first with `search_messages`) |
| "Label all mail from `billing@example.com` as Finance." | `label_bulk` |
| "Create a rule that archives `newsletter.example.com` every hour, and show me what it would match first." | `create_rule`, then `run_rules` with `dry_run: true` |
| "Find mail similar to the invoice from yesterday." | `semantic_search` with `reference_uid` |
| "Is enrichment caught up?" | `enrichment_status` |
| "Forget the cached copies of Archive older than 90 days." | `cache_sweep` (dry run first) |

The [inbox-cleanup cookbook](cookbook-inbox-cleanup.md) walks through a full
cleanup session end to end, and [section 10](#10-agent-workflows) has eleven
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
3. `get_sender_history` on each unfamiliar sender. It reads the cache, so an
   empty history means nothing from them within the sync window, which is a
   good first-time-sender signal.
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
2. For each candidate, the agent counts its messages and how many you opened
   with two `/api/query` calls (`group_by: ["from_addr"]`, one with `seen`
   `eq true`; this needs an `admin` token). Without one, it falls back to
   `get_sender_history`, which lists each sender's cached mail.
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
2. `get_sender_history` shows whether this address has written before (within
   the cache window).
3. `semantic_search` with `folder: "INBOX"`, `reference_uid: 9120` compares it with the real
   resets from that service, if any are in the cache.
4. The agent gives a verdict with reasons, for example:
   - DMARC failed;
   - `Reply-To` goes to a different domain;
   - first message from this address in the cache;
   - the link host doesn't match the brand.

   On a "yes, it's bad", it moves the message to Junk with `move_message`.

The agent reads the link text but never follows links. That's the point of
asking it.

### 10.7 Draft replies you send yourself

> "Draft replies to the three unanswered messages from 10.5. Don't send them."

The agent writes each reply as a raw RFC 2822 message and uses
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
2. `get_message` on each hit pulls out dates, times and confirmation numbers.
3. `write_file` saves `trips/october.md`: one dated itinerary with a link
   back (folder and UID) to each source message.

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

### Not there yet

Some tools exist but have nothing to return yet:
- `get_sender_profile`, `kg_query` and `get_anomalies` work, but nothing fills
  sender profiles, the knowledge graph or anomalies yet.
- `get_thread`, `get_attachments`, `export_message` and
  `cross_account_search` are stubs.

An agent that calls one of these gets an empty result or "not yet
implemented". The workflows above avoid them: for example, 10.6 reconstructs
sender history with `get_sender_history` instead of `get_sender_profile`. See
[known-limitations.md](known-limitations.md).
