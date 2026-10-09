# Known limitations

This page lists what imap-mcp does not do yet, as of 0.13.0, what you will see
because of it, and how to work around it where possible. Planned work is
tracked in [plans/README.md](plans/README.md).

| Limitation | You will see | Workaround |
|------------|--------------|------------|
| [Cross-account search covers the cache window by default](#cross-account-search-covers-the-cache-window-by-default) | Older mail missing from `cross_account_search` | Pass `live: true` |
| [Anomalies are not detected yet](#anomalies-are-not-detected-yet) | `get_anomalies` returns nothing | `get_sender_profile` (counts, reply times, DKIM/DMARC results) |
| [Sender profiles fill in during the first scan](#sender-profiles-fill-in-during-the-first-scan) | Partial counts and `unknown` roles for a while after first start | Wait for `scan_complete: true` |
| [`search_messages` is plain IMAP SEARCH](#search_messages-is-plain-imap-search) | `hall`/`wing`/`room` have no effect; INBOX only unless a folder is given | `semantic_search`, or `/api/query` filtered on `hall`/`wing`/`room` |
| [SMTP send uses password auth only](#smtp-send-uses-password-auth-only) | OAuth-only accounts cannot send | Configure a password or app password for SMTP |
| [No IMAP IDLE](#no-imap-idle) | New mail shows up in the cache after the next sync, not instantly | Lower `sync.interval_minutes`, or call `sync_account` |
| [PGP inbound gate fails closed](#pgp-inbound-gate-fails-closed) | Every inbound command rejected when `require_pgp` is on | Use the other gates |
| [datawatch capacity gate and LLM proxy need a different token](#datawatch-capacity-gate-and-llm-proxy-need-a-different-token) | Yield-to-datawatch and the `datawatch` classify provider do not work with the imap-mcp service token | Use the Ollama checks and the `ollama` classify provider |

## Cross-account search covers the cache window by default

`cross_account_search` (and `GET /api/search/cross`) searches the local cache
by default. That covers every cached folder of every account, but only mail
inside the sync window (default 30 days). With `live: true` it runs IMAP
SEARCH on each account instead, which covers full history but only one folder
per account (default INBOX). `get_thread` has no such gap: it searches the
server automatically when a thread reaches outside the window.

## Anomalies are not detected yet

Sender profiles and the knowledge graph are built (see
[intelligence.md](intelligence.md)). The anomaly log (`anomalies`) is not
filled yet; that is planned work
([plan](plans/2026-10-09-intelligence-and-stubs.md), P4).

Effect:

- `get_anomalies` and `GET /api/anomalies` return empty results, and so does
  the `anomalies` list in a sender profile.
- `/api/query` on the `anomalies` view returns no rows.
- The `anomaly.detected` event is never published, so webhooks subscribed to
  it never fire.

Workaround: a sender profile already carries the signals the detector will
use: first contact, counts, reply times and DKIM/DMARC pass/fail counts.

## Sender profiles fill in during the first scan

After the first start (or an upgrade from 0.11 or 0.12), the scanner reads all of
your history at `intelligence.backfill_per_minute` messages per minute. A
large mailbox takes hours. Until it finishes, counts are partial and many
roles are `unknown`. Each profile carries `scan_complete`, and
`/api/health` shows progress under `intelligence`. The knowledge graph fills in alongside. Roles and relations from the
classify model arrive more slowly still: a few per tick, and only while the
enrichment gates allow it.

## `search_messages` is plain IMAP SEARCH

`search_messages` (and `GET /api/search`) sends an IMAP `SEARCH` to the server:

- It searches one folder, `INBOX` unless you pass `folder`.
- It matches `from`, `subject`, body `text` and dates with the server's own
  search rules.
- The `hall`, `wing` and `room` parameters are accepted but **ignored**.
- It does not use the local FTS5 index; there is no combined full-text and
  semantic ranking.

Workarounds:

- `semantic_search` (`POST /api/search/semantic`) ranks cached, enriched mail
  by meaning. It needs enrichment to have run.
- `/api/query` on the `messages` view can filter on `hall`, `wing` and `room`
  (needs the `admin` scope). See [query.md](query.md).

## SMTP send uses password auth only

`send_message` and `POST /api/accounts/{account}/messages/send` authenticate to
SMTP with `PLAIN` using `smtp.password`, which defaults to the account's
`auth.password`. There is no OAuth (XOAUTH2) for SMTP.

Effect: an account that uses `xoauth2` or `xoauth2_service_account` for IMAP,
and has no password, cannot send.

Workaround: give the account's `smtp:` block a username and password the SMTP
server accepts, such as an app password where the provider offers one.

## No IMAP IDLE

imap-mcp does not hold IMAP IDLE connections. New mail reaches the cache on the
next background sync (`sync.interval_minutes`, default 15). The inbound command
channel polls its watch folder every 60 seconds.

Effect: semantic search, enrichment and `message.synced` events lag new mail
by up to one sync interval. Tools that talk to IMAP directly, such as
`list_messages` and `search_messages`, see new mail immediately.

Workarounds: lower `sync.interval_minutes`, or call `sync_account`
(`POST /api/accounts/{account}/sync`, `admin` scope) when you need the cache
current. See [sync-cache.md](sync-cache.md).

## PGP inbound gate fails closed

`inbound.gates.require_pgp` is accepted, but the PGP check is not implemented.
When it is on, every inbound command is rejected (`pgp gate not yet
implemented (backlog) — fails closed`). Use the allowlist, DKIM/DMARC and HMAC
gates instead.

## datawatch capacity gate and LLM proxy need a different token

imap-mcp sends `datawatch.token` to every datawatch endpoint it calls. The
imap-mcp service token from `datawatch secrets mint-service-token` is accepted
only by the secrets endpoint. datawatch's capacity endpoint (used by
`enrichment.yield.datawatch_pools`) and its LLM proxy (used by
`enrichment.classify.provider: datawatch`) need a token with other
capabilities, and imap-mcp has no separate setting for one yet.

Effect: with the service token, those two features do not work. Secrets
resolution is unaffected.

Workaround: leave `yield.datawatch_pools` empty and use the `ollama` classify
provider. The Ollama-based yield check (`yield.max_foreign_resident_gb`) does
not need datawatch. See [enrichment.md](enrichment.md).
