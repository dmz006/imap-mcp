# Storage and encryption

imap-mcp keeps its data in two SQLite files. Each one can be encrypted at rest
on its own, with its own key. This page covers what lives where, how to turn
encryption on for a new or existing install, and what to do about backups and
lost keys.

- [The two database files](#the-two-database-files)
- [How encryption works](#how-encryption-works)
- [Keys](#keys)
- [Enable encryption on a new install](#enable-encryption-on-a-new-install)
- [Encrypt an existing install: `imap-mcp db encrypt`](#encrypt-an-existing-install-imap-mcp-db-encrypt)
- [Fail-closed behaviour](#fail-closed-behaviour)
- [run-rules needs only the state key](#run-rules-needs-only-the-state-key)
- [Confirm encryption is on](#confirm-encryption-is-on)
- [Backups](#backups)
- [Deleting plaintext copies](#deleting-plaintext-copies)
- [If a key is lost](#if-a-key-is-lost)

## The two database files

| File | Config key | Default path | Contents | Rebuildable? |
|------|------------|--------------|----------|--------------|
| State DB | `db.path` | `~/.local/share/imap-mcp/imap.db` | Rules, webhooks (including each webhook's signing secret), the webhook delivery outbox (`webhook_deliveries`), inbound command replay nonces | **No.** Back it up. |
| Cache DB | `db.cache.path` | `cache.db` next to `db.path` | Cached messages, the FTS5 full-text index, embedding vectors, sender profiles, knowledge graph, anomalies, per-folder sync state, the enrichment queue | **Yes.** Everything comes back from IMAP on the next sync. Enrichment runs again. |

```yaml
db:
  path: ~/.local/share/imap-mcp/imap.db
  # encryption_key: ${secret:imap_mcp_state_key}
  cache:
    path: ~/.local/share/imap-mcp/cache.db   # default: cache.db next to path
    # encryption_key: ${secret:imap_mcp_cache_key}
```

Environment overrides: `IMAP_MCP_DB_PATH`, `IMAP_MCP_DB_CACHE_PATH`,
`IMAP_MCP_DB_ENCRYPTION_KEY`, `IMAP_MCP_DB_CACHE_ENCRYPTION_KEY`.

Both files, and their `-wal`/`-shm` sidecars, are created with mode 0600. The
directory is created with mode 0750 if it does not exist.

Installs older than 0.6.0 used a single `imap.db`. The first start of 0.6.0 or
later splits it automatically; see
[Upgrading in deployment.md](deployment.md#upgrading).

## How encryption works

- Encryption is whole-file, using the `adiantum` VFS from the pure-Go
  `ncruces/go-sqlite3` driver. The tables, the full-text index, the vectors and
  the WAL are all encrypted.
- The configured key is a passphrase. The real encryption key is derived from it
  with Argon2id.
- SQLite temporary data is kept in memory for encrypted files, so it is never
  written to a temp file.
- Each file is independent. You can encrypt only the state DB, only the cache
  DB, or both. Both can use the same secret, but separate keys let you rotate or
  lose one without affecting the other.
- imap-mcp never generates a key and never converts a file on its own.

## Keys

`db.encryption_key` (state) and `db.cache.encryption_key` (cache) take one of:

| Form | Example | Resolved from |
|------|---------|---------------|
| datawatch secret | `${secret:imap_mcp_state_key}` | datawatch's secrets service. Needs a `datawatch:` block with `api_url` and `token`. See [auth-tokens.md](auth-tokens.md#the-datawatch-service-token). |
| Environment variable | `${IMAP_MCP_STATE_KEY}` | The process environment, when the config is loaded. |

A literal passphrase in the YAML also works, but then the key sits next to
the data it protects. Use a reference.

Generate a key with enough entropy, for example 32 random bytes as hex:

```bash
openssl rand -hex 32
```

Store it in datawatch without echoing it to the terminal:

```bash
datawatch secrets set imap_mcp_state_key "$(openssl rand -hex 32)" --scope service:imap-mcp
datawatch secrets set imap_mcp_cache_key "$(openssl rand -hex 32)" --scope service:imap-mcp
```

The `--scope service:imap-mcp` part is required: imap-mcp's service token can
read only secrets with that scope.

Or put it in a 0600 environment file outside any repository (see
[deployment.md](deployment.md#the-environment-file)):

```bash
umask 077
printf 'IMAP_MCP_STATE_KEY=%s\n' "$(openssl rand -hex 32)" >> ~/.config/imap-mcp/imap-mcp.env
```

Keys are resolved only by the commands that open the file. `serve` and stdio
mode resolve both keys. `run-rules` resolves only the state key. `db encrypt`
resolves the keys for the files it was asked to convert.

## Enable encryption on a new install

If the database files do not exist yet, set the keys **before the first
start**. The files are then created encrypted.

1. Create the keys (see [Keys](#keys)).
2. Add them to the config:

   ```yaml
   db:
     path: ~/.local/share/imap-mcp/imap.db
     encryption_key: ${secret:imap_mcp_state_key}
     cache:
       encryption_key: ${secret:imap_mcp_cache_key}

   datawatch:
     api_url: ${DATAWATCH_API_URL}
     token: ${IMAP_MCP_DATAWATCH_TOKEN}
     ca_file: ~/.datawatch/tls/server/cert.pem
   ```

3. Start the server and check `/api/health` (see
   [Confirm encryption is on](#confirm-encryption-is-on)).

## Encrypt an existing install: `imap-mcp db encrypt`

If the files already exist as plaintext, adding a key is not enough: the server
refuses to open a plaintext file when a key is configured. Convert the files
explicitly:

```text
imap-mcp db encrypt [--only state|cache] [--config PATH]
```

| Flag | Meaning |
|------|---------|
| `--only state` | Convert only `db.path`. |
| `--only cache` | Convert only `db.cache.path`. |
| (none) | Convert both. |
| `--config PATH` | Config file. Default: `./config.yaml` if present, else `~/.config/imap-mcp/config.yaml`. |

### Steps

1. Back up the state DB (see [Backups](#backups)).
2. Create the key(s) and add `encryption_key` to the config.
3. Stop everything that has the files open: the service, and any scheduled
   `run-rules` that might start during the conversion.

   ```bash
   systemctl --user stop imap-mcp
   ```

4. Run the conversion:

   ```bash
   imap-mcp db encrypt --config ~/.config/imap-mcp/config.yaml
   ```

5. Start the service again and confirm with `/api/health`.
6. Delete the plaintext copies it lists once you no longer need them (see
   [Deleting plaintext copies](#deleting-plaintext-copies)).

### What it does, per file

1. **Checks the key.** If that file's key is not set, it prints
   `<name>: <key field> not set; leaving <path> as is` and moves on.
2. **Checks the file.**
   - Does not exist: prints `... does not exist yet; it will be created encrypted`.
     The next start creates it encrypted.
   - Already encrypted (or not a SQLite file at all): prints
     `... is already encrypted` and skips it. Re-running the command is safe.
3. **Refuses while in use.** It opens the file, leaves WAL mode and takes an
   exclusive lock. If any other process has the file open, this fails with
   `<path> is in use by another process; stop the service first ... and retry`.
4. **Fingerprints the original.** It counts and hashes the rows of every table.
5. **Writes an encrypted copy** to `<path>.encrypting` (created 0600) with
   `VACUUM INTO`.
6. **Verifies the copy.** The copy must not be readable as plaintext, must open
   with the key, must pass `PRAGMA integrity_check`, and must have the same
   tables with the same row counts and content digests as the original. If any
   check fails, the copy is deleted and the original is left untouched.
7. **Replaces the original atomically.** It syncs the copy to disk, renames it
   over the original, syncs the directory and removes the plaintext file's old
   `-wal`/`-shm`/`-journal` sidecars.
8. **Lists plaintext copies.** It reports every other plaintext SQLite file in
   the same directory whose name starts with the database's file name, for
   example the `imap.db.bak-<timestamp>-pre-0.6.0` backup from the 0.6.0
   migration. It never deletes them.

Example output:

```text
state: encrypted /path/to/data/imap.db (4 tables, 120 rows verified); plaintext original removed
state: these plaintext copies still exist; delete them once you no longer need them:
  /path/to/data/imap.db.bak-20261001-090000-pre-0.6.0
cache: encrypted /path/to/data/cache.db (11 tables, 5400 rows verified); plaintext original removed
```

If one file fails, the command still tries the other, prints
`<name>: FAILED: <reason>` and exits non-zero with
`encrypt failed for: <names>`. Key values never appear in error messages.

Because `db encrypt` loads the full config, every `${secret:}` reference in it
(account passwords as well as the DB keys) must resolve, so datawatch has to be
reachable while you run it.

## Fail-closed behaviour

imap-mcp refuses to open a database rather than guess. Every case below stops
startup with an error:

| Situation | Error (abridged) |
|-----------|------------------|
| Key configured, file is plaintext | `<path> is not encrypted but an encryption key is configured; refusing to open (run imap-mcp db encrypt ...)` |
| File is encrypted, no key configured | `<path> is encrypted (or not a SQLite database) and no encryption key is configured` |
| Wrong key | `<path>: wrong encryption key or corrupt file` |
| `${ENV}` reference with the variable unset | `db.encryption_key: key reference is unresolved (environment variable unset?)` |
| Reference resolves to an empty value | `db.encryption_key: resolved to an empty key` |
| `${secret:}` with no `datawatch:` block | `db.encryption_key: ${secret:...} needs a datawatch block with api_url and token` |
| datawatch unreachable, secret missing, or not scoped `service:imap-mcp` | `secret "<name>": ...` with the cause |

(`db.cache.encryption_key` errors name that field instead.)

There is no fallback to plaintext and no auto-generated key.

## run-rules needs only the state key

`imap-mcp run-rules` reads rules from the state DB and never opens the cache.
It resolves only `db.encryption_key`. A scheduled `run-rules` job therefore
needs:

- the state key (or the datawatch service token, if the key is a
  `${secret:}` reference), and
- whatever the account credentials in the config need.

It does not need the cache key. See
[deployment.md](deployment.md#scheduling-run-rules) for a wrapper that loads
the environment for scheduled runs.

## Confirm encryption is on

`/api/health` needs no token and reports the storage state:

```bash
curl -s http://127.0.0.1:8765/api/health | jq .storage
```

```json
{
  "cache_encrypted": true,
  "state_encrypted": true
}
```

Each field is `true` when that file's key is configured. Because the server
refuses to start when a key and a file disagree, a running server with `true`
means the file is encrypted.

The startup log has the same information:

```text
level=INFO msg=storage state_encrypted=true cache_encrypted=true
```

To check a file directly, look at its first bytes. A plaintext SQLite file
starts with `SQLite format 3`; an encrypted one does not:

```bash
head -c 15 ~/.local/share/imap-mcp/imap.db; echo
```

## Backups

Back up the **state DB**. The cache DB is optional: it can always be rebuilt
from IMAP, although rebuilding means a full re-sync and re-running enrichment.

1. Stop the service and make sure `run-rules` is not running, so the file is
   closed and the WAL is checkpointed into it.
2. Copy the file, keeping it private:

   ```bash
   systemctl --user stop imap-mcp
   install -m 600 ~/.local/share/imap-mcp/imap.db /path/to/backups/imap.db.$(date +%Y%m%d)
   systemctl --user start imap-mcp
   ```

Notes:

- A copy of an encrypted file is still encrypted. It is useless without its
  key, so back up the key too, but **store it separately** from the database
  backup. A datawatch secret is backed up with datawatch.
- A backup of a plaintext file is plaintext. It contains webhook signing
  secrets and your rules. Treat it as sensitive.
- Do not leave backups in the database directory with names starting with
  `imap.db` or `cache.db` unless you want `db encrypt` to report them.

## Deleting plaintext copies

After converting, remove the plaintext copies `db encrypt` listed, once you are
sure you do not need them. They usually include the pre-0.6.0 migration backup.

```bash
shred -u /path/to/imap.db.bak-20261001-090000-pre-0.6.0   # or rm
```

`shred` overwrites a file before removing it, but on SSDs, copy-on-write
filesystems (btrfs, ZFS), and filesystems with snapshots or journaling of data,
the old blocks may survive anyway. The same applies to the plaintext original
that `db encrypt` replaced: the rename removes its directory entry, not
necessarily its data blocks. If that matters to you, rely on full-disk
encryption underneath, and treat old snapshots and backups as plaintext.

## If a key is lost

| Lost key | Effect | Recovery |
|----------|--------|----------|
| Cache key | The server refuses to start (`wrong encryption key or corrupt file`, or an unresolved reference). | Delete the cache and let it rebuild. Stop the service, remove `cache.db`, `cache.db-wal` and `cache.db-shm`, set a new `db.cache.encryption_key` (or remove it to go plaintext), and start. The cache is recreated empty, filled from IMAP by the next sync, and re-enriched. No mailbox data is lost. |
| State key | The state DB cannot be opened. Rules, webhooks, webhook secrets, the delivery outbox and replay nonces are unrecoverable. | Restore the state DB from a backup together with the key it was encrypted with. Without a backup: stop the service, move the old `imap.db` aside, set a new key, start (a new empty state DB is created), then recreate your rules and webhooks. New webhooks get new signing secrets, so update every receiver. |

There is no key escrow or recovery mechanism in imap-mcp.
