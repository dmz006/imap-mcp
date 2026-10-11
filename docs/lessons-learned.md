# Lessons learned

Problems found while running imap-mcp on real mailboxes: what we saw, why,
the fix, and the release (AGENT.md D52). Newest first. No mailbox content
or personal details are recorded here; the counts are what the validation
measured. [tuning.md](tuning.md) turns these into a setup guide.

| Release | Symptom | Cause | Fix |
|---|---|---|---|
| 0.18.1 | Identity detection proposed nothing | The owner's display names were counted only from mail indexed after the release and from cached sent mail, and the cache synced only INBOX | Read the From names of up to 200 Sent messages once when none are known |
| 0.18.0 | About half of one account's `needs_reply` items were noise (46 items) | The owner's work and Kindle addresses counted as other people; calendar invites, store notices and family FYI forwards counted as conversations | Identities (D50); drop invites, automated senders and bare forwards (D51). 46 → 23 |
| 0.17.2 | After 0.17.1, the remaining `needs_reply` items on one account were still all spam | That spam passed every header check and the model labelled it a conversation | `needs_reply` lists correspondents only until the model second opinion (D49); a fake `Re:` from a stranger is held |
| 0.17.1 | 14 `needs_reply` items on one account, nearly all spam, five of them already in Trash | Folder never cleared an item (D33), and a model "conversation" label made a sender "personal" | Trash, Junk and hold folders clear items; first-time senders need the header checks too (D48) |
| 0.17.1 | Rule suggestions for senders an existing display-name rule already trashed | Coverage compared rule `from` with the address only; IMAP SEARCH FROM also matches the display name | Coverage checks the display name too |
| 0.17.1 | `needs_reply` could hang | A query ran while the outer rows were still open, on a database limited to one connection | Read all rows before any further query |
| 0.17.0 | A planned rescan would have switched the new-sender hold off for hours | The hold needs a complete scan, and the 0.13 rescan cleared that marker | Rescans keep the marker and track their own progress (`rescan_until`) |
| 0.16.0 | Reply-tracking defaults were chosen without the operator | The implementation made design choices outside the decision process | Reviewed one by one (D45); the lookback became 90 days and rescan progress shows in health |
| 0.15.3 | No digest went out on the first morning | `run-rules` opened only the state database, and the hold crashed on the missing cache | Open the cache too; guard against a missing cache |
| 0.15.2 | The hold flagged mail from `amazonaws.com` and `microsoftonline.com` as brand impersonation | A domain that carries the brand word is the brand | Domains containing the brand word are not impersonation |
| 0.15.1 | Stored classification labels were placeholders copied from the prompt | The classify prompt contained placeholder text the model repeated | Prompt without placeholders; labels validated and cleaned at startup |
| 0.15.0 | Bounce mail for an internal notification channel | A channel sent mail to a group id instead of an address | Sending rejects recipients that aren't addresses |
