# Cookbook: Clean a Mailbox and Keep It Clean (imap-mcp + datawatch)

A worked, end-to-end example of using **imap-mcp** to dig a large inbox out of
years of accumulated bulk mail, sort what's left into labels, and then keep it
clean automatically with a **datawatch** scheduled job.

> Everything below is a redacted, fictional walkthrough — example senders and
> domains only (`*.example`). No real mailbox data appears here, which is also
> the rule when you do this for real: derived output goes to your `working_dir`,
> never into a repo.

## The scenario

An operator has a personal mailbox of ~17,000 messages built up over years —
newsletters, store promos, app notifications, conference blasts, plus the mail
that actually matters (banking, health, school, family, work). Goal: get the
inbox down to real mail, organize the rest into labels, and stop the spam from
rebuilding — hands-off.

## Prerequisites

- `imap-mcp serve` running, with its MCP tools available to your agent (stdio or
  HTTP). See `docs/datawatch-integration.md` for wiring.
- A provider where labels are folders. (On Gmail, labels *are* IMAP mailboxes;
  COPY adds a label, MOVE re-files.)
- Optional: **datawatch** for the scheduled, session-independent automation in
  Phase 5. imap-mcp works standalone for Phases 1–4.

---

## Phase 1 — See what's actually in there

Don't guess. Aggregate the whole folder by sender:

```
top_senders { account: "personal", folder: "INBOX", top: 50, group_by: "domain" }
```

Typical output (redacted):

```
1572  personalmail.example       (you / person-to-person)
 478  listings.realty.example    Realty Listings
 425  deals.bigbox.example       Big Box Store
 259  hsa-provider.example       Health Savings
 220  socialapp.example          Social App notifications
 ...
```

Two things this immediately tells you:
- The big removable clusters (store/realty/social bulk).
- The big *keep* clusters (bank, health, person-to-person) you must not touch.

> **Why an aggregator, not search-and-eyeball:** paginating 50 messages at a time
> across 17k is hopeless and unrepresentative. `top_senders` scans the whole
> folder in one call.

---

## Phase 2 — Purge the obvious bulk (by sender, never by subject)

For each clearly-promotional sender, drain it in one call:

```
purge_sender { account: "personal", from: "deals.bigbox.example" }
→ purged 425 messages from "INBOX" matching from="deals.bigbox.example" → Trash
```

`purge_sender` loops until the sender is drained and auto-detects the Trash
mailbox. Everything goes to Trash (recoverable ~30 days), not hard-deleted.

### Three hard-won safety rules

1. **Purge by sender, not by subject.** A subject scan for `"% off"` / `"sale"`
   will match a *friend* writing "garage sale this weekend." When you must use
   subjects, first aggregate the *senders behind* those subjects and only purge
   the bulk vendors — never the subject globally.
2. **Exclude financial / health / personal / school.** Bank, brokerage,
   insurance, medical, and person-to-person mail are off-limits to bulk rules.
   Decide the keep-list before you delete anything.
3. **Mind whole-token matching.** Some IMAP servers (Gmail) match search terms
   as **whole tokens**, not substrings: `from:"bigbox"` can return **0** while
   `from:"bigbox.example"` returns hundreds. If a hook you *expect* to match
   returns 0, try the full domain label. Always confirm counts before and after.

### Verify, don't trust

```
search_messages { account: "personal", from: "deals.bigbox.example" }
→ total_matches: 0     # confirmed drained
```

`search_messages` reports the **true** `total_matches`, so "0 left" really means
zero.

---

## Phase 3 — Sort what remains into labels

Bulk-label by sender. COPY-based labeling keeps the message in the inbox *and*
adds the label; a follow-up move archives it out for a clean inbox.

```
# tag every message from a sender with a category label (creates it if missing)
label_bulk { account: "personal", from: "hsa-provider.example", label: "Health" }

# …or archive straight into the label (removes from inbox, keeps the label)
move_bulk { account: "personal", folder: "INBOX",
            query: "hsa-provider.example", destination: "Health" }
```

A sensible starter taxonomy (map each big sender to one):

| Label | Example senders |
|-------|-----------------|
| Financial | bank, brokerage, card, insurance, payments |
| Health | insurer, HSA, clinic, pharmacy |
| Family / School | school district, university, family addresses |
| Shopping | retailer order/receipt senders |
| Travel | airline, hotel, rental, parking |
| Tech / Work | SaaS vendors, dev tools, work domains |
| Services | local vendors (HVAC, marina, salon…) |
| Legal | law firms |
| Community | groups / lists / HOA |
| Personal | friends + known individual addresses |

**Two judgment calls worth flagging:**
- **Person-to-person webmail** (`personalmail.example`, 1,500+) can't be sorted
  by domain — it's mixed. Leave it, or tag by *known individual* addresses only.
- **Mixed-provider domains** (e.g. a big-tech `*.example` that carries security
  alerts *and* product promos *and* shared docs) aren't safe to bulk-anything.
  Target a specific sub-sender or subject, or leave them.

Archiving the categorized mail drops the inbox dramatically (in the real run:
~10k → ~3.6k) while everything stays findable under its label.

---

## Phase 4 — Make it durable with rules

Manual passes clean *today's* inbox; **rules** keep new mail from rebuilding it.
A rule is a match (`from` / `subject` / `text` / `older_than_days`) plus an
action (`trash` / `move` / `flag` / `seen`):

```
# auto-trash a recurring spammer
create_rule { name: "spam-realty", action: "trash",
              from: "listings.realty.example", account: "personal" }

# auto-file a sender into its label
create_rule { name: "label-health-hsa", action: "move", dest: "Health",
              from: "hsa-provider.example", account: "personal" }

# preview what rules would do, without acting
run_rules { dry_run: true }
```

Persist one rule per bulk sender (trash for junk, move-to-label for keepers).
Rules live in imap-mcp's local database — **not** provider-side filters and
**not** in any repo.

> Tip: a rule that returns 0 when you expect matches is almost always the
> whole-token gotcha from Phase 2 — use the full domain in the `from` hook.

---

## Phase 5 — Keep it clean automatically (datawatch scheduled job)

Apply all rules once from the shell (cron-friendly):

```
imap-mcp run-rules --config ~/.config/imap-mcp/config.yaml
→ run-rules: 105 active rules, 3 messages actioned (dry_run=false)
```

Then have **datawatch** spawn an ephemeral, session-independent job to run it
every hour:

```
datawatch schedule spawn \
  --task "!imap-mcp run-rules --config ~/.config/imap-mcp/config.yaml" \
  --cron "0 * * * *" \
  --one-shot --ephemeral \
  --schedule-name hourly-inbox-rules
```

- **`--cron "0 * * * *"`** — top of every hour.
- **spawn** — a fresh session is created at fire time, so the job does **not**
  depend on any interactive session staying alive.
- **`--one-shot --ephemeral`** — the spawned session runs the command, emits its
  completion marker, terminates, and its workspace is reaped. No accumulation.

Verify it survives a cycle (fire **and** re-arm) before trusting it:

```
datawatch schedule list      # confirm it re-armed to the next hour after firing
```

Now every hour: new junk from known senders is trashed, new mail from known
senders is filed into its label, untouched mail stays in the inbox.

---

## Phase 6 — Maintenance rhythm

- **Monthly:** re-run `top_senders`; new bulk senders always appear. Purge +
  add a rule for each so it never comes back.
- **As needed:** `empty_trash` once you're confident nothing purged was a
  mistake (Trash is your 30-day safety net until then).
- **Tune labels:** when a sender lands in the wrong bucket, move it and update
  (or replace) its rule.

---

## Lessons learned (the short version)

1. **Aggregate first.** `top_senders` over the whole folder beats sampling.
2. **By sender, not by subject.** Subjects catch personal mail; senders don't.
3. **Keep-list before delete-list.** Financial / health / personal / school are
   never bulk targets.
4. **Trust true counts.** Capped previews hide volume; verify `total_matches`.
5. **Whole-token matching is real.** When a hook returns 0 unexpectedly, use the
   full domain.
6. **Rules > one-time sweeps.** Persist a rule per bulk sender; spam stops
   rebuilding.
7. **Schedule independently.** A spawn-based, ephemeral, one-shot job keeps
   working after you close your session — and cleans up after itself.
8. **Recoverable by default.** Move to Trash; empty it only when you're sure.
