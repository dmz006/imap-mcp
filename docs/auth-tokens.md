# Token auth and scopes

`imap-mcp serve` exposes MCP at `/mcp` and the REST API at `/api`. Every
request to either needs a named bearer token, and each token carries only the
scopes its client needs. This page shows how to create tokens, store them
safely, connect clients, and diagnose failures.

Token auth applies only to `imap-mcp serve`. **stdio mode** (`imap-mcp` with no
subcommand, launched by an MCP client) and **`imap-mcp run-rules`** never read
`server.auth` and need no token.

- [Quick start](#quick-start)
- [The `server.auth` block](#the-serverauth-block)
- [Scopes](#scopes)
- [Route and tool scope reference](#route-and-tool-scope-reference)
- [Generating and storing tokens](#generating-and-storing-tokens)
- [The datawatch service token](#the-datawatch-service-token)
- [Connecting clients](#connecting-clients)
- [401 vs 403](#401-vs-403)
- [Disabling auth (insecure)](#disabling-auth-insecure)
- [Troubleshooting](#troubleshooting)

## Quick start

```bash
# 1. Generate a token into a private env file (never echoed).
umask 077
mkdir -p ~/.config/imap-mcp
printf 'IMAP_MCP_TOKEN_CLAUDE=%s\n' "$(openssl rand -hex 32)" >> ~/.config/imap-mcp/imap-mcp.env
```

```yaml
# 2. Reference it in config.yaml.
server:
  host: 127.0.0.1
  port: 8765
  auth:
    tokens:
      - name: claude
        token: ${IMAP_MCP_TOKEN_CLAUDE}
        scopes: [read, write, send, admin]
```

```bash
# 3. Start the server with the env file loaded, then call it.
set -a; . ~/.config/imap-mcp/imap-mcp.env; set +a
imap-mcp serve --config ~/.config/imap-mcp/config.yaml &
curl -s -H "Authorization: Bearer $IMAP_MCP_TOKEN_CLAUDE" http://127.0.0.1:8765/api/accounts
```

## The `server.auth` block

```yaml
server:
  host: 127.0.0.1        # default
  port: 8765             # default
  auth:
    disabled: false      # insecure opt-out; see below
    tokens:
      - name: claude
        token: ${IMAP_MCP_TOKEN_CLAUDE}          # or ${secret:imap_mcp_token_claude}
        scopes: [read, write, send, admin]
      - name: datawatch
        token: ${secret:imap_mcp_token_datawatch}
        scopes: [read, send]
```

| Field | Rules |
|-------|-------|
| `tokens[].name` | Required. Must match `[A-Za-z0-9._-]{1,64}`. Unique. Logged on every rejected or scope-denied request; use a name that identifies the client. |
| `tokens[].token` | The bearer value after resolution. At least **32 characters**. Values must be unique across tokens. Write it as a `${secret:name}` or `${ENV}` reference. A literal value is accepted but then sits in the config file. |
| `tokens[].scopes` | At least one of `read`, `write`, `send`, `admin`. An unknown scope stops startup. |
| `disabled` | `true` turns auth off. Env override: `IMAP_MCP_SERVER_AUTH_DISABLED`. |

Startup rules (`serve` checks these before it connects to anything):

- With auth enabled, at least one token is required. Otherwise:
  `server.auth: no tokens configured; add server.auth.tokens or set server.auth.disabled: true (insecure)`.
- Every token is resolved and validated even when `disabled: true`. A token
  that cannot be resolved always stops startup.
- An `${ENV}` reference whose variable is unset is left in place and rejected:
  `token reference is unresolved (environment variable unset?)`.
- A `${secret:}` reference needs a `datawatch:` block with `api_url` and
  `token` (see [below](#the-datawatch-service-token)).

On success the log shows the token names, never the values:

```text
level=INFO msg="server auth enabled" tokens="[claude datawatch]"
```

How the check works: the server hashes the presented token with SHA-256 and
compares it in constant time against every configured token. The
`Authorization` scheme is case-insensitive (`Bearer` or `bearer`).

## Scopes

| Scope | Grants |
|-------|--------|
| `read` | Accounts, folders, messages, search, analytics and intelligence reads, rules listing, enrichment status, the `/api/events` stream, reading the output sandbox. |
| `write` | Mailbox changes (move, copy, delete, flags, labels, folders, purge, empty trash, append), rule changes and runs, writing to the output sandbox. |
| `send` | Outbound mail over SMTP. |
| `admin` | Webhooks, the `/api/query` DSL, sync and enrichment triggers, cache sweeps. |

Scopes do not imply each other: a token with `write` but not `read` cannot list
messages. Give each client only what it needs, for example:

| Client | Suggested scopes |
|--------|------------------|
| Interactive assistant that triages mail | `read, write` (add `send` only if it should send) |
| datawatch `imap_mcp` messaging backend | `read, send` |
| Dashboard or read-only reporting | `read` |
| Operator scripts for webhooks, `/api/query`, sync triggers | `admin` (plus `read` if they also read mail) |

## Route and tool scope reference

`/api/health` is the only path that needs no token. Any other path, including
unknown ones, needs a valid token.

### REST routes

| Method | Path | Scope |
|--------|------|-------|
| GET | `/api/health` | none |
| GET | `/api/events` | read |
| GET | `/api/accounts` | read |
| POST | `/api/accounts/{account}/sync` | admin |
| GET | `/api/accounts/{account}/folders` | read |
| GET | `/api/accounts/{account}/folders/{folder}/messages` | read |
| GET | `/api/accounts/{account}/folders/{folder}/messages/{uid}` | read |
| DELETE | `/api/accounts/{account}/folders/{folder}/messages/{uid}` | write |
| PUT | `/api/accounts/{account}/folders/{folder}/messages/{uid}/flags` | write |
| POST | `/api/accounts/{account}/folders/{folder}/messages/{uid}/move` | write |
| GET | `/api/accounts/{account}/folders/{folder}/messages/{uid}/attachments` | read |
| GET | `/api/accounts/{account}/folders/{folder}/messages/{uid}/attachments/{part}` | write |
| GET | `/api/accounts/{account}/folders/{folder}/messages/{uid}/export.eml` | write |
| POST | `/api/export` | write |
| GET | `/api/threads/{thread_id}` | read |
| GET | `/api/search` | read |
| GET | `/api/search/cross` | read |
| POST | `/api/search/semantic` | read |
| GET | `/api/accounts/{account}/stats` | read |
| GET | `/api/senders` | read |
| GET | `/api/senders/{address}` | read |
| GET | `/api/kg` | read |
| GET | `/api/intelligence/status` | read |
| GET | `/api/anomalies` | read |
| GET | `/api/replies/needed` | read |
| GET | `/api/replies/awaiting` | read |
| POST | `/api/replies/dismiss` | write |
| POST | `/api/anomalies/{id}/resolve` | write |
| GET | `/api/enrichment/status` | read |
| POST | `/api/enrichment/trigger` | admin |
| POST | `/api/cache/sweep` | admin |
| GET | `/api/webhooks` | admin |
| POST | `/api/webhooks` | admin |
| DELETE | `/api/webhooks/{id}` | admin |
| POST | `/api/webhooks/{id}/enable` | admin |
| POST | `/api/webhooks/{id}/test` | admin |
| GET | `/api/webhooks/{id}/deliveries` | admin |
| GET | `/api/rules` | read |
| POST | `/api/rules` | write |
| PUT | `/api/rules/{id}` | write |
| DELETE | `/api/rules/{id}` | write |
| POST | `/api/rules/{id}/test` | write |
| GET | `/api/query` | admin |
| POST | `/api/query` | admin |
| POST | `/api/accounts/{account}/messages/send` | send |

See [rest-api.md](rest-api.md) for parameters and responses.

### MCP tools

| Scope | Tools |
|-------|-------|
| read | `list_accounts`, `list_folders`, `list_messages`, `get_message`, `get_thread`, `get_headers`, `get_attachments` (listing), `search_messages`, `cross_account_search`, `semantic_search`, `summarize_folder`, `detect_subscriptions`, `get_sender_history`, `get_sender_profile`, `kg_query`, `get_anomalies`, `needs_reply`, `awaiting_reply`, `enrichment_status`, `top_senders`, `list_rules`, `read_file`, `list_files` |
| write | `create_folder`, `delete_folder`, `label_message`, `label_bulk`, `empty_trash`, `move_message`, `copy_message`, `delete_message`, `set_flags`, `append_message`, `move_bulk`, `flag_bulk`, `purge_sender`, `create_rule`, `delete_rule`, `run_rules`, `resolve_anomaly`, `dismiss_reply`, `export_message`, `get_attachments` with `part` (download), `write_file`, `delete_file` |
| send | `send_message` |
| admin | `sync_account`, `trigger_enrichment`, `cache_sweep` |

Over HTTP with auth on, `tools/list` shows only the tools the token can call. A
tool missing from the scope table is denied. Some tools are still stubs; see
[known-limitations.md](known-limitations.md).

## Generating and storing tokens

Generate each token with a cryptographic random source. 32 bytes as hex gives
64 characters:

```bash
openssl rand -hex 32
```

Use a different token per client. That way the logs tell you which client made
a request, and you can revoke one client by removing its entry.

Keep the value out of the config file. There are two ways.

### Option A: datawatch secrets

Store the token in datawatch, scoped so imap-mcp can read it:

```bash
datawatch secrets set imap_mcp_token_claude "$(openssl rand -hex 32)" --scope service:imap-mcp
```

The command substitution keeps the value off your screen. It is briefly visible
in the process list while the command runs.

Reference it:

```yaml
server:
  auth:
    tokens:
      - name: claude
        token: ${secret:imap_mcp_token_claude}
        scopes: [read, write]
```

This needs the `datawatch:` block described in the
[next section](#the-datawatch-service-token). Each client still needs the same
value. Fetch it where the client runs (see
[headersHelper](#claude-code-fetch-the-token-at-connect-time-with-headershelper)).

### Option B: environment variables

Put the token in a 0600 env file outside any repository and reference it as
`${NAME}`:

```bash
umask 077
printf 'IMAP_MCP_TOKEN_CLAUDE=%s\n' "$(openssl rand -hex 32)" >> ~/.config/imap-mcp/imap-mcp.env
```

```yaml
token: ${IMAP_MCP_TOKEN_CLAUDE}
```

`${NAME}` references are expanded from the environment when the config is
loaded. For a systemd service, load the file with `EnvironmentFile=` (see
[deployment.md](deployment.md#systemd-user-service)).

### Rotating a token

1. Add a second entry with a new name and a new value (values must be unique).
2. Restart `serve`, switch the client to the new token, and confirm it works.
3. Remove the old entry and restart again.

The server reads tokens only at startup.

## The datawatch service token

`${secret:NAME}` references are resolved against datawatch's external-service
endpoint, `GET {api_url}/api/external/secrets/{NAME}` (datawatch v8.75.0 or
later). imap-mcp authenticates with its own **service token**, which can read
only secrets scoped `service:imap-mcp`.

1. Mint the service token yourself, in a terminal (not through an agent or a
   chat). It is shown once:

   ```bash
   datawatch secrets mint-service-token imap-mcp
   ```

2. Put it in the 0600 env file:

   ```text
   DATAWATCH_API_URL=https://127.0.0.1:8443
   IMAP_MCP_DATAWATCH_TOKEN=<service token>
   ```

3. Add the `datawatch:` block:

   ```yaml
   datawatch:
     api_url: ${DATAWATCH_API_URL}
     token: ${IMAP_MCP_DATAWATCH_TOKEN}
     ca_file: ~/.datawatch/tls/server/cert.pem
   ```

| Field | Meaning |
|-------|---------|
| `api_url` | Base URL of the datawatch API. |
| `token` | imap-mcp's service token, sent as `Authorization: Bearer`. Use an `${ENV}` reference; `${secret:}` is not resolved here. |
| `ca_file` | Optional PEM certificate trusted in addition to the system roots, for datawatch's self-signed certificate. TLS verification is never disabled. A missing or invalid file stops startup. |

Every secret imap-mcp reads must be set with `--scope service:imap-mcp`.
Secrets are fetched once per name per process.

| datawatch answer | imap-mcp error | Fix |
|------------------|----------------|-----|
| 401 | `datawatch rejected the service token (401); mint one with ...` | Mint a new service token and update the env file. |
| 403 or 404 | `not found or not scoped to this service (...); set it with --scope service:imap-mcp` | Create the secret, or re-set it with the right scope. |
| unreachable | `datawatch unreachable at <url>: ...` | Start datawatch, or order the imap-mcp service after it. |
| TLS error | `x509: certificate signed by unknown authority` | Set `ca_file`. |

More on datawatch: [datawatch-integration.md](datawatch-integration.md).

## Connecting clients

All examples assume the default bind, `http://127.0.0.1:8765`.

### curl

```bash
TOKEN=<token>
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8765/api/accounts

# Unsafe methods on /api also need a JSON content type.
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{}' http://127.0.0.1:8765/api/enrichment/trigger

# The event stream needs read.
curl -sN -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8765/api/events
```

The server also rejects browser-style requests before checking the token. The
`Host` header must be a loopback name or the configured `server.host`. Any
`Origin` header must also name an allowed host. `POST`, `PUT` and `DELETE` on
`/api/` must send `Content-Type: application/json`. curl, datawatch and Claude
Code meet these rules by default.

### Claude Code: static header (user scope)

Register the server at **user scope**, with the header:

```bash
claude mcp add --transport http --scope user imap-mcp http://127.0.0.1:8765/mcp \
  --header "Authorization: Bearer <token>"
```

The resulting entry has this shape:

```json
{
  "mcpServers": {
    "imap-mcp": {
      "type": "http",
      "url": "http://127.0.0.1:8765/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

This stores the token in your user-level Claude Code config. That is simple,
but the value sits in a file in plain text. The next option avoids that.

### Claude Code: fetch the token at connect time with `headersHelper`

`headersHelper` is a command Claude Code runs on each connection, at session
start and on every reconnect. It must print a JSON object of header names and
values on stdout and finish within 10 seconds. The result is not cached. On a
401 or 403, Claude Code runs the helper again and retries once, so a rotated
token is picked up without restarting the session.

Example helper, `~/.config/imap-mcp/mcp-headers.sh`:

```sh
#!/bin/sh
# Print the Authorization header for imap-mcp as JSON.
# Reads the token from a 0600 file. To use a secret manager instead, replace
# the `token=` line with a command that prints the token on stdout.
set -eu

token_file="${IMAP_MCP_TOKEN_FILE:-$HOME/.config/imap-mcp/claude.token}"
token=$(tr -d '\r\n' < "$token_file")

# Refuse anything that is not a plain token, so the JSON below stays valid.
case "$token" in
  ''|*[!A-Za-z0-9._~+/=-]*) echo "mcp-headers: bad or empty token in $token_file" >&2; exit 1 ;;
esac

printf '{"Authorization":"Bearer %s"}\n' "$token"
```

```bash
chmod 700 ~/.config/imap-mcp/mcp-headers.sh
umask 077; openssl rand -hex 32 > ~/.config/imap-mcp/claude.token   # or copy the existing value
~/.config/imap-mcp/mcp-headers.sh >/dev/null && echo ok             # test without printing the token
```

Register it at user scope:

```bash
claude mcp add-json --scope user imap-mcp \
  '{"type":"http","url":"http://127.0.0.1:8765/mcp","headersHelper":"~/.config/imap-mcp/mcp-headers.sh"}'
```

If your Claude Code version does not accept `headersHelper` through
`add-json`, add the field to the user-scope entry with `/config`, or edit the
entry directly. Check the result with `claude mcp get imap-mcp`.

The helper's environment includes `CLAUDE_CODE_MCP_SERVER_NAME` and
`CLAUDE_CODE_MCP_SERVER_URL`, so one script can serve several servers.

### Why user scope, not a project `.mcp.json`

- **The trust gate.** For servers declared in a project `.mcp.json` or at local
  scope, Claude Code runs `headersHelper` only after you accept the trust dialog
  for that project directory. Until then it connects with static headers only,
  so the connection gets a 401. Non-interactive runs (`claude -p`, the SDK)
  print `headersHelper not run` and also connect without the token. A user-scope
  server's helper runs in every session.
- **No secrets in repositories.** A `.mcp.json` is usually committed. A static
  token in it would end up in git history.
- **Project servers need approval.** Interactive sessions ask before using
  servers from a project `.mcp.json`.

### The datawatch messaging backend

datawatch's `imap_mcp` messaging backend reads `GET /api/events` and sends
replies through `POST /api/accounts/{account}/messages/send`. Give it its own
token with exactly `read` and `send`:

```yaml
- name: datawatch
  token: ${secret:imap_mcp_token_datawatch}
  scopes: [read, send]
```

Configure the same value on the datawatch side, in its `imap_mcp` backend.
That is datawatch configuration; see the datawatch documentation for the
field. If the backend has no token, or a wrong one, its requests get 401 and the
imap-mcp log shows `auth: rejected request` with `path=/api/events`.

## 401 vs 403

| Status | When | Body |
|--------|------|------|
| 401 | No `Authorization: Bearer` header, a different scheme, or a token that matches no configured token. Applies to every path except `/api/health`, including unknown paths. | `{"error":"unauthorized: valid bearer token required"}` plus `WWW-Authenticate: Bearer realm="imap-mcp"` |
| 403 (REST) | The token is valid but lacks the route's scope. | `{"error":"forbidden: token lacks scope write"}` |
| 403 (guard) | Browser protections: bad `Host` or `Origin`. Checked before the token. | `forbidden: host not allowed` or `forbidden: cross-origin request` (plain text) |
| 415 (guard) | Unsafe `/api/` method without `Content-Type: application/json`. Checked before the token. | `unsupported media type: Content-Type must be application/json` |

MCP calls behave differently from REST once the token is valid. The `/mcp`
transport accepts any valid token. Tools the token cannot call are hidden from
`tools/list`. Calling one anyway returns a tool error result, not HTTP 403:

```text
forbidden: tool "send_message" requires scope "send"
```

Log lines (token values are never logged):

```text
level=WARN msg="auth: rejected request" path=/mcp method=POST remote=127.0.0.1:50412 reason="missing bearer token"
level=WARN msg="auth: scope denied" token=datawatch path=/api/rules required=write
```

`reason` is either `missing bearer token` or `invalid token`. Successful
requests are logged at debug level with the token name.

## Disabling auth (insecure)

```yaml
server:
  auth:
    disabled: true
```

or `IMAP_MCP_SERVER_AUTH_DISABLED=true` in the environment.

With auth disabled:

- `/api` and `/mcp` accept requests with no token. **Any local process**,
  including software running as other users on the same machine, can read all
  cached mail, send mail, delete messages, empty the trash, change rules and
  register webhooks.
- MCP shows and allows every tool; there is no scope filtering.
- The browser protections still apply, so web pages cannot reach the server.
  They do nothing against local programs.
- If `server.host` is not a loopback address, the server is also reachable from
  the network.
- Each startup logs
  `server.auth.disabled is set: /api and /mcp accept unauthenticated requests from any local process (insecure)`,
  and `/api/health` reports `"auth": "disabled"`.
- Configured tokens are still resolved and validated. A bad reference still
  stops startup.

Use it only on a single-user machine for short-lived testing. Check the state
at any time:

```bash
curl -s http://127.0.0.1:8765/api/health | jq .auth    # "enabled" or "disabled"
```

## Troubleshooting

### A client loops on OAuth discovery after a 401

**Symptom:** the log fills with `auth: rejected request` warnings, often for
paths such as `/.well-known/oauth-protected-resource` or
`/.well-known/oauth-authorization-server`, around one per second per client.
In Claude Code, `/mcp` shows the server as needing authentication.

**Cause:** an MCP client connected **without** an `Authorization` header gets a
401 with `WWW-Authenticate: Bearer`. MCP clients treat that as a request to run
OAuth and probe the well-known discovery paths. imap-mcp has no OAuth server,
so every probe is another 401, and the client keeps retrying. This usually
means a session started before the header or `headersHelper` was configured, or
a project-scope helper that did not run because the project was not trusted.

**Fix:**

1. Configure the header or `headersHelper` at user scope (see above).
2. Restart the affected client sessions, or reconnect from `/mcp`. A running
   session does not pick up a changed config.
3. Do not choose "Authenticate" in `/mcp`. There is no OAuth flow to complete.

Once the client sends a header, a 401 is reported as a failed connection rather
than an OAuth prompt, which points straight at a wrong token.

### Other problems

| Symptom | Cause | Fix |
|---------|-------|-----|
| `serve` exits: `server auth: server.auth: no tokens configured ...` | No tokens and not disabled. | Add a token. |
| `token reference is unresolved (environment variable unset?)` | The `${ENV}` variable is not in the server's environment. | Load the env file (`EnvironmentFile=` for systemd). |
| `token must be at least 32 characters` | Short token. | Generate one with `openssl rand -hex 32`. |
| `token "b" reuses the value of token "a"` | Two entries share a value. | Give each client its own token. |
| `${secret:...} needs a datawatch block with api_url and token` | `${secret:}` reference without datawatch config. | Add the `datawatch:` block, or use `${ENV}`. |
| curl gets 401 with a token that looks right | Extra whitespace or newline in the value, or the server was not restarted after the change. | Re-copy the value; restart `serve`. |
| REST call gets 403 `token lacks scope` | Missing scope. | Add the scope to that token and restart, or use a different token. |
| A tool is missing from the client's tool list | The token lacks its scope. | As above. |
| `forbidden: host not allowed` | Request used a hostname other than loopback or `server.host`. | Use `127.0.0.1` or `localhost`. |
