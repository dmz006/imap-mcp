# Enterprise Gmail / Google Workspace over OAuth

Workspace orgs commonly disable App Passwords and require OAuth for IMAP.
imap-mcp supports two OAuth paths. Pick by how your org is run.

| | Path A — user consent | Path B — service account |
|---|---|---|
| Auth model | 3-legged OAuth, one-time browser sign-in | 2-legged JWT, domain-wide delegation |
| Who sets it up | the user | a Workspace admin (once) |
| Browser flow | yes (once per user) | none |
| Per-user token files | yes (refresh token) | none |
| Headless / many users | awkward | ideal |
| imap-mcp `auth.type` | `xoauth2` | `xoauth2_service_account` |

> **Sending mail does not use OAuth.** `send_message` authenticates to SMTP with
> PLAIN (username + password) only. An OAuth account can read mail, but to send
> its `smtp:` block needs a `username` and `password` (for example an App
> Password, if your org allows them).

> **Required either way:** the IMAP OAuth scope is `https://mail.google.com/`.
> For Workspace addresses you **must** set `provider: google` (Path A) — a custom
> domain like `you@company.com` can't be auto-detected and would otherwise be
> routed to the Microsoft endpoint. (Microsoft 365 accounts use `provider:
> microsoft`; imap-mcp then requests
> `https://outlook.office.com/IMAP.AccessAsUser.All` and `offline_access`
> instead. This guide covers Google only.)

---

## Path A — 3-legged user consent

**Google Cloud (once):**
1. Create/choose a project at <https://console.cloud.google.com>.
2. **APIs & Services → OAuth consent screen** → User type **Internal**
   (Internal Workspace apps skip Google verification and the 7-day test-token
   expiry). Publish it.
3. **Credentials → Create credentials → OAuth client ID → Desktop app.**
   imap-mcp uses the fixed loopback redirect URI
   `http://localhost:8766/oauth/callback`. Desktop-app clients accept loopback
   redirects without registering them; if you use a **Web application** client
   instead, add that exact URI under *Authorized redirect URIs*.
4. Note the **client ID** and **client secret**. (No API to "enable" — the
   `https://mail.google.com/` scope is requested by imap-mcp at sign-in. Your
   admin may need to allow the app under Admin Console → Security → API
   controls.)

**imap-mcp config:**
```yaml
accounts:
  - name: work-gmail
    imap: { host: imap.gmail.com, port: 993, tls: true }
    auth:
      type: xoauth2
      provider: google                 # REQUIRED for Workspace domains
      username: you@company.com
      client_id: ${GMAIL_OAUTH_CLIENT_ID}
      client_secret: ${secret:gmail_oauth_client_secret}   # or ${ENV}
      token_file: ~/.config/imap-mcp/work-gmail.token.json
```

**Authorize (once):**
```bash
imap-mcp auth-setup --account work-gmail --config /path/to/config.yaml
```
(`--account` defaults to the default account; `--config` to the usual config
path.) What happens:

1. imap-mcp prints a consent URL. It does **not** open a browser; copy the URL
   into one yourself.
2. It listens for the callback on `127.0.0.1` and `::1`, port 8766 (loopback
   only), and prints `Waiting for OAuth callback on
   http://localhost:8766/oauth/callback ...`.
3. You sign in and approve. Google redirects the browser to that URL.
   imap-mcp checks the random `state` value it generated (callbacks with a
   wrong or missing state are rejected and it keeps waiting), exchanges the
   code, and writes the token (including the refresh token) to `token_file`
   with mode 0600.
4. If you deny consent, Google's error is reported and the command exits.

It waits until the callback arrives or you press Ctrl-C; there is no timeout.
The browser must reach port 8766 on the machine running `auth-setup`. On a
headless server, forward the port first, e.g. `ssh -L 8766:localhost:8766
server`, then open the URL in your local browser.

`auth-setup` works only for `type: xoauth2` accounts. After that, imap-mcp
refreshes the access token automatically on every connect.

---

## Path B — service account + domain-wide delegation

Best for headless/admin-managed/multi-user. No browser, no per-user tokens.

**Google Cloud + Workspace admin (once):**
1. Create a **service account** in a GCP project; create a **JSON key**.
2. Enable **domain-wide delegation** on the service account; note its **client
   ID** (a long number).
3. In the **Workspace Admin Console → Security → Access and data control → API
   controls → Domain-wide delegation**, add that client ID and authorize the
   scope `https://mail.google.com/`.

**imap-mcp config:**
```yaml
accounts:
  - name: work-gmail-sa
    imap: { host: imap.gmail.com, port: 993, tls: true }
    auth:
      type: xoauth2_service_account
      username: you@company.com                    # mailbox to access (impersonated)
      service_account_file: ~/.config/imap-mcp/gworkspace-sa.json
      # subject: you@company.com                   # defaults to username
```
No `auth-setup` step — imap-mcp mints an impersonated access token from the key
on each connect. To service many mailboxes, add one account block per user
(same key file, different `username`/`subject`).

> Treat the service-account JSON key like a password: keep it outside the repo
> (it's gitignored via `*.json`/`tokens/`), lock it down (`chmod 600`), and
> consider referencing it from a secrets-managed path.

---

## Troubleshooting

- **`xoauth2 authenticate … AUTHENTICATIONFAILED`** — usually the access token
  lacks the IMAP scope, or (Path B) domain-wide delegation isn't authorized for
  `https://mail.google.com/`. Re-check the scope in both places.
- **Routed to the Microsoft endpoint** — you forgot `provider: google` on a
  custom-domain account (Path A).
- **`listen for OAuth callback: … address already in use`** — something else
  holds port 8766 (often an earlier `auth-setup`). Stop it and retry.
- **Browser shows "invalid state"** — the callback came from an older consent
  URL. Use the URL printed by the running `auth-setup`.
- **Refresh token stops working after ~7 days** — the OAuth consent screen is in
  "testing"; set it to Internal/Published.
- **`invalid_grant` / `unauthorized_client` (Path B)** — the service-account
  client ID isn't authorized in the Admin Console, or the `subject` user doesn't
  exist in the domain.
