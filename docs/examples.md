# Examples: what you can do with imap-mcp

Short, runnable recipes for the features added in 0.5–0.10: scoped tokens, the
sync cache, `/api/query`, rules, webhooks, semantic search and the event stream.
Each one links to the reference page with the details.

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
cleanup session end to end.

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
