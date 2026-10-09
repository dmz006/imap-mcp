# Known limitations

This page lists what imap-mcp does not do yet, as of 0.10.4, what you will see
because of it, and how to work around it where possible. Planned work is
tracked in [plans/README.md](plans/README.md).

| Limitation | You will see | Workaround |
|------------|--------------|------------|
| [Four MCP tools are stubs](#four-mcp-tools-are-stubs) | `<tool>: not yet implemented` | Use the tools listed below |
| [Intelligence tables are never filled](#intelligence-tables-are-never-filled) | Empty sender profiles, knowledge graph and anomalies | Use `top_senders`, `get_sender_history`, `/api/query` on `messages` |
| [`search_messages` is plain IMAP SEARCH](#search_messages-is-plain-imap-search) | `hall`/`wing`/`room` have no effect; INBOX only unless a folder is given | `semantic_search`, or `/api/query` filtered on `hall`/`wing`/`room` |
| [SMTP send uses password auth only](#smtp-send-uses-password-auth-only) | OAuth-only accounts cannot send | Configure a password or app password for SMTP |
| [No IMAP IDLE](#no-imap-idle) | New mail shows up in the cache after the next sync, not instantly | Lower `sync.interval_minutes`, or call `sync_account` |
| [PGP inbound gate fails closed](#pgp-inbound-gate-fails-closed) | Every inbound command rejected when `require_pgp` is on | Use the other gates |
| [datawatch capacity gate and LLM proxy need a different token](#datawatch-capacity-gate-and-llm-proxy-need-a-different-token) | Yield-to-datawatch and the `datawatch` classify provider do not work with the imap-mcp service token | Use the Ollama checks and the `ollama` classify provider |

## Four MCP tools are stubs

These tools are registered, appear in `tools/list` and have scopes, but every
call returns a tool error `<tool>: not yet implemented`:

| Tool | Scope | Instead |
|------|-------|---------|
| `get_thread` | read | The cache records a `thread_id` per message (from `References` / `In-Reply-To`). Query it with `/api/query` on the `messages` view, filtering on `thread_id`. |
| `get_attachments` | read | None for attachment content. `/api/query` on the `messages` view can filter on `has_attachments`. |
| `export_message` | write | `get_message`, then `write_file` into the output sandbox. |
| `cross_account_search` | read | Call `search_messages` once per account, or `semantic_search` with `account` omitted (searches all accounts' enriched cache). |

## Intelligence tables are never filled

The cache has tables for sender profiles (`senders`), a knowledge graph
(`kg_entities`, `kg_relationships`) and anomalies (`anomalies`), but nothing
writes to them yet. The code that builds them is planned work.

Effect:

- `get_sender_profile`, `kg_query` and `get_anomalies` return empty results.
- `GET /api/senders`, `/api/senders/{address}`, `/api/kg` and `/api/anomalies`
  return empty results.
- `/api/query` on the `senders`, `anomalies` and `kg` views returns no rows.
- The `anomaly.detected` event is never published, so webhooks subscribed to
  it never fire.

Workarounds: `top_senders` ranks a folder's senders, `get_sender_history`
lists a sender's messages, and `/api/query` on the `messages` view can group
and count by sender (see [query.md](query.md)).

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
