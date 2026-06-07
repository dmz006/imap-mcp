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
- For datawatch layers: a running datawatch daemon (v8.0.0+).

## Wiring imap-mcp into Claude Code (operator-controlled)

You attach imap-mcp yourself; datawatch never injects it. Add it to `~/.mcp.json`
(global) or a project `.mcp.json`. datawatch preserves non-datawatch entries on
session spawn, so it persists.

stdio (reconnects per session):
```json
{ "mcpServers": { "imap-mcp": { "command": "/home/dmz/workspace/imap-mcp/imap-mcp" } } }
```

HTTP (persistent connections — start `imap-mcp serve` first):
```json
{ "mcpServers": { "imap-mcp": { "url": "http://localhost:8765/mcp" } } }
```

A session that wasn't given imap-mcp simply doesn't have it. That is the design.

---

## Layer 1 — Secrets

Resolve credentials from datawatch's secrets service instead of env vars.

**Credential resolution order** (per field): `${ENV_VAR}` → `${secret:name}` →
plain value.

1. Store the secret in datawatch (operator):
   ```
   datawatch secrets set gmail_app_password
   ```
2. Reference it in the imap-mcp config and add a `datawatch:` block:
   ```yaml
   accounts:
     - name: gmail
       auth:
         type: plain
         username: you@gmail.com
         password: ${secret:gmail_app_password}

   datawatch:
     api_url: ${DATAWATCH_API_URL}        # e.g. http://localhost:7777
     token: ${DATAWATCH_SECRETS_TOKEN}    # agent-scoped token (least privilege)
   ```
3. Export the two refs and start imap-mcp:
   ```
   export DATAWATCH_API_URL=http://localhost:7777
   export DATAWATCH_SECRETS_TOKEN=<datawatch agent-scoped secrets token>
   ./imap-mcp serve --config config.yaml
   ```

**How it resolves:** imap-mcp calls `GET {api_url}/api/agents/secrets/{name}`
with the bearer token at startup, once per secret (cached).

**Failure modes (by design):**
- `${secret:...}` used but no `datawatch:` block → startup error (no silent
  placeholder).
- datawatch unreachable / secret missing → startup error naming the secret.

**Security:** `api_url`/`token` must be `${ENV_VAR}` references — never write a
literal token into a config file or repo.

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
  - name: dmzs
    imap: { host: mail.dmzs.com, port: 993, tls: true }
    auth: { type: plain, username: me@dmzs.com, password: ${secret:dmzs_pw} }
    smtp:
      host: mail.dmzs.com
      port: 587          # 587=STARTTLS, 465=implicit TLS
      starttls: true
      from: "Me <me@dmzs.com>"
      # username/password default to the auth block above
```
Drive it with the `send_message` MCP tool:
```json
{ "account": "dmzs", "to": "a@b.com", "subject": "hi", "body": "..." }
```

### 3b. Inbound command channel (default-deny, composable gates)

Turn an account into a control channel. A command is acted on **only if every
required gate passes**. Because `From:` is spoofable, gates are layered:

```yaml
    inbound:
      enabled: true
      watch_folder: INBOX
      gates:                       # ALL configured gates required (AND)
        allowlist: [ops@dmzs.com]  # addresses or bare domains
        require_dkim: true         # dkim=pass in Authentication-Results
        require_dmarc: true        # dmarc=pass
        hmac_secret: ${secret:dmzs_inbound_hmac}
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

### 3c. The datawatch side (not yet built)

For datawatch to *act on* inbound commands and *send* through imap-mcp, it needs
a `messaging.Backend` that consumes only `inbound.command` events and sends via
`send_message`. That is **datawatch-side development**, tracked at
[datawatch#127](https://github.com/dmz006/datawatch/issues/127). imap-mcp gates
*authenticity*; datawatch decides *what a verified command may do* (its own
capability scoping + human-in-the-loop). Until that backend lands, the inbound
channel verifies and emits events, but no datawatch comm consumes them yet.

---

## Quick reference: which layer needs what

| You want… | Configure | datawatch piece |
|-----------|-----------|-----------------|
| Creds from datawatch vault | `datawatch:` block + `${secret:}` | secrets service running |
| An agent that knows the workflows | nothing in imap-mcp | `skills_registry_sync community imap-mcp` |
| Send mail | per-account `smtp:` | none |
| Trust-gated inbound commands | per-account `inbound:` + gates | backend (datawatch#127) to consume events |

## Verifying it works

```bash
./imap-mcp serve --config config.yaml &
curl -s localhost:8765/api/health           # {"status":"ok",...}
curl -s localhost:8765/api/accounts          # connection status per account
# MCP: initialize → tools/list should include send_message when smtp is configured
```

See the top-level `README.md` for the full tool list and `config.example.yaml`
for every config field with inline docs.
