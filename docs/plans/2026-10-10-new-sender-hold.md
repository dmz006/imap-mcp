# New-sender hold and held-mail digest (0.15.0)

Decisions: AGENT.md D30 (hold) and D31 (digest). Status: done in 0.15.0 (live validation below).

## Problem

Domain and display-name rules cannot keep up with snowshoe spam: in a
sampled inbox nearly every spam message came from a domain seen once. What
those messages share is that the sender has no history with the mailbox owner.
Holding every first-time sender would also hold real first contacts, so the
hold has to tell the two apart, and the owner must not have to keep checking a
folder.

Authentication alone does not separate them: on a self-hosted server the
sampled spam either passed DKIM (throwaway domains sign their mail) or carried
no DKIM/DMARC result at all.

## Design

A rule condition, `new_sender: true` (window `new_sender_days`, default 30),
paired with `action: move` to a holding folder. A message matches when:

1. **The sender has no history.** Not one of the account's own addresses,
   never written to or replied to, nothing from them before the window, not
   released before (see below), and not at a non-freemail domain the owner
   has written to. It refuses to match anything until the account's history
   scan is complete, or every sender would look new.
2. **And it looks like bulk mail or a scam.** Header signals only (PEEK, no
   body):

   | Signal | Weight |
   |---|---|
   | Display name names a brand, a government agency or the owner's own domain, sent from an unrelated domain | 2 |
   | Not addressed to the owner (no recipients, undisclosed, or only the sender) | 1 |
   | Bulk headers (List-Unsubscribe, List-Id, Precedence bulk/list/junk) | 1 |
   | Throwaway-looking domain (digits mixed into the name, or a low-cost TLD) | 1 |
   | The owner's address in the subject | 1 |
   | Reply-To at a different domain | 1 |

   Signs of a real contact keep the message in the inbox whatever the score:
   it replies to mail the owner sent (In-Reply-To/References match an
   outgoing message in the D28 index), or it copies someone the owner has
   written to.

   Score 0 stays. Score 2 or more is held. Score 1 asks the model: the
   message's enrichment hall (`conversation`/`personal` stays, any other hall
   is held). Not classified yet means it stays for now and is judged on the
   next run.

**Release trusts the sender.** Each held message is recorded (`held_messages`
in `imap.db`: account, message hash, Message-ID, sender, reasons, time). If a
held message is found back in the inbox, the owner moved it: the sender is
marked trusted and never held again.

**Digest (D31).** Once a day, after the configured hour, a full rule run
(the hourly job) sends a digest of messages held since the last one:

- a summary message APPENDed to the account's INBOX (no mail is sent):
  sender, subject, date and why it was held, and how to release;
- a `hold.digest` bus event (webhooks): account and count only, never
  addresses or subjects.

## Phases

| Phase | Content | Status |
|---|---|---|
| H1 | `new_sender` history condition, scan-complete guard, validation | Done |
| H2 | Header signals, real-contact overrides, hall fallback | Done |
| H3 | `held_messages`, release detection, `senders.trusted` | Done |
| H4 | Daily digest: INBOX summary + `hold.digest` event; config | Done |
| H5 | Tests, docs (rules, webhooks, examples, skill), live validation with the rule inactive first | Tests and docs done; live validation in progress |

Also in 0.15.0: classification placeholders. The classify model sometimes
copied its prompt's placeholder text into the wing/room tags, and those became
knowledge-graph entities. The prompt no longer contains placeholders, tags are
sanitised on the way in, and startup removes the stored ones.
