# Sender intelligence

imap-mcp builds a profile of every sender you correspond with. It reads your
mail's history once, keeps the profiles up to date as new mail arrives, and
gives each sender a role. `get_sender_profile`, `GET /api/senders` and the
`/api/query` `senders` view read the result.

The knowledge graph and anomaly detection build on these profiles. They are
planned work and still return empty results
([known-limitations.md](known-limitations.md)).

## What a profile holds

| Field | Meaning |
|-------|---------|
| `role` | `colleague`, `vendor`, `newsletter`, `bot`, `personal` or `unknown` |
| `role_source` | How the role was decided: `signal:list`, `signal:auto-submitted`, `signal:noreply`, `signal:sent-to`, `hall` or `llm` (see below) |
| `first_seen`, `last_seen` | First and last contact in either direction (Unix time) |
| `message_count` | Messages received from them |
| `sent_count` | Messages you sent to them (To or Cc) |
| `reply_count`, `avg_reply_seconds` | Your replies to their mail, and how long you took on average |
| `list_count`, `bulk_count`, `auto_count` | Their messages with list headers, `Precedence: bulk/list/junk`, or `Auto-Submitted` |
| `dkim_pass`, `dkim_fail`, `dmarc_pass`, `dmarc_fail` | DKIM and DMARC results recorded by your mail server for their messages. Any result other than pass, including `none`, counts as a fail. |
| `scan_complete` | `false` while the first scan of your history is still running: counts and roles are partial until then |

```json
{"address": "billing@example.com", "name": "Example Billing", "domain": "example.com",
 "role": "vendor", "role_source": "hall", "first_seen": 1640995200, "last_seen": 1791500000,
 "message_count": 58, "sent_count": 2, "reply_count": 2, "avg_reply_seconds": 5400,
 "list_count": 0, "bulk_count": 0, "auto_count": 0, "dkim_pass": 58, "dkim_fail": 0,
 "dmarc_pass": 58, "dmarc_fail": 0, "scan_complete": true,
 "cached_messages": 3, "relationships": [], "anomalies": []}
```

## How it is built

**A header-only scan.** A background scanner reads every folder:
- **Message fields:** the sender, recipients, date, Message-ID and In-Reply-To;
- **Header fields:** `List-Id`, `List-Unsubscribe`, `Precedence`,
  `Auto-Submitted` and `Authentication-Results`.

It never reads subjects or bodies. It opens folders read-only and fetches
with `PEEK`, so it never marks anything as read.

**Which folders.** On a server with an All Mail folder (Gmail), only that
folder is scanned, because it holds every message once. Elsewhere the scan
covers every folder except Junk, Drafts and any you list in
`intelligence.exclude_folders`. Trash is included: mail you deleted is still
correspondence history.

**First scan, then new mail only.** The first pass walks all your history
at `intelligence.backfill_per_minute` messages per minute. Profiles fill in
as it goes: roles and reply times are refreshed every 5,000 messages. After
that, each tick (every `scan_interval_minutes`) reads only messages newer
than the last one seen in each folder. The scan resumes where it stopped
after a restart. If a server renumbers a folder (a UIDVALIDITY change), that
folder is scanned again.

**Each message counts once.** A message filed in two folders, or carrying
two Gmail labels, is still one message. `imap.db` keeps a small index with
one row per message: a hash of its Message-ID, its date, the sender's id,
whether it is incoming or outgoing, and a hash of the message it replies to.
The rows hold no addresses, subjects or bodies (AGENT.md D28). The same index
pairs your replies with the mail they answer to measure reply times. A reply
sent more than 30 days later counts as a new conversation, not a reply time.

**Which result is trusted.** Only the first `Authentication-Results` header
is used. Your server adds that one when the message arrives; headers further
down could have been written by the sender.

## Roles

A sender's role comes from three sources, in order (AGENT.md D20):

1. **Header signals.**
   - Half or more of their mail has list headers or `Precedence: bulk`:
     **newsletter**.
   - Half or more is `Auto-Submitted`, or the address looks automated
     (`noreply`, `no-reply`, `notifications`, `mailer-daemon` and similar):
     **bot**.
   - You have written to them: **colleague** when they share your account's
     own domain (never for webmail domains like gmail.com), otherwise
     **personal**.
2. **The cache's classification tags.** Enrichment tags each cached message
   (`hall`). The sender gets the role that matches the most common tag on
   their mail: newsletter → newsletter, transactional → vendor, notification
   or alert → bot, conversation or personal → personal.
3. **The classify model**, for senders still `unknown` that have mail in
   the cache. It sees only the address, the display name and up to five
   recent subjects, never a body, and is asked for one of the five roles. The
   model runs only when enrichment is enabled and the enrichment backfill
   gates allow it (quiet hours, GPU yield, datawatch capacity). It handles
   `llm_roles_per_tick` senders per tick. A sender it can't place is not
   asked again for a week.

New mail from a sender re-runs steps 1 and 2. A role from the model is kept
until a signal or tag decides otherwise.

## Configuration

```yaml
intelligence:
  enabled: true
  scan_interval_minutes: 15
  backfill_per_minute: 600
  batch_size: 200
  exclude_folders: []
  llm_roles: true
  llm_roles_per_tick: 20
```

| Setting | Default | Environment variable |
|---------|---------|----------------------|
| `enabled` | true | `IMAP_MCP_INTELLIGENCE_ENABLED` |
| `scan_interval_minutes` | 15 | `IMAP_MCP_INTELLIGENCE_SCAN_INTERVAL_MINUTES` |
| `backfill_per_minute` | 600 | `IMAP_MCP_INTELLIGENCE_BACKFILL_PER_MINUTE` |
| `batch_size` | 200 (1–1000) | `IMAP_MCP_INTELLIGENCE_BATCH_SIZE` |
| `exclude_folders` | none | |
| `llm_roles` | true | `IMAP_MCP_INTELLIGENCE_LLM_ROLES` |
| `llm_roles_per_tick` | 20 | `IMAP_MCP_INTELLIGENCE_LLM_ROLES_PER_TICK` |

At 600 messages a minute, a mailbox of 100,000 messages takes about three
hours for its first scan. The scan shares each account's IMAP connection
with the tools and the sync, one batch at a time, so it doesn't block them
for long.

## Watching progress

`GET /api/health` has an `intelligence` block (counts only):

```json
"intelligence": {"enabled": true, "folders": 14, "folders_complete": 14, "backfill_complete": true,
                 "messages_indexed": 48210, "senders": 3120, "replies_paired": 912,
                 "roles": {"newsletter": 1210, "bot": 640, "vendor": 410, "personal": 380, "colleague": 95, "unknown": 385},
                 "last_scan": "2026-10-09T22:49:45Z"}
```

The logs record one `intel: scan tick` line per tick that found something.

## Storage and privacy

Profiles, the index and scan progress live in `imap.db`, the durable state
file. That file is encrypted when `db.encryption_key` is set
([encryption.md](encryption.md)). They are not in the disposable cache, so
they survive the cache window and cache rebuilds. Upgrading from 0.11 moves
the four empty intelligence tables out of `cache.db` without rebuilding the
cache. Back up `imap.db` before upgrading, as for any release that changes
it.

What is stored per sender: their address, display name, domain, and the
counts and dates above. Message content is never stored, and subjects are
never stored here; they appear only in the transient model prompt in step 3.
