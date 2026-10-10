package service

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/intel"
)

// Learning from moves (Q2; AGENT.md D34–D36, D46, D47). The header scan
// records where each message was last seen. A sender whose received mail the
// owner mostly discarded (moved to Trash or Junk) becomes a suggested rule
// that repeats what the owner did; rules.learn.mode decides whether it is
// only suggested or created (inactive or active). Correspondents, trusted
// senders, the owner's own addresses, senders a rule already covers and held
// senders are never suggested, and freemail domains are never domain-ruled.

const (
	ruleMovesRetention   = 365 * 24 * time.Hour
	defaultSuggestLimit  = 50
	digestSuggestLimit   = 25
	learnedRulePrefix    = "learned: "
	learnStatusSuggested = "suggested"
	learnStatusCreated   = "created"
	learnStatusDismissed = "dismissed"
)

// Suggestion is one learned rule, suggested or created.
type Suggestion struct {
	Account   string   `json:"account"`
	Target    string   `json:"target"` // an address, or @domain
	Kind      string   `json:"kind"`   // address | domain
	Addresses []string `json:"addresses,omitempty"`
	Received  int      `json:"received"`
	Discarded int      `json:"discarded"`
	Ratio     float64  `json:"ratio"`
	Action    string   `json:"action"` // trash | move
	Dest      string   `json:"dest,omitempty"`
	Matches   int      `json:"matches"` // what the rule would match in INBOX now
	Status    string   `json:"status"`  // new | suggested | created
	RuleID    int64    `json:"rule_id,omitempty"`
	Rule      db.Rule  `json:"rule"` // the exact rule (create_rule input)
}

// recordRuleMoves remembers messages a rule moved or trashed (D34).
func (s *Service) recordRuleMoves(account string, rule db.Rule, messageIDs []string) error {
	if len(messageIDs) == 0 || s.db == nil || s.db.StateSQL() == nil {
		return nil
	}
	dest := ""
	if len(rule.Actions) > 0 {
		dest = rule.Actions[0].Dest
	}
	now := time.Now().Unix()
	for _, id := range messageIDs {
		if h := intel.MsgHash(id); h != 0 {
			if _, err := s.db.StateSQL().Exec(`INSERT INTO rule_moves(account, msg_hash, rule_id, dest, moved_at) VALUES(?,?,?,?,?)
				ON CONFLICT(account, msg_hash) DO UPDATE SET rule_id = excluded.rule_id, dest = excluded.dest, moved_at = excluded.moved_at`,
				account, h, rule.ID, dest, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// discardFolders lists an account's discard folders by kind (D34).
func (s *Service) discardFolders(account string) (map[string]string, error) {
	conn, err := s.pool.Resolve(account)
	if err != nil {
		return nil, err
	}
	conn.Lock()
	defer conn.Unlock()
	opts := &imaplib.ListOptions{}
	if conn.Client().Caps().Has(imaplib.CapSpecialUse) {
		opts.ReturnSpecialUse = true
	}
	boxes, err := conn.Client().List("", "*", opts).Collect()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, b := range boxes {
		attrs := make([]string, len(b.Attrs))
		for i, a := range b.Attrs {
			attrs[i] = string(a)
		}
		if k := intel.DiscardKind(b.Mailbox, attrs, s.cfg.Rules.Learn.DiscardFolders); k != "" {
			out[b.Mailbox] = k
		}
	}
	return out, nil
}

// senderTally is one sender's received and discarded counts.
type senderTally struct {
	address, domain string
	name            string // display name; IMAP SEARCH FROM matches it too
	received        int
	discarded       int
	byFolder        map[string]int
}

// suggestions computes an account's learned rules from the index. It
// changes nothing. With matches, each one's INBOX match count is read live.
func (s *Service) suggestions(ctx context.Context, account string, matches bool) ([]Suggestion, error) {
	lc := s.cfg.Rules.Learn
	kinds, err := s.discardFolders(account)
	if err != nil {
		return nil, err
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	folders := make([]any, 0, len(kinds))
	for f := range kinds {
		folders = append(folders, f)
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(folders)), ",")
	st := s.db.StateSQL()

	// Candidates: senders with at least min_discards of the owner's own
	// discards, never written to or replied to, not trusted.
	rows, err := st.QueryContext(ctx, `SELECT s.address, COALESCE(s.domain,''), COALESCE(s.name,''), m.folder, count(*)
		FROM intel_messages m JOIN senders s ON s.id = m.sender_id
		LEFT JOIN rule_moves rm ON rm.account = m.account AND rm.msg_hash = m.msg_hash
		WHERE m.account = ? AND m.outgoing = 0 AND m.folder IN (`+ph+`) AND rm.msg_hash IS NULL
			AND s.sent_count = 0 AND s.reply_count = 0 AND COALESCE(s.trusted, 0) = 0
		GROUP BY s.address, m.folder`, append([]any{account}, folders...)...)
	if err != nil {
		return nil, err
	}
	tally := map[string]*senderTally{}
	for rows.Next() {
		var addr, domain, name, folder string
		var n int
		if err := rows.Scan(&addr, &domain, &name, &folder, &n); err != nil {
			rows.Close()
			return nil, err
		}
		t := tally[addr]
		if t == nil {
			t = &senderTally{address: addr, domain: domain, name: name, byFolder: map[string]int{}}
			tally[addr] = t
		}
		t.discarded += n
		t.byFolder[folder] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for addr, t := range tally {
		if t.discarded < lc.MinDiscardsOrDefault() {
			delete(tally, addr)
		}
	}
	if len(tally) == 0 {
		return nil, nil
	}
	for addr, t := range tally {
		if err := st.QueryRowContext(ctx, `SELECT count(*) FROM intel_messages m JOIN senders s ON s.id = m.sender_id
			WHERE m.account = ? AND m.outgoing = 0 AND s.address = ?`, account, addr).Scan(&t.received); err != nil {
			return nil, err
		}
		if t.received == 0 || float64(t.discarded)/float64(t.received) < lc.RatioOrDefault() {
			delete(tally, addr)
		}
	}

	ex, err := s.learnExclusions(ctx, account)
	if err != nil {
		return nil, err
	}
	byDomain := map[string][]*senderTally{}
	for addr, t := range tally {
		// A rule covers a sender when its From matches the address or the
		// display name, as IMAP SEARCH FROM does (e.g. a "Dr. Martin" rule).
		if ex.own[addr] || ex.me.has(addr) || ex.ownDomains[t.domain] || ex.held[addr] || ex.covered(addr) || (t.name != "" && ex.covered(t.name)) ||
			ex.state[addr] == learnStatusDismissed ||
			ex.state["@"+t.domain] == learnStatusDismissed { // a dismissed domain covers its addresses too
			continue
		}
		byDomain[t.domain] = append(byDomain[t.domain], t)
	}

	var out []Suggestion
	for domain, ts := range byDomain {
		sort.Slice(ts, func(i, j int) bool { return ts[i].address < ts[j].address })
		target := "@" + domain
		if domain != "" && len(ts) >= lc.DomainMinOrDefault() && !freemailDomains[domain] && !ex.covered(target) &&
			ex.state[target] != learnStatusDismissed {
			protected, err := s.domainProtected(ctx, domain)
			if err != nil {
				return nil, err
			}
			if !protected {
				merged := &senderTally{address: target, domain: domain, byFolder: map[string]int{}}
				var addrs []string
				for _, t := range ts {
					merged.received += t.received
					merged.discarded += t.discarded
					for f, n := range t.byFolder {
						merged.byFolder[f] += n
					}
					addrs = append(addrs, t.address)
				}
				out = append(out, s.suggestion(account, target, "domain", addrs, merged, kinds, ex))
				continue
			}
		}
		for _, t := range ts {
			out = append(out, s.suggestion(account, t.address, "address", nil, t, kinds, ex))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Discarded != out[j].Discarded {
			return out[i].Discarded > out[j].Discarded
		}
		return out[i].Target < out[j].Target
	})
	if matches {
		for i := range out {
			n, err := s.applyRule(out[i].Rule, true, nil)
			if err != nil {
				return nil, err
			}
			out[i].Matches = n
		}
	}
	return out, nil
}

// suggestion builds one learned rule that mirrors where the owner put the
// mail: mostly Trash means a trash rule, otherwise a move to that folder.
func (s *Service) suggestion(account, target, kind string, addrs []string, t *senderTally, kinds map[string]string, ex *learnExclusions) Suggestion {
	top, topN := "", -1
	for f, n := range t.byFolder {
		if n > topN || (n == topN && f < top) {
			top, topN = f, n
		}
	}
	sg := Suggestion{Account: account, Target: target, Kind: kind, Addresses: addrs, Received: t.received,
		Discarded: t.discarded, Ratio: float64(int(float64(t.discarded)/float64(t.received)*100)) / 100, Status: "new"}
	act := db.RuleAction{Type: "trash"}
	if kinds[top] != intel.KindTrash {
		act = db.RuleAction{Type: "move", Dest: top}
		sg.Dest = top
	}
	sg.Action = act.Type
	sg.Rule = db.Rule{Name: learnedRulePrefix + target, Active: false, // accepted rules start inactive and are previewed
		Description: fmt.Sprintf("Learned from your moves: you discarded %d of %d messages from %s", t.discarded, t.received, target),
		Conditions:  db.RuleConditions{Account: account, From: target}, Actions: []db.RuleAction{act}}
	if st := ex.state[target]; st != "" {
		sg.Status, sg.RuleID = st, ex.ruleID[target]
	}
	return sg
}

// learnExclusions is what learning never suggests (D36, D47).
type learnExclusions struct {
	me                    meSet // the owner's addresses (D50)
	own, ownDomains, held map[string]bool
	froms                 []string          // lower-case From conditions of the account's rules
	state                 map[string]string // learn_state status by target
	ruleID                map[string]int64
}

func (e *learnExclusions) covered(target string) bool {
	t := strings.ToLower(target)
	return slices.ContainsFunc(e.froms, func(f string) bool { return strings.Contains(t, f) })
}

func (s *Service) learnExclusions(ctx context.Context, account string) (*learnExclusions, error) {
	ex := &learnExclusions{me: s.me(ctx), own: map[string]bool{}, ownDomains: map[string]bool{}, held: map[string]bool{},
		state: map[string]string{}, ruleID: map[string]int64{}}
	for _, a := range s.cfg.Accounts {
		addrs := []string{a.Auth.Username}
		if a.SMTP != nil {
			addrs = append(addrs, a.SMTP.From)
		}
		for _, addr := range addrs {
			if addr = bareAddr(addr); strings.Contains(addr, "@") {
				ex.own[addr] = true
				if d := domainOf(addr); !freemailDomains[d] {
					ex.ownDomains[d] = true
				}
			}
		}
	}
	rules, err := s.db.Rules.List()
	if err != nil {
		return nil, err
	}
	def := s.cfg.DefaultAccount().Name
	for _, r := range rules {
		ra := r.Conditions.Account
		if ra == "" {
			ra = def
		}
		if ra == account && strings.TrimSpace(r.Conditions.From) != "" && r.Conditions.Subject == "" && r.Conditions.Text == "" {
			ex.froms = append(ex.froms, strings.ToLower(strings.TrimSpace(r.Conditions.From)))
		}
	}
	st := s.db.StateSQL()
	rows, err := st.QueryContext(ctx, `SELECT DISTINCT COALESCE(sender,'') FROM held_messages WHERE account = ?`, account)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			ex.held[bareAddr(a)] = true
		}
	}
	rows.Close()
	rows, err = st.QueryContext(ctx, `SELECT target, status, COALESCE(rule_id,0) FROM learn_state WHERE account = ?`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t, status string
		var id int64
		if err := rows.Scan(&t, &status, &id); err != nil {
			return nil, err
		}
		ex.state[t], ex.ruleID[t] = status, id
	}
	return ex, rows.Err()
}

// domainProtected reports whether anyone at the domain is a correspondent
// or trusted, so no domain rule may cover them (D36).
func (s *Service) domainProtected(ctx context.Context, domain string) (bool, error) {
	var n int
	err := s.db.StateSQL().QueryRowContext(ctx, `SELECT count(*) FROM senders WHERE domain = ?
		AND (sent_count > 0 OR reply_count > 0 OR COALESCE(trusted, 0) = 1)`, domain).Scan(&n)
	return n > 0, err
}

// SuggestParams selects learned-rule suggestions.
type SuggestParams struct {
	Account string // "" = every account
	Limit   int    // 0 = 50
}

// SuggestionList is a suggest_rules result.
type SuggestionList struct {
	Mode        string       `json:"mode"`
	Count       int          `json:"count"`
	Suggestions []Suggestion `json:"suggestions"`
	// HistoryComplete is false while the history rescan is still recording
	// where messages are; counts may be low until it finishes.
	HistoryComplete bool `json:"history_complete"`
}

// SuggestRules lists learned rules not yet accepted: what the owner's moves
// suggest, each with the exact rule, the evidence and its INBOX match count.
func (s *Service) SuggestRules(ctx context.Context, p SuggestParams) (SuggestionList, error) {
	out := SuggestionList{Mode: s.cfg.Rules.Learn.ModeOrDefault(), Suggestions: []Suggestion{}}
	if s.db == nil || s.db.StateSQL() == nil {
		return out, unavailable("the state database is not open in this mode")
	}
	if !s.cfg.Intel.On() {
		return out, unavailable("learning from moves needs intelligence (the header scan) enabled")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = defaultSuggestLimit
	}
	accounts := s.replyAccounts(p.Account)
	for _, account := range accounts {
		list, err := s.suggestions(ctx, account, false)
		if err != nil {
			return out, err
		}
		for _, sg := range list {
			if sg.Status != learnStatusCreated && len(out.Suggestions) < p.Limit {
				out.Suggestions = append(out.Suggestions, sg)
			}
		}
	}
	for i := range out.Suggestions {
		n, err := s.applyRule(out.Suggestions[i].Rule, true, nil)
		if err != nil {
			return out, err
		}
		out.Suggestions[i].Matches = n
	}
	out.Count = len(out.Suggestions)
	complete, err := s.replyHistoryComplete(ctx, accounts)
	out.HistoryComplete = complete
	return out, err
}

// DismissSuggestion marks a sender or domain never-suggest (D47).
func (s *Service) DismissSuggestion(ctx context.Context, account, target string) (map[string]any, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return nil, unavailable("the state database is not open in this mode")
	}
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" || !strings.Contains(target, "@") {
		return nil, invalid("target must be an address or @domain from suggest_rules")
	}
	account = s.accountName(account)
	if _, err := s.db.StateSQL().ExecContext(ctx, `INSERT INTO learn_state(account, target, status, updated_at) VALUES(?,?,?,unixepoch())
		ON CONFLICT(account, target) DO UPDATE SET status = excluded.status, updated_at = excluded.updated_at`,
		account, target, learnStatusDismissed); err != nil {
		return nil, err
	}
	return map[string]any{"account": account, "target": target, "dismissed": true}, nil
}

// learnRun is the hourly pass (a full run-rules): new suggestions are
// recorded, or turned into rules in the inactive and active modes, and
// published as rule.suggested with full details (D46).
func (s *Service) learnRun(ctx context.Context, now time.Time) error {
	if !s.cfg.Intel.On() || s.db == nil || s.db.StateSQL() == nil {
		return nil
	}
	st := s.db.StateSQL()
	if _, err := st.ExecContext(ctx, `DELETE FROM rule_moves WHERE moved_at < ?`, now.Add(-ruleMovesRetention).Unix()); err != nil {
		return err
	}
	mode := s.cfg.Rules.Learn.ModeOrDefault()
	var errs []string
	for _, a := range s.cfg.Accounts {
		list, err := s.suggestions(ctx, a.Name, true)
		if err != nil {
			errs = append(errs, a.Name+": "+err.Error())
			continue
		}
		var fresh []Suggestion
		current := map[string]bool{}
		suggested, created := 0, 0
		for _, sg := range list {
			current[sg.Target] = true
			if sg.Status != "new" {
				continue
			}
			status := learnStatusSuggested
			if mode != config.LearnSuggest {
				r := sg.Rule
				r.Active = mode == config.LearnActive
				id, err := s.CreateRule(ctx, &r)
				if err != nil {
					errs = append(errs, a.Name+": create rule for "+sg.Target+": "+err.Error())
					continue
				}
				status, sg.RuleID, sg.Rule.ID, sg.Rule.Active = learnStatusCreated, id, id, r.Active
				created++
			} else {
				suggested++
			}
			sg.Status = status
			if _, err := st.ExecContext(ctx, `INSERT INTO learn_state(account, target, status, rule_id, updated_at) VALUES(?,?,?,?,?)
				ON CONFLICT(account, target) DO UPDATE SET status = excluded.status, rule_id = excluded.rule_id, updated_at = excluded.updated_at`,
				a.Name, sg.Target, status, nullID(sg.RuleID), now.Unix()); err != nil {
				return err
			}
			fresh = append(fresh, sg)
		}
		// A suggestion that no longer qualifies is forgotten, so it is
		// announced again if it comes back.
		rows, err := st.QueryContext(ctx, `SELECT target FROM learn_state WHERE account = ? AND status = ?`, a.Name, learnStatusSuggested)
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var t string
			if rows.Scan(&t) == nil && !current[t] {
				stale = append(stale, t)
			}
		}
		rows.Close()
		for _, t := range stale {
			if _, err := st.ExecContext(ctx, `DELETE FROM learn_state WHERE account = ? AND target = ? AND status = ?`, a.Name, t, learnStatusSuggested); err != nil {
				return err
			}
		}
		if len(fresh) > 0 {
			if b := s.pool.Bus(); b != nil {
				b.Publish(bus.Event{Type: bus.EventRuleSuggested, Account: a.Name, Payload: map[string]any{
					"account": a.Name, "mode": mode, "suggested": suggested, "created": created, "suggestions": fresh}})
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("learn: %s", strings.Join(errs, "; "))
	}
	return nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// forgetLearnedRule makes a deleted learned rule a dismissal, so it is
// never re-created (D47).
func (s *Service) forgetLearnedRule(id int64) {
	if s.db == nil || s.db.StateSQL() == nil {
		return
	}
	_, _ = s.db.StateSQL().Exec(`UPDATE learn_state SET status = ?, updated_at = unixepoch() WHERE rule_id = ?`, learnStatusDismissed, id)
}
