# Sender intelligence

imap-mcp builds a profile of every sender you correspond with, and a
knowledge graph of who you correspond with, who appears together, which
organizations, threads, projects and topics they connect to, and what your
recent mail says about reporting lines and deadlines. It also flags anomalies:
new senders, spoofing signs, look-alike domains, silences and bursts. It reads
your mail's history once and keeps everything up to date as new mail
arrives.

- Profiles: `get_sender_profile`, `GET /api/senders`, the `/api/query`
  `senders` view.
- Graph: `kg_query`, `GET /api/kg`, the `/api/query` `kg` view, and the
  `relationships` list in each profile.
- Anomalies: `get_anomalies`, `resolve_anomaly`, `GET /api/anomalies`, the
  `anomaly.detected` event (SSE and webhooks), and each profile's `anomalies`
  list.
- Reply tracking: `needs_reply`, `awaiting_reply`, `dismiss_reply`,
  `GET /api/replies/needed`, `GET /api/replies/awaiting`, and the daily
  digest's "Waiting on you" section.

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

## Knowledge graph

### Entities

| Type | What it is | Name |
|------|------------|------|
| `person` | A correspondent, or you: each account's own address stands for the mailbox owner | The address. Names from model extraction (below) are kept as written |
| `organization` | A correspondent's domain (never a shared webmail domain such as gmail.com) | The domain, or a name from model extraction |
| `thread` | A conversation | Its root Message-ID: pass it to `get_thread` |
| `project`, `topic` | The `wing` and `room` tags enrichment gives cached mail | Lower-cased tag |

### Relationships

| Predicate | From → to | Built from |
|-----------|-----------|------------|
| `belongs_to` | person → organization | Header scan: the address's domain |
| `corresponds_with` | person → you | Header scan: person-to-person mail in either direction |
| `cc_with` | person → person | Header scan: people on the same message, you excluded, stored once per pair; only messages with at most 8 people, so mailing lists don't link everyone to everyone |
| `is_subscription` | sender → you | Header scan: list, bulk or automated mail |
| `participates_in` | person → thread | Header scan: person-to-person mail (thread root from References) |
| `works_on` | person → project | Cache: the `wing` tag of their mail. Model extraction can add more |
| `discusses` | person → topic | Cache: the `room` tag of their mail |
| `manages`, `reports_to` | person → person | Model extraction |
| `works_at` | person → organization | Model extraction |
| `deadline` | thread → topic | Model extraction; `properties` holds `{"due": "YYYY-MM-DD"}` when a date was stated |

Each relationship carries:
- `weight`: the number of messages behind it;
- `valid_from`: the first evidence;
- `last_seen`: the latest evidence;
- `confidence`: 1.0 for header and tag evidence, 0.6 when only the model saw it.

A relationship with no new evidence for `intelligence.kg_stale_days`
(default 365) gets `valid_to` set and `current: false`. It is history, not
deleted. New evidence makes it current again. `kg_query` lists the strongest
relationships first.

```json
{"subject": "ann@example.com", "subject_type": "person", "predicate": "corresponds_with",
 "object": "you@example.com", "object_type": "person", "valid_from": 1640995200,
 "confidence": 1, "weight": 214, "last_seen": 1791500000, "current": true}
```

### Built once per message

Header edges are added during the header scan, in the same transaction as
the profile counts. The per-message index row records that a message's edges
exist (`kg_done`), so copies in other folders or labels, and rescans, never
add them twice. Tag edges and model edges are also recorded once per message,
the same way.

**Upgrading from 0.12** adds these flags to an index that already has rows.
On the first start the header scan begins again from the start of every
folder, so the graph gets built for all of history. Profile counts don't
change during this rescan: the index already knows every message. It takes
as long as the first scan did.

### Model extraction

For cached mail classified as conversation or personal, the classify model
reads:
- the sender;
- the date;
- the subject;
- up to 1,500 characters of the body, with quoted replies and signatures
  removed.

It is asked for `manages`, `reports_to`, `works_at`, `works_on` and
`deadline` relations as JSON. The answer is treated as untrusted data:
- any other predicate is dropped;
- names are cleaned and capped at 80 characters;
- at most 10 relations are kept per message;
- dates must be `YYYY-MM-DD`.

The model can only add edges; it can't cause any other action.

Expect sparse, imperfect results from a small local model. Most mail states
no reporting line or deadline, so most messages add nothing. The model
sometimes confuses predicates (for example "manages" with a project as the
object), which is why these edges carry confidence 0.6 and agents are told
to present them as inferred. When a model puts a deadline's date in `object`,
imap-mcp moves it to `due`. Names with no letters or digits (placeholders
copied from the prompt) are dropped.

This is the one place message text is sent anywhere. It goes only to the
configured classify model, under the same gates as backfill enrichment
(quiet hours, GPU yield, datawatch capacity). Each message is read once,
`intelligence.kg_llm_per_tick` messages per tick. Turn it off with
`intelligence.kg_llm: false`. The header and tag graph is then still built.

## Anomalies

Anomaly detection uses the profiles and the scan (AGENT.md D22). Each finding
has a type, a severity, a plain description, and `details` with the numbers
behind it.

### Checks on each new message

These checks run during the header scan for newly arrived incoming mail, and
only:
- after the account's history scan is complete. The first scan, and the
  rescan after an upgrade, flag nothing. Otherwise "first message from this
  sender" would be wrong until every folder had been read.
- for mail from the last `intelligence.anomaly_lookback_days` (default 7).

Each check compares the message with the sender's profile *before* this
message is counted.

| Type | Severity | When |
|------|----------|------|
| `new_sender` | low | The first message ever from this address, in person-to-person mail (no list, bulk or automated headers), from someone you have never written to |
| `auth_failure` | high | The message fails DMARC (or DKIM, when it has no DMARC result), and at least `anomaly_auth_min_passes` (3) earlier messages from this sender passed. A sender whose mail always passed and now fails is a classic sign of spoofing |
| `lookalike_domain` | high | The sender's domain is not one you write to, but looks like one: one character off (two for domains of 10+ characters), or the same after folding look-alike characters (`rn`→`m`, `vv`→`w`, `0`→`o`, `1`/`i`→`l`). Domains shorter than 6 characters are not compared |
| `reply_to_mismatch` | medium | Person-to-person mail from a sender with at least 3 earlier messages, where replies would go to a different domain than the sender's |

Per-message findings carry `folder`, `uid` and `message_ref` (the
Message-ID), so an agent can open the message (`get_message`) or its
conversation (`get_thread`). Each message produces at most one finding per
type.

### Periodic checks

These run at the end of each scan tick, once the history scan is complete.

| Type | Severity | When |
|------|----------|------|
| `silence` | low | A person or colleague with at least `anomaly_silence_min_messages` (20) messages has been quiet longer than both `anomaly_silence_min_days` (30) and three times their usual gap between messages. It is reported while the silence is new (within one more `anomaly_silence_min_days`), not for people you lost touch with years ago, and resolves itself when they write again |
| `volume_spike` | medium | A sender's last 24 hours exceed both `anomaly_spike_min` (10) and `anomaly_spike_factor` (5) times their average per day |

Only one finding per sender and type is open at a time.

### Events, scores and resolution

- Each new finding publishes `anomaly.detected` on the event stream and to
  webhooks with `{id, type, severity}` only, never the sender. Receivers
  fetch the details with their own token (`GET /api/anomalies`).
- A sender's `anomaly_score` is their open findings weighted by severity:
  high 1, medium 0.5, low 0.2.
- `resolve_anomaly` (MCP, write scope) or `POST /api/anomalies/{id}/resolve`
  marks a finding reviewed. It stays in the log with `resolved: true` and
  drops out of the default listing and the score.

## Reply tracking

The same scan keeps, for every person-to-person conversation in your history,
its latest message (AGENT.md D32). A conversation is mail grouped by thread
root (the first References entry, else In-Reply-To, else the Message-ID), the
same key `get_thread` uses. Only your own messages to someone else and
incoming mail without list, bulk or auto-submitted headers count.

- **`needs_reply`**: the latest message is someone else's, addressed to you
  (To or Cc), and you have not answered it. Newsletters, bots, mail a
  `new_sender` rule held, and mail now in Trash, Junk or a hold folder are
  left out. People you have written to or replied to always count. A first
  contact counts only when all three agree it is a real person (D48): the
  classify model called the message a conversation or personal mail, its
  headers show none of the new-sender hold's bulk or scam signals (checked
  live), and the sender has no open anomaly other than `new_sender`.
- **`awaiting_reply`**: you wrote last and nobody has answered.

Both take `account`, `older_than_days` (default 2), `within_days` (default
90; use a large number such as 3650 for all history) and `limit`. Each item
has a `thread_id` (the same id `get_thread` takes), the `counterpart`, `subject`, `folder`, `uid`,
`message_ref` (Message-ID) and `days_waiting`. Longest-waiting first.

An item clears when (D33):

- **you reply**: your reply joins the conversation (or answers the message
  directly by In-Reply-To) once the scan sees it in Sent or All Mail;
- **the message is flagged `\Answered`**: `needs_reply` checks the listed
  messages' flags on the server each time, so a reply sent from a client
  that does not save to Sent still counts;
- **you dismiss it**: `dismiss_reply { account, thread_id }` (write scope) or
  `POST /api/replies/dismiss`. A newer message in the conversation brings
  it back.

Other than Trash, Junk and hold folders, where a message is filed does not
matter: rules move real conversations out of INBOX, so moving or archiving a
message does not clear it.

The 0.16 upgrade rescans all history once to fill this in. Until it
finishes, results carry `history_complete: false` and may miss older
conversations; the rest of the intelligence keeps working meanwhile.
`/api/health` shows its progress: `intelligence.reply_history_complete`,
and per account `rescan_complete` and `rescan_folders_remaining`.

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
  kg: true
  kg_stale_days: 365
  kg_llm: true
  kg_llm_per_tick: 10
  anomalies: true
  anomaly_lookback_days: 7
  anomaly_auth_min_passes: 3
  anomaly_silence_min_messages: 20
  anomaly_silence_min_days: 30
  anomaly_spike_min: 10
  anomaly_spike_factor: 5
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
| `kg` | true | `IMAP_MCP_INTELLIGENCE_KG` |
| `kg_stale_days` | 365 | `IMAP_MCP_INTELLIGENCE_KG_STALE_DAYS` |
| `kg_llm` | true | `IMAP_MCP_INTELLIGENCE_KG_LLM` |
| `kg_llm_per_tick` | 10 | `IMAP_MCP_INTELLIGENCE_KG_LLM_PER_TICK` |
| `anomalies` | true | `IMAP_MCP_INTELLIGENCE_ANOMALIES` |
| `anomaly_lookback_days` | 7 | `IMAP_MCP_INTELLIGENCE_ANOMALY_LOOKBACK_DAYS` |
| `anomaly_auth_min_passes` | 3 | `IMAP_MCP_INTELLIGENCE_ANOMALY_AUTH_MIN_PASSES` |
| `anomaly_silence_min_messages` | 20 | `IMAP_MCP_INTELLIGENCE_ANOMALY_SILENCE_MIN_MESSAGES` |
| `anomaly_silence_min_days` | 30 | `IMAP_MCP_INTELLIGENCE_ANOMALY_SILENCE_MIN_DAYS` |
| `anomaly_spike_min` | 10 | `IMAP_MCP_INTELLIGENCE_ANOMALY_SPIKE_MIN` |
| `anomaly_spike_factor` | 5 | `IMAP_MCP_INTELLIGENCE_ANOMALY_SPIKE_FACTOR` |

At 600 messages a minute, a mailbox of 100,000 messages takes about three
hours for its first scan. The scan shares each account's IMAP connection
with the tools and the sync, one batch at a time, so it doesn't block them
for long.

## Watching progress

`GET /api/health` has an `intelligence` block (counts only):

```json
"intelligence": {"enabled": true, "folders": 14, "folders_complete": 14, "backfill_complete": true,
                 "messages_indexed": 48210, "senders": 3120, "replies_paired": 912,
                 "kg_entities": 9400, "kg_relationships": 31200, "kg_model_messages": 420,
                 "roles": {"newsletter": 1210, "bot": 640, "vendor": 410, "personal": 380, "colleague": 95, "unknown": 385},
                 "last_scan": "2026-10-09T22:49:45Z"}
```

`intelligence.accounts` breaks the scan down per account, in config order. Health
needs no token, so it shows positions (`index`), never account names. The same
breakdown with names is at `GET /api/intelligence/status` (read scope):

```bash
curl -sS "$IMAP_MCP/api/intelligence/status" -H "Authorization: Bearer $READ_TOKEN" \
  | jq '.accounts[] | {account, folders_complete, folders, scanned, backfill_complete}'
```

`scanned` counts headers read in the current pass, including messages already
indexed (an upgrade rescan reads everything again).

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
counts and dates above. Since 0.17 the index also records the folder where
each message was last seen, and which messages a rule moved, for learning
from moves ([rules.md](rules.md#learning-from-your-moves)). The graph stores entity names (addresses, domains,
thread root Message-IDs, tags, and names the model read in a body) and the
relationships between them, with counts and dates. Message bodies are never
stored here. Subjects are stored for two things only: the latest message of
each person-to-person conversation (reply tracking: subject, counterpart,
Message-ID, folder and UID) and mail a `new_sender` rule held. Otherwise
subjects appear only in transient role prompts, and bodies only in transient
extraction prompts.
