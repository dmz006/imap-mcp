package intel

import (
	"context"
	"database/sql"
	"strings"
	"unicode"
)

// The owner's other addresses (AGENT.md D50). Besides the account logins,
// identity.also_me (addresses or @domains) and confirmed identities count as
// the owner everywhere. Candidates are detected from the owner's display
// names and address forms and wait for the owner to confirm or reject them.

// ownSet holds the owner's addresses and "@domain" entries.
type ownSet map[string]bool

// has reports whether addr is the owner's: listed itself, or at a listed domain.
func (o ownSet) has(addr string) bool {
	if o[addr] {
		return true
	}
	d := domainOf(addr)
	return d != "" && o["@"+d]
}

func (o ownSet) clone() ownSet {
	c := ownSet{}
	for k, v := range o {
		c[k] = v
	}
	return c
}

// Identity statuses.
const (
	IdentityCandidate = "candidate"
	IdentityConfirmed = "confirmed"
	IdentityRejected  = "rejected"
	IdentityConfig    = "config"
)

// NormalizeIdentity lower-cases an address or @domain entry ("" if invalid).
func NormalizeIdentity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.Contains(s, "@") || strings.ContainsAny(s, " <>") {
		return ""
	}
	return s
}

// refreshIdentities rebuilds each account's own set from the logins, the
// config list and confirmed identities, and rewrites the stored history once
// for every entry not yet applied.
func (sc *Scanner) refreshIdentities(ctx context.Context) error {
	for _, e := range sc.alsoMe {
		if e = NormalizeIdentity(e); e == "" {
			continue
		}
		if _, err := sc.state.ExecContext(ctx, `INSERT INTO identities(address, status, updated_at) VALUES(?,?,unixepoch())
			ON CONFLICT(address) DO UPDATE SET status = excluded.status, updated_at = unixepoch()
			WHERE identities.status <> 'confirmed' AND identities.status <> 'config'`, e, IdentityConfig); err != nil {
			return err
		}
	}
	extra, pending, err := ownIdentities(ctx, sc.state)
	if err != nil {
		return err
	}
	for account, base := range sc.base {
		set := base.clone()
		for _, e := range extra {
			set[e] = true
		}
		sc.own[account] = set
	}
	for _, e := range pending {
		if err := ApplyIdentity(ctx, sc.state, e); err != nil {
			return err
		}
	}
	return nil
}

// ownIdentities returns the config and confirmed entries, and those whose
// history has not been rewritten yet.
func ownIdentities(ctx context.Context, db *sql.DB) (all, pending []string, err error) {
	rows, err := db.QueryContext(ctx, `SELECT address, applied_at IS NULL FROM identities WHERE status IN ('confirmed','config')`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		var todo bool
		if err := rows.Scan(&a, &todo); err != nil {
			return nil, nil, err
		}
		all = append(all, a)
		if todo {
			pending = append(pending, a)
		}
	}
	return all, pending, rows.Err()
}

// ApplyIdentity rewrites the stored history for an address or @domain that
// is the owner's (D50): its past messages become the owner's own, its sender
// profile is marked as the owner's and hidden, and conversations with it are
// dropped from reply tracking. Recipients were never stored, so the counts of
// people it wrote to are not recomputed.
func ApplyIdentity(ctx context.Context, db *sql.DB, entry string) error {
	col, val := "address", entry
	if strings.HasPrefix(entry, "@") {
		col, val = "domain", strings.TrimPrefix(entry, "@")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{
		`UPDATE intel_messages SET outgoing = 1 WHERE sender_id IN (SELECT id FROM senders WHERE ` + col + ` = ?)`,
		`DELETE FROM reply_threads WHERE counterpart IN (SELECT address FROM senders WHERE ` + col + ` = ?)`,
		`UPDATE senders SET role = 'self', role_source = 'identity', dirty = 0, updated_at = unixepoch() WHERE ` + col + ` = ?`,
		`UPDATE identities SET applied_at = unixepoch() WHERE address = ?`,
	} {
		arg := val
		if strings.HasPrefix(q, "UPDATE identities") {
			arg = entry
		}
		if _, err := tx.ExecContext(ctx, q, arg); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ownerName counts a display name the owner sends under.
func (st *store) ownerName(ctx context.Context, tx *sql.Tx, name string) error {
	n := normName(name)
	if n == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO owner_names(name, count) VALUES(?, 1)
		ON CONFLICT(name) DO UPDATE SET count = owner_names.count + 1`, n)
	return err
}

// seedSample is how many Sent messages are read for the owner's display
// names when none are known yet (headers only).
const seedSample = 200

// seedOwnerNames reads the owner's display names from the Sent folder once,
// when none are known: the index only counts names on mail indexed from
// 0.18 on, and the cache may not sync Sent.
func (sc *Scanner) seedOwnerNames(ctx context.Context, account string, all []Folder) error {
	var n int
	if err := sc.state.QueryRowContext(ctx, `SELECT count(*) FROM owner_names`).Scan(&n); err != nil || n > 0 {
		return err
	}
	sent := ""
	for _, f := range all {
		if slicesContains(f.Attrs, `\Sent`) {
			sent = f.Name
			break
		}
		switch strings.ToLower(f.Name) {
		case "sent", "sent mail", "sent items", "sent messages", "[gmail]/sent mail", "inbox.sent":
			sent = f.Name
		}
	}
	if sent == "" {
		return nil
	}
	b, err := sc.src.Fetch(ctx, account, sent, 0, seedSample)
	if err != nil {
		return err
	}
	own := sc.own[account]
	tx, err := sc.state.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, h := range b.Headers {
		if own.has(h.From.Addr) && h.From.Name != "" {
			if err := sc.st.ownerName(ctx, tx, h.From.Name); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// minOwnerNameUses: a display name counts as the owner's after this many
// outgoing messages, so a one-off "on behalf of" name does not.
const minOwnerNameUses = 3

// normName reduces a display name to its sorted lower-case words, without
// parenthesised parts or punctuation: "Zendzian, David (Work)" and
// "david zendzian" both become "david zendzian".
func normName(name string) string {
	var b strings.Builder
	depth := 0
	for _, r := range strings.ToLower(name) {
		switch {
		case r == '(':
			depth++
		case r == ')':
			if depth > 0 {
				depth--
			}
		case depth > 0:
		case unicode.IsLetter(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	if len(words) < 2 { // a single word ("David") is too weak to be evidence
		return ""
	}
	sortStrings(words)
	return strings.Join(words, " ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// localForms are address local parts built from a name: first+last in the
// usual shapes ("davidzendzian", "david.zendzian", "dzendzian", ...).
func localForms(words []string) map[string]bool {
	out := map[string]bool{}
	if len(words) < 2 {
		return out
	}
	for i, a := range words {
		for j, b := range words {
			if i == j {
				continue
			}
			for _, sep := range []string{"", ".", "_", "-"} {
				out[a+sep+b] = true
			}
			out[a[:1]+b] = true
			out[a[:1]+"."+b] = true
		}
	}
	return out
}

// detectIdentities records candidate addresses: a sender that uses one of
// the owner's display names, or whose address is built from it. Known
// identities, confirmed or rejected, are left alone.
func (sc *Scanner) detectIdentities(ctx context.Context) error {
	rows, err := sc.state.QueryContext(ctx, `SELECT name FROM owner_names WHERE count >= ?`, minOwnerNameUses)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	forms := map[string]bool{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names[n] = true
			for f := range localForms(strings.Fields(n)) {
				forms[f] = true
			}
		}
	}
	rows.Close()
	own := ownSet{}
	for _, set := range sc.own {
		for k := range set {
			own[k] = true
		}
	}
	// Recent sent mail in the cache also shows the owner's names, so
	// detection works before much new mail has been indexed.
	if sc.cache != nil {
		crows, err := sc.cache.QueryContext(ctx, `SELECT lower(from_addr), COALESCE(from_name,''), count(*) FROM messages
			WHERE COALESCE(from_name,'') <> '' GROUP BY 1, 2`)
		if err == nil {
			for crows.Next() {
				var addr, name string
				var n int
				if crows.Scan(&addr, &name, &n) == nil && n >= minOwnerNameUses && own.has(addr) {
					if nn := normName(name); nn != "" {
						names[nn] = true
						for f := range localForms(strings.Fields(nn)) {
							forms[f] = true
						}
					}
				}
			}
			crows.Close()
		}
	}
	if len(names) == 0 {
		return nil
	}
	rows, err = sc.state.QueryContext(ctx, `SELECT s.address, COALESCE(s.name,'') FROM senders s
		LEFT JOIN identities i ON i.address = s.address
		WHERE i.address IS NULL AND COALESCE(s.role_source,'') <> 'identity'`)
	if err != nil {
		return err
	}
	type cand struct{ addr, evidence string }
	var found []cand
	for rows.Next() {
		var addr, name string
		if rows.Scan(&addr, &name) != nil || own.has(addr) {
			continue
		}
		var why []string
		if n := normName(name); n != "" && names[n] {
			why = append(why, "uses your name ("+name+")")
		}
		local := addr
		if i := strings.IndexByte(addr, '@'); i > 0 {
			local = addr[:i]
		}
		if forms[local] {
			why = append(why, "the address is built from your name")
		}
		if len(why) > 0 {
			found = append(found, cand{addr, strings.Join(why, "; ")})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range found {
		if _, err := sc.state.ExecContext(ctx, `INSERT OR IGNORE INTO identities(address, status, evidence, updated_at)
			VALUES(?,?,?,unixepoch())`, c.addr, IdentityCandidate, c.evidence); err != nil {
			return err
		}
	}
	return nil
}
