# imap-mcp documentation

Start with the project [README](../README.md) for an overview and quick start.
Then see **[examples.md](examples.md)** for runnable recipes that show what the
features are for.

## Setup and operations

| Document | What it covers |
|----------|----------------|
| [auth-tokens.md](auth-tokens.md) | Bearer tokens and scopes for `serve`: creating and storing tokens, the route and tool scope table, connecting curl and Claude Code (including `headersHelper`), the datawatch service token, and the insecure opt-out. |
| [encryption.md](encryption.md) | The `imap.db` / `cache.db` split, optional at-rest encryption, key references, `imap-mcp db encrypt`, backups and lost keys. |
| [deployment.md](deployment.md) | systemd user service, scheduling `run-rules`, logs, clean stop, upgrading and rolling back. |
| [sync-cache.md](sync-cache.md) | The mail cache: sync window, folders and SPECIAL-USE tokens, overrides, CONDSTORE and UIDVALIDITY, `keep_flagged`, cleaning, VACUUM and `cache_sweep`. |
| [enrichment.md](enrichment.md) | Background embeddings and classification: providers, queue lanes, rate limits, yield gates, backoff, `enrichment_status` and triggers. |
| [known-limitations.md](known-limitations.md) | What is not implemented yet, what you will see, and workarounds. |

## Using the API

| Document | What it covers |
|----------|----------------|
| [rest-api.md](rest-api.md) | Every REST route with method, scope, parameters and responses; `/api/health` fields; the `/api/events` SSE stream and event types. |
| [rules.md](rules.md) | The rules engine: conditions and actions, MCP tools and REST routes, the `run-rules` CLI, dry runs, scheduling and `rule.fired`. |
| [query.md](query.md) | The `/api/query` JSON query DSL over the cache views. |
| [webhooks.md](webhooks.md) | Webhook registration, signed delivery, the durable outbox, retries and event payloads. |
| [cookbook-inbox-cleanup.md](cookbook-inbox-cleanup.md) | A worked example: clean up a large inbox with the MCP tools, then keep it clean with a scheduled job. |

## Integrations and accounts

| Document | What it covers |
|----------|----------------|
| [datawatch-integration.md](datawatch-integration.md) | Optional datawatch integration: secrets, the companion skill, and the inbound command channel and messaging backend. |
| [enterprise-gmail-oauth.md](enterprise-gmail-oauth.md) | OAuth setup for Google Workspace accounts, including service-account (domain-wide delegation) auth. |

## Design and reference

| Document | What it covers |
|----------|----------------|
| [architecture/overview.md](architecture/overview.md) | Components and data flow. |
| [plans/README.md](plans/README.md) | Active plans, fixed bugs and the backlog. |
| [../IMAP-MCP-CONTEXT.md](../IMAP-MCP-CONTEXT.md) | Developer and agent context: tools, data model and conventions for working on the code. |
| [../skills/imap-mcp/SKILL.md](../skills/imap-mcp/SKILL.md) | Usage skill for agents working with the imap-mcp tools. |
| [../config.example.yaml](../config.example.yaml) | Annotated example configuration with every setting. |
| [../.env.example](../.env.example) | Example environment variables and overrides. |
| [../CHANGELOG.md](../CHANGELOG.md) | Release history and upgrade notes. |
