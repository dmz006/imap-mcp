# Tuning imap-mcp on a real mailbox

This is the order that works, what to expect, and how to check each step,
learned from tuning imap-mcp on a busy, years-old mailbox (AGENT.md D52).
`setup_check` (or `GET /api/setup-check`) finds most of the gaps below on
your own install. [lessons-learned.md](lessons-learned.md) records each
problem we hit and how it was fixed.

## The first week, in order

1. **Let the history scan finish.** Profiles, the new-sender hold, reply
   tracking, rule suggestions and identity detection all read it. Watch
   `/api/health` → `intelligence`: wait for `backfill_complete` and
   `reply_history_complete` to be true. A large mailbox takes a few hours
   at the default 600 messages a minute.
2. **Tell it which addresses are you.** Run `suggest_identities` and
   confirm your other addresses (work, Kindle, old ones), or list them in
   `identity.also_me`. Until then they count as other people: they show up
   as conversations you owe and can be held as new senders. Expect a few
   services that put your name on their own mail (GitHub notifications,
   airline reservations); reject those.
3. **Import the starter rule packs you want.** `list_rule_packs`, then
   `import_rule_pack` (rules arrive inactive). Dry-run each one, then turn
   it on.
4. **Turn on the new-sender hold, carefully.** Create the `new_sender` rule
   inactive, dry-run it, and read the `preview`: every held sender with the
   reason. Keep anyone it gets wrong by moving their mail back after you
   turn it on; that sender is never held again. See
   [examples.md §14](examples.md#14-hold-first-time-senders-that-look-like-spam).
5. **Sweep old spam once.** The hold only looks back 30 days. A one-off
   rule with `new_sender_days: 3650`, moved to the same holding folder,
   catches older spam; delete that rule afterwards.
6. **Read the first digests.** The daily summary in your inbox lists held
   mail, conversations waiting on you, rule suggestions, addresses that may
   be yours and new setup findings. Act on what's wrong, not on everything.
7. **Let learning catch up.** After a week of you trashing and junking mail
   yourself, `suggest_rules` has evidence. Accept the suggestions that are
   right; `dismiss_suggestion` the rest.

## What to expect on real mail

- **Some spam looks exactly like a person.** Addressed to you, no list
  headers, an ordinary-looking domain, and the classify model calls it a
  conversation. Header rules can't separate it from a genuine first
  contact. That is why `needs_reply` lists only people you've written to
  until the model second opinion arrives, and why a fake `Re:` from a
  stranger is held on its own.
- **Gmail and other servers search differently.** Gmail matches whole
  words; most other servers match substrings, display names included. A
  rule that works on one may never match on the other. `setup_check`
  reports active rules that have never matched in 30 days
  ([rules.md](rules.md#gotcha-whole-token-matching)).
- **Webmail domains need care.** Never write a domain rule for gmail.com,
  outlook.com and the like; learned rules never do.
- **Old clean-ups look like your own choices.** Mail your rules trashed
  before move tracking existed sits in Trash like mail you trashed. Rule
  suggestions skip senders an existing rule already covers (by address or
  display name), so old clean-ups don't come back as suggestions.
- **The cache may not have your sent mail.** If `sync.folders` lists only
  INBOX, search and the model never see what you wrote. Reply tracking and
  identity detection use the history scan instead.

## Checking that it works

Check with counts, not by reading mail:

- `needs_reply` / `awaiting_reply` with `limit: 200`: is the count
  plausible, and are the items people? If a kind of noise repeats, fix the
  cause (a rule, an identity, a dismissal), not the items.
- `suggest_rules`: each suggestion's `discarded` / `received` should match
  what you remember doing.
- The new-sender hold: the digest's "Held for review" count each morning.
  Anything real in it means a release (move it back) and a trusted sender.
- `setup_check` after any change.

## Lessons as design rules

What we learned, and how imap-mcp now behaves:

| Lesson | Built in |
|---|---|
| Rules file real conversations out of INBOX | Reply tracking ignores the folder, except Trash, Junk and hold folders |
| A model label alone doesn't make a sender a person | First-time senders need the header checks too, and are off until Q3 |
| Your other addresses are you | `identity.also_me`, `suggest_identities`, history rewritten on confirm |
| Calendar invites, store notices and FYI forwards aren't conversations | `needs_reply` drops invites, automated senders and bare forwards |
| A stranger's `Re:` that answers nothing is staged | The hold scores it 2 |
| Old rule clean-ups look like your own discards | Suggestions skip rule-covered senders, by address or display name |
| Nothing should change mail without a preview | Holds, packs and learned rules start inactive or suggested |
