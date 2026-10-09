# Using imap-mcp with datawatch

A complete, task-oriented guide to running imap-mcp on its own **or** integrated
with [datawatch](https://github.com/dmz006/datawatch). The integration has three
**independent, opt-in** layers — use any subset. Nothing auto-injects into a
session: the operator decides if, when, and where imap-mcp connects.

| Layer | Gives you | Needs datawatch? |
|-------|-----------|------------------|
| [1. Secrets](#layer-1--secrets) | credentials resolved from datawatch's vault | datawatch secrets service |
| [2. Skill](#layer-2--skill) | an agent that knows the safe mail workflows | datawatch skills registry |
| [3. Comm](#layer-3--comm-bidirectional-email) | send mail + a trust-gated inbound command channel | partially — see below |

> **Standalone always works.** With no `datawatch:` block and no skill/comm
> wiring, imap-mcp is a self-contained IMAP MCP server. Layers 1–3 are additive
> shells around that.

---

## Prerequisites

- A built `imap-mcp` binary (`go build -o imap-mcp ./cmd/imap-mcp/`).
- A config file (copy `config.example.yaml`). Never commit it — `*.yaml` is
  gitignored except the example.
- For datawatch layers: a running datawatch daemon, **v8.75.0 or later**. That
  is the first release with the external secrets endpoint
  (`GET /api/external/secrets/{name}`) that Layer 1 uses.
- For HTTP mode (`imap-mcp serve`): bearer tokens in `server.auth.tokens`. Auth
  is on by default; every `/api` route except `/api/health`, and every MCP tool,
  needs a token with the right scope (`read`, `write`, `send`, `admin`). See
  [auth-tokens.md](auth-tokens.md).

Related docs: [auth-tokens.md](auth-tokens.md) (token scopes and client
headers), [encryption.md](encryption.md) (database keys), and
[deployment.md](deployment.md) (systemd service, `EnvironmentFile`, scheduling
`run-rules`).

## Wiring imap-mcp into Claude Code (operator-controlled)

You attach imap-mcp yourself; datawatch never injects it. Add it to `~/.mcp.json`
(global) or a project `.mcp.json`. datawatch preserves non-datawatch entries on
session spawn, so it persists.

stdio (reconnects per session):
```json
{ "mcpServers": { "imap-mcp": { "command": "/path/to/imap-mcp/imap-mcp" } } }
```

HTTP (persistent connections — start `imap-mcp serve` first). The client must
send a bearer token; `tools/list` shows only the tools that token's scopes
allow:
```json
{ "mcpServers": { "imap-mcp": {
    "type": "http",
    "url": "http://localhost:8765/mcp",
    "headers": { "Authorization": "Bearer ${IMAP_MCP_TOKEN}" } } } }
```
See [auth-tokens.md](auth-tokens.md) for other ways to supply the header
(including Claude Code's `headersHelper`).

A session that wasn't given imap-mcp simply doesn't have it. That is the design.

---

## Layer 1 — Secrets

Resolve credentials from datawatch's secrets service instead of env vars.

**Credential resolution order** (per field): `${ENV_VAR}` → `${secret:name}` →
plain value.

1. Store the secret in datawatch (operator):
   ```
   datawatch secrets set gmail_app_password --scope service:imap-mcp
   ```
   Every secret imap-mcp reads must carry `--scope service:imap-mcp`; it cannot
   read anything else.
2. Reference it in the imap-mcp config and add a `datawatch:` block:
   ```yaml
   accounts:
     - name: gmail
       auth:
         type: plain
         username: you@gmail.com
         password: ${secret:gmail_app_password}

   datawatch:
     api_url: ${DATAWATCH_API_URL}        # e.g. https://127.0.0.1:8443
     token: ${IMAP_MCP_DATAWATCH_TOKEN}   # imap-mcp service token (least privilege)
     ca_file: ~/.datawatch/tls/server/cert.pem  # pin datawatch's self-signed cert
   ```
3. Mint imap-mcp's service token **yourself, in a real terminal** (never via an
   agent or a chat passthrough; the token is shown once):
   ```
   datawatch secrets mint-service-token imap-mcp
   ```
4. Export the two refs (for a systemd service, put them in an `EnvironmentFile`
   with mode 0600, outside any repo) and start imap-mcp:
   ```
   export DATAWATCH_API_URL=https://127.0.0.1:8443
   export IMAP_MCP_DATAWATCH_TOKEN=<the service token>
   ./imap-mcp serve --config config.yaml
   ```

**How it resolves:** imap-mcp calls datawatch's external-service endpoint
`GET {api_url}/api/external/secrets/{name}` (datawatch v8.75.0 or later) with
the service token, once per secret per process (cached). `serve` and
`run-rules` use the same path (AGENT.md D15).

**Failure modes (by design):**
- `${secret:...}` used but no `datawatch:` block → startup error (no silent
  placeholder).
- datawatch unreachable / secret missing → startup error naming the secret.
- 401 → the service token is wrong or revoked: mint a new one.
- `x509: certificate signed by unknown authority` → set `ca_file` to datawatch's
  certificate (`~/.datawatch/tls/server/cert.pem`). Verification is never
  skipped (AGENT.md D15a); an unreadable `ca_file` fails startup.
- 403/404 → the secret does not exist or is not scoped `service:imap-mcp`.

**Security:** keep `api_url`/`token` as `${ENV_VAR}` references — never write a
literal token into a config file or repo. (imap-mcp does not enforce this; it
is on you.)

### The `datawatch:` block

| Field | Meaning |
|-------|---------|
| `api_url` | Base URL of the datawatch HTTP API, e.g. `https://127.0.0.1:8443`. |
| `token` | imap-mcp's datawatch service token, sent as `Authorization: Bearer`. Use an `${ENV}` reference. |
| `ca_file` | Optional PEM certificate to trust in addition to the system roots, e.g. datawatch's self-signed `~/.datawatch/tls/server/cert.pem`. `~` is expanded. |

- The block is optional. Without it, imap-mcp runs standalone and any
  `${secret:}` reference is a startup error.
- `ca_file` applies to every call imap-mcp makes to datawatch (secrets,
  capacity gate, LLM proxy). TLS verification is never turned off. A missing
  or unreadable file, or one with no certificate, fails startup.
- `${secret:}` resolution needs both `api_url` and `token`.

### `${secret:}` for database keys and server tokens

The same `${secret:name}` references work in two more places. Each is resolved
only by the command that needs it:

| Field | Resolved by | Notes |
|-------|-------------|-------|
| `db.encryption_key` | commands that open `imap.db` (`serve`, `run-rules`, …) | key for the state database |
| `db.cache.encryption_key` | commands that open `cache.db` | `run-rules` never opens the cache, so never needs this key |
| `server.auth.tokens[].token` | `serve` only | stdio and `run-rules` never resolve server tokens |

```yaml
db:
  encryption_key: ${secret:imap_mcp_state_key}
  cache:
    encryption_key: ${secret:imap_mcp_cache_key}

server:
  auth:
    tokens:
      - name: datawatch
        token: ${secret:imap_mcp_token_datawatch}
        scopes: [read, send]
```

Resolution fails closed: an unreachable datawatch, a missing secret, an unset
`${ENV}` or an empty value stops startup. A key is never generated for you.
Secrets need `--scope service:imap-mcp` like any other. A scheduled `run-rules`
job also needs `DATAWATCH_API_URL` and the service token in its environment
when its config uses `${secret:}`; [deployment.md](deployment.md) shows a
wrapper for that. Database encryption itself is covered in
[encryption.md](encryption.md).

### Enrichment features that call datawatch (not usable yet)

Two enrichment settings also call datawatch, with the same `datawatch.token`:

- **LLM classify provider:** `enrichment.classify.provider: datawatch` with
  `datawatch_llm: <name>` sends classify prompts to
  `POST {api_url}/api/proxy/llm/<name>`.
- **Capacity gate:** `enrichment.yield.datawatch_pools` makes backfill pause
  while those datawatch pools are full, read from `GET {api_url}/api/capacity`.

Datawatch accepts the imap-mcp service token **only** on the secrets endpoint.
The LLM proxy and the capacity endpoint need a datawatch federation-peer token,
and imap-mcp has no separate config field for one yet. Leave both off for now:
the classify provider will fail its calls, and the capacity gate cannot read
capacity, so it never pauses (it fails open). See
[known-limitations.md](known-limitations.md).

---

## Layer 2 — Skill

Give a datawatch agent the *workflows* for these tools (triage, unsubscribe,
sender audit, bulk-archive, search, export). The skill is **instructions only**
— it bundles no tools and opens no connection. Loading it ≠ connecting.

It is published to the community registry at `skills/comms/imap-mcp`
([dmz006/datawatch-community](https://github.com/dmz006/datawatch-community)).

Pull it on demand (operator or session) — **pull-based, never auto-loaded:**
```
# datawatch MCP:
skills_registry_sync   { name: "community", skills: "imap-mcp" }
# or CLI:
datawatch skills registry sync community imap-mcp
```

Then a session can `skill_load imap-mcp` to read it. The skill's first
instruction is to confirm the imap-mcp MCP server is attached and stop if not —
it never tries to connect anything itself.

Source of truth for the skill lives in this repo at `skills/imap-mcp/SKILL.md`.

---

## Layer 3 — Comm (bidirectional email)

Make email a first-class channel: imap-mcp **sends** and exposes a **trust-gated
inbound command channel**. imap-mcp is the trust boundary; it emits a verified
event only when a message passes every required gate.

### 3a. Outbound (per-domain SMTP)

Each account sends through its own server, not a shared relay:
```yaml
accounts:
  - name: work
    imap: { host: mail.example.com, port: 993, tls: true }
    auth: { type: plain, username: me@example.com, password: ${secret:work_pw} }
    smtp:
      host: mail.example.com
      port: 587          # 587=STARTTLS, 465=implicit TLS
      starttls: true
      from: "Me <me@example.com>"
      # username/password default to the auth block above
```
Drive it with the `send_message` MCP tool. Over HTTP, `send_message` appears in
`tools/list` and can be called only with a token that has the `send` scope
(stdio has no tokens). SMTP auth is PLAIN with a password only; an `xoauth2`
account cannot send unless its `smtp:` block has a username and password.
```json
{ "account": "work", "to": "a@b.com", "subject": "hi", "body": "..." }
```

### 3b. Inbound command channel (default-deny, composable gates)

Turn an account into a control channel. A command is acted on **only if every
required gate passes**. Because `From:` is spoofable, gates are layered:

```yaml
    inbound:
      enabled: true
      watch_folder: INBOX
      gates:                       # ALL configured gates required (AND)
        allowlist: [ops@example.com]  # addresses or bare domains
        require_dkim: true         # dkim=pass in Authentication-Results
        require_dmarc: true        # dmarc=pass
        hmac_secret: ${secret:work_inbound_hmac}
        replay_window_minutes: 10  # reject stale; nonces are single-use
        require_pgp: false         # BACKLOG — true fails closed until implemented
      capabilities: [status, mail.archive]   # allowed verbs (default-deny)
```
> If `inbound.enabled: true`, you **must** configure at least one gate or
> startup refuses — there is no ungated command channel.

**Command envelope** — a fenced block in the message body:
```
-----BEGIN DATAWATCH COMMAND-----
verb: mail.archive
args: {"from":"noise@example.com"}
nonce: 7f3c1a9e
ts: 2026-06-07T20:00:00Z
hmac: <hex hmac-sha256 over the canonical envelope>
-----END DATAWATCH COMMAND-----
```
The HMAC (when `hmac_secret` is set) is computed over the canonical form
`verb:<v>\nargs:<a>\nnonce:<n>\nts:<rfc3339>\n` — see
`trust.CanonicalEnvelope` for the exact bytes a signer must reproduce.

**Gate reference:**

| Gate | Proves |
|------|--------|
| `allowlist` | sender is expected (coarse; spoofable alone) |
| `require_dkim` / `require_dmarc` | receiving server validated domain auth |
| `hmac_secret` | envelope carries a valid shared-secret HMAC |
| `replay_window_minutes` + nonce | command is fresh and single-use |
| `require_pgp` | **backlog** — PGP-signed envelope; **fails closed** for now |

**What imap-mcp emits** (on its event bus):
- `inbound.command` — a fully verified `VerifiedCommand`. **Only this is actionable.**
- `inbound.rejected` — a command attempt that failed a gate. Audit only.

Ordinary mail (no envelope) is never touched and emits nothing.

### 3c. The datawatch side (built — loop is closed)

datawatch ships an `imap_mcp` messaging backend
(`internal/messaging/backends/imapmcp`) that consumes only verified
`inbound.command` events and sends replies through imap-mcp. imap-mcp gates
*authenticity*; datawatch decides *what a verified command may do* (its own
capability scoping + human-in-the-loop). Tracked at
[datawatch#127](https://github.com/dmz006/datawatch/issues/127).

**Wire it up (datawatch config.yaml):**
```yaml
imap_mcp:
  enabled: true
  url: "http://localhost:8765"   # imap-mcp HTTP server (run: imap-mcp serve)
  account: ""                    # empty = imap-mcp default account
  subject_prefix: "datawatch"    # prepended to reply subjects
  # plus the bearer token for imap-mcp; see datawatch's imap_mcp backend docs
  # for the field name
```

**Bearer token.** The backend calls imap-mcp's REST API, so it needs an
imap-mcp token. Give datawatch its own token with exactly the `read` scope (for
`GET /api/events`) and the `send` scope (for the send endpoint), and nothing
else. Keep the token value as a datawatch-held secret: datawatch passes it to
imap-mcp, and imap-mcp's config references the same secret:

```yaml
# imap-mcp config.yaml
server:
  auth:
    tokens:
      - name: datawatch
        token: ${secret:imap_mcp_token_datawatch}
        scopes: [read, send]
```

How datawatch reads that secret into its `imap_mcp:` block is defined by
datawatch; see datawatch's imap_mcp backend docs. Token generation and scopes
are covered in [auth-tokens.md](auth-tokens.md).

**The transport contract (imap-mcp v0.2.1+ serves both):**

| Direction | Endpoint | Notes |
|-----------|----------|-------|
| Receive | `GET /api/events` (SSE) | scope `read`; datawatch subscribes, acts only on `inbound.command`, reconnects with backoff |
| Send | `POST /api/accounts/{account}/messages/send` | scope `send`; `{to,subject,body,cc}`; `account` may be `_default` |

Both calls carry `Authorization: Bearer <token>`. For example:

```bash
curl -sN -H "Authorization: Bearer <token>" http://localhost:8765/api/events
curl -sS -X POST -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{"to":"ops@example.com","subject":"status","body":"ok"}' \
  http://localhost:8765/api/accounts/_default/messages/send
```

Event envelope is `{type, account, payload}`; the verified-command payload
carries `Account`, `From`, `Command{Verb,Args,Nonce}`, `Gates`.

**End-to-end flow:** trust-gated email arrives → imap-mcp runs the gates →
emits `inbound.command` over SSE → datawatch's backend surfaces it as an inbound
message, applies its own dispatch/scoping → replies via the send endpoint, which
goes out through the account's SMTP. Both halves are now released
(imap-mcp ≥ v0.2.1, datawatch `imap_mcp` backend).

> Run `imap-mcp serve` (HTTP mode) for this — the SSE stream needs a persistent
> server. The operator still chooses to enable the `imap_mcp:` channel; nothing
> auto-connects.

---

## Quick reference: which layer needs what

| You want… | Configure | datawatch piece |
|-----------|-----------|-----------------|
| Creds from datawatch vault | `datawatch:` block + `${secret:}` | secrets service (datawatch v8.75.0+) |
| An agent that knows the workflows | nothing in imap-mcp | `skills_registry_sync community imap-mcp` |
| Send mail | per-account `smtp:` | none |
| Trust-gated inbound commands | per-account `inbound:` + gates; a `read`+`send` token for datawatch | datawatch `imap_mcp:` backend (built, datawatch#127) |

## Verifying it works

```bash
./imap-mcp serve --config config.yaml &
curl -s localhost:8765/api/health           # {"status":"ok",...} — the one open route
curl -s -H "Authorization: Bearer <token>" localhost:8765/api/accounts   # needs read
# MCP: initialize → tools/list lists send_message only for a token with the send scope
```

See the top-level `README.md` for the full tool list and `config.example.yaml`
for every config field with inline docs.
