# Unsubscribing safely

`unsubscribe` takes you off a mailing list without anyone opening a link
(AGENT.md Q4, D39–D41, D56, D57). It uses only the sender's own
`List-Unsubscribe` header, and only two methods:

| Method | When | What imap-mcp does |
|---|---|---|
| `one-click` | The header has an `https:` URI, the message has `List-Unsubscribe-Post: List-Unsubscribe=One-Click` (RFC 8058), and a DKIM signature that verifies **here** covers both headers | One HTTPS `POST` with the body `List-Unsubscribe=One-Click` |
| `mailto` | The header has a `mailto:` address (and one-click isn't possible) | Sends the email the address asks for, from the account that received the mail |
| `none` | No header, or neither method is safe | Nothing; a rule can trash or move the sender's mail instead |

It never follows links in the body, never fetches a page, and never opens a
tracking URL.

## Why DKIM is checked here

Anyone can put a `List-Unsubscribe` header on a message. A signature from
the sender's domain that covers that header shows the sender put it there.
Your mail server's `Authentication-Results: dkim=pass` says *a* signature
verified, not which headers it covered, so imap-mcp verifies the signature
itself (go-msgauth; the key is looked up in DNS) and requires it to cover
both `List-Unsubscribe` and `List-Unsubscribe-Post` (D41).

## The one-click request

- `https:` only, to a public address: loopback, private, link-local and
  carrier-grade NAT addresses are refused at connect time.
- No cookies, no credentials, no `Referer`; `User-Agent: imap-mcp/<version>`.
- Redirects only to the same host (at most 3); a redirect elsewhere fails.
- `unsubscribe.timeout_seconds` (default 15).
- A 2xx answer is success. The URL usually carries a token that identifies
  you, so it is shown to the caller in the dry run but never logged or
  stored; the log keeps the host.

## Permission and confirmation

- A token needs the **`unsubscribe`** scope; no token has it until you add
  it (D39). The tool is hidden from tokens without it.
- Every call without `confirm: true` is a dry run: the method, the reason
  one-click isn't possible, the target host (or the exact mailto message),
  and how many messages the sender has in the folder.
- A call with `confirm: true` needs a dry run for the same sender in the
  last hour.
- A `mailto` unsubscribe sends an email, so the token also needs **`send`**
  (D40).
- `archive_to` moves the sender's mail in the folder after a successful
  unsubscribe, so it also needs **`write`** (D57).

## Senders who keep mailing

Every unsubscribe is recorded (sender, method, host, HTTP status, time). Mail
from that sender arriving more than `unsubscribe.grace_days` (default 3)
later is reported once in the daily digest ("Still mailing after you
unsubscribed"), in `setup_check`, and in `list_unsubscribes`, each with an
inactive trash rule to create (D56). Arrival is read from the history scan,
so it needs `intelligence.enabled`.

## Using it

```
unsubscribe { account: "work", sender: "news@list.example.com" }
# → {"status":"dry_run","plan":{"method":"one-click","url":"https://list.example.com/u/…","host":"list.example.com",
#    "signed_by":"list.example.com"},"messages_from_sender":42,"next":"Call again with confirm: true …"}

unsubscribe { account: "work", sender: "news@list.example.com", confirm: true, archive_to: "Archive" }
# → {"status":"done","http":{"status":200,"ok":true},"archived":42,"archive_to":"Archive", …}

list_unsubscribes { account: "work" }
# → {"count":1,"items":[{"sender":"news@list.example.com","method":"one-click","host":"list.example.com",
#    "status":"done","status_code":200,"archived":42, …}],"still_mailing":[]}
```

Over REST: `POST /api/unsubscribe` (body `{account, sender | uid, folder,
confirm, archive_to}`) and `GET /api/unsubscribes?account=&dry_runs=&limit=`
([rest-api.md](rest-api.md)). Each confirmed unsubscribe publishes
`unsubscribe.done` ([webhooks.md](webhooks.md)).

## Settings

```yaml
unsubscribe:
  grace_days: 3          # IMAP_MCP_UNSUBSCRIBE_GRACE_DAYS
  timeout_seconds: 15    # IMAP_MCP_UNSUBSCRIBE_TIMEOUT_SECONDS
```
