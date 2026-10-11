# The screener

The new-sender hold ([examples.md §14](examples.md#14-hold-first-time-senders-that-look-like-spam))
parks mail from first-time senders that look like bulk mail or scams. The
screener decides each held sender without you opening the holding folder
(AGENT.md Q5, D42, D43, D61, D62):

| Decision | What happens |
|---|---|
| `approve` (to `inbox`) | The sender is trusted (never held again) and its held mail moves to INBOX |
| `approve` to `feed` | Trusted; held mail moves to the feed folder, and a rule moves its future mail there |
| `approve` to `receipts` | The same, with the receipts folder |
| `block` | Held mail moves to Trash, and a trash rule is created for the sender's **domain**, or only the **address** when the domain is webmail (gmail.com, outlook.com, …) or one where you have a correspondent (D43) |

Rules the screener creates start **inactive** unless
`rules.screener.rules_active: true` (D61). The feed and receipts folders
are created the first time they are used.

## Deciding

Every call is a **preview** unless `confirm: true` (D62). The preview shows
where the held mail would go, the rule each decision creates, and how many
messages that rule matches in INBOX now; its `run` field is the exact call
that applies it.

```
held_senders { account: "work" }
# → {"count":2,"senders":[{"number":1,"sender":"news@shop.example.com","messages":3,"reasons":"bulk headers; …"},
#                         {"number":2,"sender":"win@prize.example.net","messages":1, …}]}

screen_senders { account: "work", decisions: [
  {sender: "news@shop.example.com", action: "approve", dest: "feed"},
  {sender: "win@prize.example.net", action: "block"}] }
# → {"dry_run":true,"decisions":[
#     {"action":"approve","move_to":"Feed","held_messages":3,"trusted":true,
#      "rule":{"from":"news@shop.example.com","action":"move","dest":"Feed","active":false,"matches_in_inbox":0}},
#     {"action":"block","move_to":"Trash","held_messages":1,
#      "rule":{"from":"prize.example.net","action":"trash","active":false,"matches_in_inbox":0}}],
#    "run":"screen_senders {\"account\":\"work\",\"decisions\":[…],\"confirm\":true}"}
```

Over REST: `GET /api/held?account=` (read) and
`POST /api/held/{account}/decide` with `{decisions, confirm}` (write). From
a terminal:

```bash
imap-mcp screen                                   # list held senders, numbered
imap-mcp screen --account work --approve 1:feed --block 2            # preview
imap-mcp screen --account work --approve 1:feed --block 2 --confirm  # apply
```

Each applied decision is recorded, and `screener.decided` is published with
counts only (`approved`, `blocked`, `moved`, `via`).

## The digest

The daily digest numbers each held sender (`[3] promo@shop.example.com`),
lists senders still held from earlier, and says how to decide them. How you
can answer depends on `rules.screener.email_commands` (D42):

| Mode | How you decide | Trust |
|---|---|---|
| `off` (default) | Your agent, `screen_senders`, REST or `imap-mcp screen` | Your token's `write` scope |
| `token` | Reply to the digest with lines like `approve 3`, `approve 3 feed`, `approve 5 receipts`, `block 7`, `release all` | The reply must answer the **latest** digest, come from the account's own address, carry `dkim=pass` **and** `dmarc=pass` in your server's Authentication-Results, and contain the digest's token (`SCR-…`), which works once |
| `hmac` | A `screen` command through the [inbound command channel](datawatch-integration.md#3b-inbound-command-channel-default-deny-composable-gates) with args `{"account", "commands": "approve 3 feed; block 7"}` (numbers from the latest digest) or `{"account", "decisions": […]}` | Every gate the account's `inbound` block requires, including the HMAC over the envelope; the account must list `screen` in `inbound.capabilities` |

In `token` mode, serve checks for replies every `poll_minutes` (5). A reply
that fails a check is logged with the reason and changes nothing. A handled
reply is marked seen and answered, and a short confirmation ("Screener: 2
approved, 1 blocked") is appended to INBOX.

**The trade-off of `token` mode.** Anyone who can send as you with a valid
DKIM signature *and* has read your latest digest could decide held senders.
The actions only move held mail and create rules, so the risk is small, but
`off` is the default. Check that your server records `dkim=pass` and
`dmarc=pass` on mail you send yourself before relying on it; some providers
don't add Authentication-Results to mail between your own addresses, and
then every reply is rejected.

## Settings

```yaml
rules:
  screener:
    email_commands: off      # off | token | hmac; IMAP_MCP_RULES_SCREENER_EMAIL_COMMANDS
    feed_folder: Feed        # IMAP_MCP_RULES_SCREENER_FEED_FOLDER
    receipts_folder: Receipts   # IMAP_MCP_RULES_SCREENER_RECEIPTS_FOLDER
    rules_active: false      # IMAP_MCP_RULES_SCREENER_RULES_ACTIVE
    poll_minutes: 5          # token mode; IMAP_MCP_RULES_SCREENER_POLL_MINUTES
```
