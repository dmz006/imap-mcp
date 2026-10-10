# Deployment

This page covers running `imap-mcp serve` as a systemd user service, scheduling
`run-rules`, reading logs, upgrading and rolling back.

- [Install the binary](#install-the-binary)
- [The environment file](#the-environment-file)
- [systemd user service](#systemd-user-service)
- [Scheduling run-rules](#scheduling-run-rules)
- [Logs](#logs)
- [Stopping cleanly](#stopping-cleanly)
- [Upgrading](#upgrading)
- [Rolling back](#rolling-back)

## Install the binary

```bash
git clone https://github.com/dmz006/imap-mcp /path/to/imap-mcp
cd /path/to/imap-mcp
go build -o ~/.local/bin/imap-mcp ./cmd/imap-mcp
imap-mcp version
```

Install to a fixed path outside the source tree, so rebuilding in the checkout
never replaces the binary a running service or scheduled job uses.

Commands used below:

| Command | Purpose |
|---------|---------|
| `imap-mcp serve [--config PATH]` | HTTP server: MCP at `/mcp`, REST at `/api`. Default bind `127.0.0.1:8765`. |
| `imap-mcp run-rules [--dry-run] [--config PATH]` | Apply all active rules once and exit. |
| `imap-mcp db encrypt [--only state\|cache] [--config PATH]` | Encrypt existing plaintext databases. See [encryption.md](encryption.md). |
| `imap-mcp version` | Print the version. |

Without `--config`, imap-mcp uses `./config.yaml` if it exists, otherwise
`~/.config/imap-mcp/config.yaml`. Pass `--config` explicitly in services and
scheduled jobs so the working directory does not matter.

## The environment file

Keep every secret the config references as `${NAME}` in one env file, mode
0600, outside any repository:

```bash
umask 077
mkdir -p ~/.config/imap-mcp
$EDITOR ~/.config/imap-mcp/imap-mcp.env
chmod 600 ~/.config/imap-mcp/imap-mcp.env
```

```text
# ~/.config/imap-mcp/imap-mcp.env
DATAWATCH_API_URL=https://127.0.0.1:8443
IMAP_MCP_DATAWATCH_TOKEN=<datawatch service token>
IMAP_MCP_TOKEN_CLAUDE=<64 hex characters>
```

Write plain `NAME=value` lines with no quotes, no spaces around `=` and no
`export`. That format works both as a systemd `EnvironmentFile=` and when
sourced by `sh`.

If every secret is a `${secret:}` reference resolved by datawatch, the file
needs only `DATAWATCH_API_URL` and `IMAP_MCP_DATAWATCH_TOKEN`. See
[auth-tokens.md](auth-tokens.md#the-datawatch-service-token).

## systemd user service

`~/.config/systemd/user/imap-mcp.service`:

```ini
[Unit]
Description=imap-mcp (MCP + REST for IMAP)
# Only needed when the config uses ${secret:} references resolved by datawatch.
# Adjust the unit name to match how datawatch runs on your machine.
After=network-online.target datawatch.service
Wants=network-online.target datawatch.service
# Give up after 5 failed starts within 10 minutes instead of looping forever.
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
Type=simple
EnvironmentFile=%h/.config/imap-mcp/imap-mcp.env
ExecStart=%h/.local/bin/imap-mcp serve --config %h/.config/imap-mcp/config.yaml
# Files created by the service (databases, sandbox output) are private.
UMask=0077
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now imap-mcp
systemctl --user status imap-mcp
curl -s http://127.0.0.1:8765/api/health | jq '{status, version, auth, storage}'
```

To keep the service running while you are logged out:

```bash
loginctl enable-linger "$USER"
```

Why these settings:

- **`After=` / `Wants=` datawatch.** `serve` resolves `${secret:}` references
  (account passwords, tokens, database keys) at startup and refuses to start if
  datawatch is unreachable. Ordering after datawatch avoids a failed first
  start at boot. `After=` orders only units in the same systemd instance: if
  datawatch runs as a system service or elsewhere, drop it from these lines and
  rely on `Restart=`.
- **`EnvironmentFile=`** supplies the `${NAME}` values. The file stays 0600;
  the values never appear in the unit.
- **`UMask=0077`.** imap-mcp already creates its databases and their WAL/SHM
  files with mode 0600. The umask covers everything else the process creates.
- **`Restart=on-failure` with `StartLimitIntervalSec`.** Startup failures that
  fix themselves, such as datawatch not being up yet, are retried. An IMAP
  account that cannot connect does not stop startup: it is logged and the
  server runs without it. Configuration errors, such as a wrong key or a
  missing token, fail the same way every time, and the start limit stops the loop so the error is
  easy to find in the journal.
- A clean `systemctl --user stop` exits with status 0 (see
  [Stopping cleanly](#stopping-cleanly)), so `on-failure` does not restart it.

## Scheduling run-rules

`imap-mcp run-rules` connects to every account, applies all active rules once,
prints one line per rule plus a summary, and exits. It is meant for a
scheduler.

- It opens only the state DB (`imap.db`) and needs only the state key, never
  the cache key. See [encryption.md](encryption.md#run-rules-needs-only-the-state-key).
- It shares `imap.db` with a running `serve` safely (WAL mode with a busy
  timeout).
- Each run is limited to 15 minutes.
- `--dry-run` reports match counts without acting.
- It publishes `rule.fired` events to the webhook outbox in `imap.db`; a running
  `serve` delivers them. See [webhooks.md](webhooks.md).
- It exits non-zero if the config, keys or state DB cannot be loaded or
  opened. An account that cannot connect is logged (`connect account`) and
  skipped; the run continues with the others. A rule that fails is printed
  with `ERROR:` and does not stop the others.

Rules themselves are covered in [rules.md](rules.md).

### Wrapper script

A scheduler does not load your env file, so `${NAME}` references would stay
unresolved and `${secret:}` references would have no datawatch token. Wrap the
command:

`~/.local/bin/imap-mcp-run-rules`:

```sh
#!/bin/sh
# Run imap-mcp rules once with the service's environment loaded, so ${ENV}
# and ${secret:} references in the config resolve.
set -eu

ENV_FILE="${IMAP_MCP_ENV_FILE:-$HOME/.config/imap-mcp/imap-mcp.env}"
CONFIG="${IMAP_MCP_CONFIG:-$HOME/.config/imap-mcp/config.yaml}"
BIN="${IMAP_MCP_BIN:-$HOME/.local/bin/imap-mcp}"

umask 077
set -a          # export every variable the env file defines
. "$ENV_FILE"
set +a

exec "$BIN" run-rules --config "$CONFIG" "$@"
```

```bash
chmod 700 ~/.local/bin/imap-mcp-run-rules
~/.local/bin/imap-mcp-run-rules --dry-run      # test it
```

### cron

```cron
# m h dom mon dow  command
5 * * * *  $HOME/.local/bin/imap-mcp-run-rules >> $HOME/.local/state/imap-mcp/run-rules.log 2>&1
```

Create the log directory first (`mkdir -p ~/.local/state/imap-mcp`).

### systemd timer

As an alternative to cron, a timer keeps the output in the journal:

`~/.config/systemd/user/imap-mcp-run-rules.service`:

```ini
[Unit]
Description=imap-mcp run-rules
After=datawatch.service

[Service]
Type=oneshot
ExecStart=%h/.local/bin/imap-mcp-run-rules
UMask=0077
```

`~/.config/systemd/user/imap-mcp-run-rules.timer`:

```ini
[Unit]
Description=Run imap-mcp rules hourly

[Timer]
OnCalendar=hourly
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now imap-mcp-run-rules.timer
```

### datawatch scheduled job

If you schedule work through datawatch, point the job's command at the same
wrapper script, for example `$HOME/.local/bin/imap-mcp-run-rules`. Set the
schedule (for example hourly) as the datawatch documentation describes. The
wrapper is what makes the job independent of the environment datawatch
starts it with.

## Logs

imap-mcp logs to stderr. Under systemd, that goes to the journal:

```bash
journalctl --user -u imap-mcp -f
journalctl --user -u imap-mcp -p warning --since today
```

| Setting | Values | Default |
|---------|--------|---------|
| `log.level` | `debug`, `info`, `warn`, `error` | `info` |
| `log.format` | `text`, `json` | `text` |
| `IMAP_MCP_LOG_LEVEL` env | overrides `log.level` | |

Useful lines to look for:

| Message | Meaning |
|---------|---------|
| `server listening addr=... auth=true` | Server is up. |
| `server auth enabled tokens=[...]` | Token names loaded. |
| `server.auth.disabled is set ...` (WARN) | Auth is off. See [auth-tokens.md](auth-tokens.md#disabling-auth-insecure). |
| `storage state_encrypted=... cache_encrypted=...` | Which database files are encrypted. |
| `auth: rejected request ... reason=...` (WARN) | A request without a valid token. A steady stream usually means an MCP client looping on OAuth discovery; see [auth-tokens.md](auth-tokens.md#a-client-loops-on-oauth-discovery-after-a-401). |
| `auth: scope denied token=... required=...` (WARN) | A valid token without the needed scope. |
| `migrated legacy imap.db ... backup=...` | The one-time 0.6.0 split ran. |
| `forcing open connections closed at shutdown` (WARN) | A request ignored shutdown for 5 s and was cut off. |

Token values and encryption keys are never logged.

## Stopping cleanly

On `SIGTERM` or `SIGINT` (what `systemctl stop` sends), `serve`:

1. stops accepting new connections;
2. ends long-lived streams immediately, including the `/api/events` SSE stream
   and MCP streamable GET connections, because requests share the server's
   context;
3. waits up to 5 seconds for other in-flight requests;
4. closes any connection still open after that, logs a warning, and exits with
   status 0.

Before 0.10.3, a stop while any client held a stream open (for example the
datawatch backend's `/api/events` subscription) exited with status 1 and
`context deadline exceeded`. systemd recorded that as a failure and, with
`Restart=on-failure`, could restart the service you had just stopped. Upgrade
to 0.10.4 or later.

## Upgrading

### General procedure

1. **Read the [CHANGELOG](../CHANGELOG.md)** for every version between yours
   and the new one. Look for config changes and anything marked **Upgrade**.
2. **Back up the state DB** (`imap.db`). It holds your rules, webhooks and
   webhook secrets and cannot be rebuilt. See
   [encryption.md](encryption.md#backups). Keep the old binary too:

   ```bash
   systemctl --user stop imap-mcp
   install -m 600 ~/.local/share/imap-mcp/imap.db ~/imap-mcp-backup/imap.db.$(imap-mcp version)
   cp ~/.local/bin/imap-mcp ~/.local/bin/imap-mcp.$(imap-mcp version)
   ```

3. Build and install the new binary (see [Install the binary](#install-the-binary)).
4. Update the config and every client for any new requirements.
5. Start and check:

   ```bash
   systemctl --user start imap-mcp
   journalctl --user -u imap-mcp -n 50
   curl -s http://127.0.0.1:8765/api/health | jq '{version, auth, storage}'
   ```

### Version notes

| Upgrading past | What changes for you |
|----------------|----------------------|
| 0.5.3 | `serve` requires bearer tokens. Add `server.auth.tokens`, and add the header to **every client** (Claude Code, the datawatch backend, scripts) **before** upgrading, or they all get 401. See [auth-tokens.md](auth-tokens.md). |
| 0.6.0 | One-time storage split into `imap.db` + `cache.db` (below). New SQLite driver. Optional encryption becomes available. |
| 0.7.0 | The cache is filled by a real sync. A 0.6.0 cache is dropped and rebuilt from IMAP on first start. |
| 0.9.0 | Cache schema v3 (enrichment queue lanes). The cache is rebuilt from IMAP on first start, and all cached mail is enriched again. |
| 0.10.0 | `${secret:}` references resolve through datawatch's external-service endpoint: needs datawatch v8.75.0 or later, a service token from `datawatch secrets mint-service-token imap-mcp`, and every secret scoped `service:imap-mcp`. New: `datawatch.ca_file`, `imap-mcp db encrypt`, webhooks, `/api/query`. |
| 0.10.3 | Clean exit on stop with streams open. |
| 0.10.4 | Microsoft 365 / Outlook `xoauth2` requests the Microsoft IMAP scope, so Microsoft OAuth accounts work. `auth-setup` checks a random OAuth `state` and listens only on loopback (`127.0.0.1` and `::1`, port 8766). Reading mail (`get_message`, `get_headers`, `detect_subscriptions`, the inbound watcher) no longer marks it read. |
| 0.11.0 | `get_thread`, `get_attachments`, `export_message`, `cross_account_search` work. New `tools:` config block (defaults are fine). Attachment downloads and exports need the `write` scope. |
| 0.12.0 | **Back up `imap.db` first.** The intelligence tables move from `cache.db` to `imap.db` (the empty cache copies are dropped; the cache is not rebuilt). A header scanner then reads all of your history once, at `intelligence.backfill_per_minute` (default 600 a minute). Expect a few hours for a large mailbox, with `/api/health` showing progress. Set `intelligence.enabled: false` to skip it. See [intelligence.md](intelligence.md). |
| 0.13.0 | **Back up `imap.db` first.** Columns are added to `imap.db` in place. On first start the header scan starts again from the beginning to build the knowledge graph for all history. Profile counts don't change. It takes as long as the 0.12 scan. Set `intelligence.kg: false` to skip the graph, or `intelligence.kg_llm: false` to keep message bodies away from the model. |
| 0.14.0 | **Back up `imap.db` first.** Columns are added to `anomalies` in place; no rescan. Anomaly detection starts once the history scan is complete, and only for new mail, so expect a few findings a day, not a flood. Set `intelligence.anomalies: false` to turn it off. New tool `resolve_anomaly` (write scope). |
| 0.16.0 | **Back up `imap.db` first.** A column is added to `intel_scan` and two tables are created. On first start the header scan reads all of your history once more to build reply tracking; profiles, anomaly detection and the new-sender hold keep working meanwhile, and nothing is counted twice. It takes as long as the 0.12 scan. `needs_reply` and `awaiting_reply` report `history_complete: false` until it finishes. New tools `needs_reply`, `awaiting_reply` (read) and `dismiss_reply` (write). |

### The one-time 0.6.0 split

Before 0.6.0, everything lived in a single `imap.db`. The first time 0.6.0 or
later opens such a file (`serve`, stdio mode or `run-rules`), it splits it:

1. Backs the file up with `VACUUM INTO` to
   `imap.db.bak-<YYYYMMDD-HHMMSS>-pre-0.6.0` (mode 0600), next to the original.
2. Fingerprints every row of `rules`, `webhooks` and `inbound_nonces`, and
   checks that the backup has the same fingerprints.
3. In one transaction, drops the cache tables from `imap.db`, checks that the
   state fingerprints are unchanged, and commits. Any difference rolls back.
4. Compacts `imap.db` with `VACUUM` and logs
   `migrated legacy imap.db: cache tables moved to cache.db (rebuilt from IMAP); state verified unchanged`
   with the backup path and row counts.

`cache.db` is created empty and filled by the next sync. The split is safe if
`serve` and `run-rules` start at the same moment: the second one waits and then
finds the work done. It does nothing for new files, already-split files and
encrypted files.

**Do not set an encryption key until the split has run.** The split works only
on a plaintext file, and a key on a plaintext file stops startup. Upgrade
first, start once, then follow
[encryption.md](encryption.md#encrypt-an-existing-install-imap-mcp-db-encrypt).
`db encrypt` lists the pre-0.6.0 backup as a plaintext copy for you to delete
when you no longer need it.

### Cache schema resets

`cache.db` stores a schema version in SQLite's `user_version` (currently 3).
On open, if the stored version differs from the binary's, imap-mcp drops the
cache tables and recreates them empty. That is safe because every row comes
back from IMAP, but expect after such an upgrade:

- a full sync of every configured folder within the sync window, and
- enrichment (embeddings and classification) of all of it again, which can
  keep Ollama busy for a while. Backfill limits apply; see
  [enrichment.md](enrichment.md).

Semantic search and the intelligence views are incomplete until enrichment
catches up. Check progress with `enrichment_status` or
`GET /api/enrichment/status`.

The state DB is never reset this way.

## Rolling back

1. Stop the service and any scheduled `run-rules`.
2. Put the previous binary back:

   ```bash
   install -m 755 ~/.local/bin/imap-mcp.<old-version> ~/.local/bin/imap-mcp
   ```

3. Restore the old config if you changed it for the upgrade.
4. Start the service.

What to expect from the data:

- **Cache.** If the old binary uses a different cache schema version, it drops
  and rebuilds the cache from IMAP, as above. Nothing is lost.
- **State DB.** Only tables are added between versions, and a binary creates
  only the tables it knows. If you hit a problem anyway, restore the state DB
  backup you took before upgrading, losing any rule or webhook changes made
  since.
- **Encrypted files** cannot be opened by versions before 0.6.0. Versions
  before 0.10.0 cannot resolve `${secret:}` references with a service token,
  because they use an older datawatch endpoint.
- **Rolling back past 0.6.0** means restoring the
  `imap.db.bak-<timestamp>-pre-0.6.0` backup as `imap.db` (with the old binary
  and the service stopped). Rules or webhooks created since the split are lost.
- **Rolling back past 0.5.3** removes token auth. Remove the tokens from the
  clients too, and remember the server then accepts unauthenticated local
  requests.
