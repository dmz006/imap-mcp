# Sync cache

imap-mcp keeps a local copy of recent mail in `cache.db`. Semantic search,
enrichment, the query DSL and the analytics routes read this cache. Message
tools such as `list_messages`, `get_message` and `search_messages` read the
IMAP server directly.

The cache is disposable. You can delete it at any time, and it rebuilds
itself from IMAP. Rules, webhooks and other state live in `imap.db`. See
[encryption.md](encryption.md).

The syncer runs in both `imap-mcp serve` and stdio mode. `run-rules` never
opens the cache.

## Read-only against the server

The syncer never changes the mailbox:

- It opens folders with `EXAMINE` (read-only `SELECT`).
- It fetches bodies with `BODY.PEEK[]`, so `\Seen` is never set.
- When a message leaves the cache because it was expunged, moved, aged out of
  the window, swept or dropped from config, only the cached copy is deleted.

The syncer takes the account's connection lock once per IMAP step, not for a
whole folder. Interactive tools are therefore not blocked during a long
backfill.

## The sync cycle

Every `interval_minutes` (default 15), and once at startup when
`full_sync_on_start: true` (the default), the syncer runs these steps for
each account and each folder entry:

1. **Resolve** the entry to a mailbox (see [Folder selection](#folder-selection)).
2. **EXAMINE** it. If its UIDVALIDITY changed, drop that folder's cache and
   rebuild it.
3. **Search** with `UID SEARCH SINCE <date>`. The date is midnight UTC,
   `window_days` before today. `SINCE` compares the **INTERNALDATE**, which is
   when the server received the message, not the `Date:` header. With
   `keep_flagged`, the search is `OR SINCE <date> FLAGGED`.
4. **Gone** = cached minus found. Delete these from the cache and publish
   `message.deleted`.
5. **Flags** for messages already cached: fetch the changes (see
   [CONDSTORE](#incremental-flags-condstore)). Store any that differ and
   publish `message.updated`.
6. **New** = found minus cached. Fetch these newest first, 25 per batch, store
   them, queue them for enrichment and publish `message.synced`.
7. Save the folder's UIDVALIDITY and HIGHESTMODSEQ, and publish
   `folder.synced`.

Each account ends with `sync.complete`. A failed account publishes
`sync.error`. After all accounts, the [cleaning pass](#cleaning) runs.

To start a cycle for one account by hand, use the `sync_account` tool or
`POST /api/accounts/{account}/sync`. Both need the `admin` scope. A manual
sync does not run the cleaning pass.

## The window

The window is a number of days counted back by INTERNALDATE. The default is
30. You can override it per account and per folder. For each folder entry the
syncer uses the first value that is set (greater than 0):

1. `accounts[].sync.folder_window_days[<entry>]`
2. `sync.folder_window_days[<entry>]`
3. `accounts[].sync.window_days`
4. `sync.window_days`
5. `30`

The `<entry>` key is the folder entry exactly as it is written in `folders`,
for example `\Sent` or `INBOX`. It is not the resolved mailbox name.

- **Grow the window:** the next cycle finds older messages and fetches them.
  They go into the enrichment **backfill** lane.
- **Shrink the window:** the next cycle drops the messages that are now
  outside it, unless `keep_flagged` keeps them. This is how you purge the
  cache by age.

## Folder selection

`folders` lists the entries to cache. The default is `INBOX` and `\Sent`. An
entry is either:

- A **SPECIAL-USE token** (RFC 6154), with a leading backslash: `\Sent`,
  `\Archive`, `\Drafts`, `\Junk`, `\Trash`, `\Flagged`, `\All`. It matches a
  mailbox attribute, ignoring case, so `\Sent` resolves to `Sent` on one
  server and `[Gmail]/Sent Mail` on Gmail. This needs a server that supports
  `SPECIAL-USE`.
- A **literal mailbox name**, matched exactly. `INBOX` matches in any case.

If an entry does not resolve, the folder's stats show
`"error": "folder not found on server"`, and the other folders still sync. If
two entries resolve to the same mailbox, the syncer syncs it once, using the
first entry.

`accounts[].sync.folders` **replaces** the global list for that account. It
does not add to it.

### `\All`

On Gmail, `\All` is "All Mail", which holds every message. Selecting it
caches every message in the window, and the server logs a warning. The same
message then often sits in the cache twice, under `INBOX` and under `\All`.
See [Message-ID deduplication](#message-id-deduplication).

## Headers and bodies

The syncer caches these for every message:

- The envelope: Message-ID, subject, from, to, cc, reply-to and date.
- Flags, size and INTERNALDATE.
- A thread id, built from References and In-Reply-To.

The full message is fetched only when its size is at most `max_message_mb`
(default 25). From it, the syncer stores the text body, the HTML body and the
attachment **metadata**. Attachment content is never cached.

A larger message is cached headers-only (`body_skipped`). It still appears in
counts and listings, and it is enriched from its subject alone.
`max_message_mb: 0` turns off the cap, so every body is fetched.

## Incremental flags (CONDSTORE)

If the server supports `CONDSTORE` and reports a HIGHESTMODSEQ, the syncer
fetches flags with `FETCH ... (FLAGS) (CHANGEDSINCE <last modseq>)`. Only
messages whose flags changed come back. Otherwise, every cycle re-fetches the
flags of every cached message in the window. Both paths give the same
result. CONDSTORE is cheaper.

Each folder's stats show `condstore: true` or `false`.

## UIDVALIDITY rebuild

UIDs are valid only for one UIDVALIDITY. If a folder's UIDVALIDITY changes
(the server rebuilt or renamed it), the syncer drops that folder's whole
cache and fetches it again. The folder's stats show `rebuilt: true`, and the
log says `uidvalidity changed; folder cache rebuilt`.

If UIDVALIDITY changes in the middle of a cycle, that folder's cycle stops
with an error, and the next cycle rebuilds it.

## Message-ID deduplication

The same email can sit in several folders, for example INBOX and All Mail, or
a Gmail message with two labels. Each copy is cached under its own
account, folder and UID. Only the **first** copy is queued for enrichment.
Later copies with the same Message-ID get `enrichment_status = 'duplicate'`.
The `message.synced` event shows `"queued": false` for them. Messages without
a Message-ID are always queued.

## Cleaning

The cleaning pass touches the cache only.

### After every cycle

- **Stale accounts and folders.** If an account was removed from config, its
  cache is dropped. If a folder is no longer in `folders` or is gone from the
  server, its cache and sync state are dropped. Folder pruning happens at the
  end of each account's sync, but only when the folder list was read
  successfully.
- **Orphans.** Embedding vectors and enrichment queue rows whose message is
  gone are deleted. The full-text index gets an integrity check and is rebuilt
  if the check fails.
- **WAL checkpoint** (`TRUNCATE`).
- **VACUUM.** This runs at most once every `vacuum_interval_hours` (default
  24, `0` = never automatically). The first one runs one interval after
  startup.

Each pass publishes `cache.cleaned`:

```json
{"type": "cache.cleaned", "payload": {
  "stale_folders": [{"account": "work", "folder": "Old", "count": 120}],
  "orphans": {"vectors": 3, "queue_entries": 0, "fts_rebuilt": false},
  "vacuumed": true,
  "at": "2026-10-09T12:15:00Z"}}
```

### `keep_flagged`

With `keep_flagged: true`, `\Flagged` messages stay cached, and are fetched,
even when they are older than the window. The default is `false`.
`cache_sweep` can still delete them, and the next cycle fetches them again.

## On-demand: `cache_sweep`

The `cache_sweep` MCP tool and `POST /api/cache/sweep` both need the `admin`
scope. They delete chosen cached copies. Mailbox mail is never touched.

| Argument | Type | Default | Selects |
|----------|------|---------|---------|
| `account` | string | | One account |
| `folder` | string | | One **resolved** mailbox name, for example `[Gmail]/Sent Mail` |
| `older_than_days` | number | | INTERNALDATE (or Date) older than N days |
| `errors_only` | bool | false | Messages whose enrichment ended in `error` |
| `all` | bool | false | The whole cache, for a full rebuild |
| `dry_run` | bool | **true** | Count only |

The filters combine with AND. You must set at least one filter or
`all: true`. Otherwise the call is refused.

A dry run returns the per-folder counts:

```json
{"dry_run": true, "folders": [{"account": "work", "folder": "INBOX", "count": 2}], "total": 2,
 "note": "dry run: nothing deleted; repeat with dry_run=false to delete these cached copies (the mailbox is never touched)"}
```

With `dry_run: false`, the sweep does the following:

1. Deletes the selected cached messages.
2. Resets the sync state of every affected folder.
3. Cleans orphans.
4. Runs VACUUM.
5. Publishes `cache.cleaned` with the result, which includes `orphans`.

Messages still inside the window come back on the next cycle. Because the sync
state was reset, they come back as backfill.

```bash
# Re-fetch and re-enrich messages whose enrichment failed
curl -sS -X POST http://127.0.0.1:8765/api/cache/sweep \
  -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{"errors_only": true, "dry_run": false}'
```

```
cache_sweep { account: "work", folder: "INBOX", older_than_days: 60 }
cache_sweep { all: true, dry_run: false }   # full rebuild
```

## Config

```yaml
sync:
  interval_minutes: 15        # cycle period (≤ 0 falls back to 15)
  full_sync_on_start: true    # run a cycle at startup
  folders:                    # SPECIAL-USE tokens or literal names
    - INBOX
    - \Sent
  window_days: 30             # by INTERNALDATE
  folder_window_days:         # key = entry as written above
    \Sent: 90
  max_message_mb: 25          # larger messages are cached headers-only; 0 = no cap
  keep_flagged: false
  vacuum_interval_hours: 24   # 0 = no automatic VACUUM

accounts:
  - name: archive
    # ...imap/auth...
    sync:                     # optional per-account overrides
      folders: [INBOX, \Archive]   # replaces sync.folders for this account
      window_days: 365
      folder_window_days:
        INBOX: 30
```

| Environment variable | Overrides |
|----------------------|-----------|
| `IMAP_MCP_SYNC_INTERVAL_MINUTES` | `sync.interval_minutes` |
| `IMAP_MCP_SYNC_WINDOW_DAYS` | `sync.window_days` |
| `IMAP_MCP_SYNC_MAX_MESSAGE_MB` | `sync.max_message_mb` |
| `IMAP_MCP_SYNC_KEEP_FLAGGED` | `sync.keep_flagged` |
| `IMAP_MCP_SYNC_VACUUM_INTERVAL_HOURS` | `sync.vacuum_interval_hours` |
| `IMAP_MCP_DB_CACHE_PATH` | `db.cache.path` |

The global settings appear under `sync` in `GET /api/health`. The results of
each folder's last sync appear in `GET /api/accounts/{account}/stats` and in
the `folder.synced` events.

## Limits

- The syncer polls on the interval. It does not use IMAP IDLE yet, so new mail
  appears in the cache on the next cycle at the latest.
- Each cycle searches the window of every folder. A very large window on a
  large folder makes every cycle slower, even with CONDSTORE.
