# Changelog

All notable changes to imap-mcp are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
[Semantic Versioning](https://semver.org/).

## [0.3.0] - 2026-06-14

Cleanup tooling and automation, built from the friction of a real ~16K-message
inbox cleanup. **42 MCP tools.**

### Added
- `purge_sender` — move ALL mail from a sender to Trash, draining the folder in
  one call; auto-detects the Trash mailbox (`\Trash` special-use / `[Gmail]/Trash`).
- `top_senders` — rank a folder's senders by count (address/domain), scanning the
  whole folder, so bulk/spam clusters surface in one call.
- Rules engine — `create_rule`, `list_rules`, `delete_rule`, `run_rules`. A rule is
  a match (from/subject/text/older_than_days) plus an action (trash/move/flag/seen),
  persisted in the `rules` table; `run_rules` supports `dry_run` to preview counts.
- `label_message` — apply a Gmail label by COPY into the label mailbox (creates it
  if missing).
- `empty_trash` — permanently delete everything in the auto-detected Trash mailbox.
- IMAP keepalive (NOOP every 4 min) with auto-reconnect on dropped connections.

### Changed
- `search_messages` now returns the true `total_matches` (previously capped at the
  page size, which hid real volumes behind "50").
- `/api/accounts` live-probes each connection (NOOP) instead of trusting pool
  membership, so a silently-dropped connection reports as disconnected.
- `create_folder` / `delete_folder` accept `folder` as an alias for `path`.

## [0.2.1] - 2026-06-07

Closes the datawatch comm loop (datawatch#127).

### Added
- `GET /api/events` — SSE event stream; fans out bus events to connected clients
  (15s heartbeats; slow clients dropped, never backpressure the bus). datawatch's
  `imap_mcp` backend consumes verified `inbound.command` events here.
- `POST /api/accounts/{account}/messages/send` — REST send via the account's SMTP
  (`account` may be `_default`); same semantics as the `send_message` MCP tool.

## [0.2.0] - 2026-06-07

datawatch integration across three independent, operator-opt-in layers (no
auto-injection).

### Added
- **Secrets** — `${secret:name}` credential references resolve via the datawatch
  secrets service when a `datawatch:` block is present; otherwise fully standalone
  (`${ENV}`/plain). Agent-scoped token (least privilege); clear startup error if a
  `${secret:}` reference is used without a datawatch block.
- **Skill** — companion usage skill published to the datawatch community registry
  at `skills/comms/imap-mcp`. Instructions only; pull-based; never auto-loaded.
- **Bidirectional comm**
  - Outbound: per-account `smtp:` config + `send_message` tool (header-injection
    guarded). **34 MCP tools.**
  - Inbound command channel: trust boundary with composable, default-deny gates —
    allowlist, DKIM/DMARC (Authentication-Results), HMAC over a fenced command
    envelope, nonce/replay, capability scoping. Emits `inbound.command` (verified)
    / `inbound.rejected` (audited).
  - PGP gate declared but **fails closed** — backlogged.

## [0.1.0]

Initial scaffold: multi-account IMAP connection pool, MCP server (stdio +
Streamable HTTP), REST API shell, SQLite cache (FTS5 + vectors), Ollama-backed
enrichment pipeline, enforced output sandbox, and the first message/folder/search
tools.

[0.3.0]: https://github.com/dmz006/imap-mcp/releases/tag/v0.3.0
[0.2.1]: https://github.com/dmz006/imap-mcp/releases/tag/v0.2.1
[0.2.0]: https://github.com/dmz006/imap-mcp/releases/tag/v0.2.0
