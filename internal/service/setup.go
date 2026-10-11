package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/rulepacks"
)

// Sharing what tuning taught us (AGENT.md D52): starter rule packs and a
// setup check that flags the gaps production tuning found.

// ListRulePacks returns the built-in rule packs.
func (s *Service) ListRulePacks() ([]rulepacks.Pack, error) { return rulepacks.All() }

// ImportResult reports what importing a pack created.
type ImportResult struct {
	Pack    string   `json:"pack"`
	Account string   `json:"account"`
	Created []int64  `json:"created"`
	Skipped []string `json:"skipped,omitempty"` // already imported
	Note    string   `json:"note"`
}

// ImportRulePack creates a pack's rules, inactive, for one account. A move
// pack needs its folder to exist (dest overrides the pack's default).
func (s *Service) ImportRulePack(ctx context.Context, account, pack, dest string) (ImportResult, error) {
	res := ImportResult{Created: []int64{}}
	p, err := rulepacks.Get(pack)
	if err != nil {
		return res, notFound("%v", err)
	}
	account = s.accountName(account)
	res.Pack, res.Account = p.Name, account
	rules := p.Build(account, dest)
	needs := ""
	for _, r := range rules {
		if r.Actions[0].Type == "move" {
			needs = r.Actions[0].Dest
		}
	}
	if needs != "" {
		ok, err := s.folderExists(account, needs)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, invalid("folder %q does not exist in %s: create it first (create_folder) or pass dest", needs, account)
		}
	}
	existing, err := s.db.Rules.List()
	if err != nil {
		return res, err
	}
	have := map[string]bool{}
	for _, r := range existing {
		have[r.Name] = true
	}
	for i := range rules {
		if have[rules[i].Name] {
			res.Skipped = append(res.Skipped, rules[i].Name)
			continue
		}
		id, err := s.CreateRule(ctx, &rules[i])
		if err != nil {
			return res, err
		}
		res.Created = append(res.Created, id)
	}
	res.Note = "Rules are inactive: dry-run each with run_rules {id, dry_run: true}, then activate the ones you want."
	return res, nil
}

func (s *Service) folderExists(account, name string) (bool, error) {
	conn, err := s.pool.Resolve(account)
	if err != nil {
		return false, err
	}
	conn.Lock()
	defer conn.Unlock()
	boxes, err := conn.Client().List("", name, nil).Collect()
	if err != nil {
		return false, err
	}
	return len(boxes) > 0, nil
}

// Finding is one setup-check result.
type Finding struct {
	ID       string `json:"id"`
	Account  string `json:"account,omitempty"`
	Severity string `json:"severity"` // info | warn
	What     string `json:"what"`
	Why      string `json:"why"`
	Fix      string `json:"fix"`
}

// SetupReport is a setup_check result.
type SetupReport struct {
	Count    int       `json:"count"`
	Findings []Finding `json:"findings"`
}

// SetupCheck looks for the setup gaps production tuning found (D52):
// counts and settings only.
func (s *Service) SetupCheck(ctx context.Context) (SetupReport, error) {
	out := SetupReport{Findings: []Finding{}}
	if s.db == nil || s.db.StateSQL() == nil {
		return out, unavailable("the state database is not open in this mode")
	}
	add := func(f Finding) { out.Findings = append(out.Findings, f) }
	st := s.db.StateSQL()
	if !s.cfg.Intel.On() {
		add(Finding{ID: "intel_off", Severity: "warn", What: "The history scan (intelligence) is off.",
			Why: "Sender profiles, the new-sender hold, reply tracking, learned rules and identity detection all need it.",
			Fix: "Set intelligence.enabled: true (the default) and restart."})
	}
	if !s.cfg.Rules.HoldDigestOn() {
		add(Finding{ID: "digest_off", Severity: "info", What: "The daily digest is off.",
			Why: "Held mail, conversations waiting on you, rule suggestions and new setup findings are summarised there.",
			Fix: "Set rules.hold_digest: true."})
	}
	if !syncsSent(s.cfg.Sync.Folders) {
		add(Finding{ID: "sent_not_synced", Severity: "info", What: "The cache does not sync your Sent folder.",
			Why: "Semantic search and the classify model never see what you wrote; reply tracking and identity detection fall back to the history scan.",
			Fix: `Add "\Sent" to sync.folders (the default is INBOX and \Sent).`})
	}
	var n int
	if err := st.QueryRowContext(ctx, `SELECT count(*) FROM identities WHERE status = 'candidate'`).Scan(&n); err != nil {
		return out, err
	}
	if n > 0 {
		add(Finding{ID: "identities_pending", Severity: "warn", What: fmt.Sprintf("%d address(es) look like your own and wait for an answer.", n),
			Why: "Until confirmed, your other addresses count as other people: they show up as conversations you owe and can be held as new senders.",
			Fix: "Review suggest_identities, then confirm_identity or reject_identity."})
	}
	rules, err := s.db.Rules.List()
	if err != nil {
		return out, err
	}
	def := s.cfg.DefaultAccount().Name
	for _, a := range s.cfg.Accounts {
		hold, holdActive := false, false
		for _, r := range rules {
			ra := r.Conditions.Account
			if ra == "" {
				ra = def
			}
			if ra == a.Name && r.Conditions.NewSender {
				hold, holdActive = true, holdActive || r.Active
			}
		}
		switch {
		case s.cfg.Intel.On() && !hold:
			add(Finding{ID: "no_hold_rule", Account: a.Name, Severity: "info", What: "No new-sender hold rule.",
				Why: "Snowshoe spam uses a new domain for almost every message; sender and domain rules can't keep up.",
				Fix: "Create a new_sender rule (inactive), preview it with run_rules dry_run, then activate it (docs/examples.md §14)."})
		case hold && !holdActive:
			add(Finding{ID: "hold_inactive", Account: a.Name, Severity: "info", What: "The new-sender hold rule is inactive.",
				Why: "It holds nothing until it is turned on.",
				Fix: "Dry-run it to review who it would hold, then set active: true."})
		}
		if s.cfg.Intel.On() {
			complete, err := s.replyHistoryComplete(ctx, []string{a.Name})
			if err != nil {
				return out, err
			}
			if !complete {
				add(Finding{ID: "history_incomplete", Account: a.Name, Severity: "info", What: "The history scan has not finished.",
					Why: "Reply lists, rule suggestions and the hold see only part of your history until it does.",
					Fix: "Wait; /api/health shows progress (rescan_folders_remaining)."})
			}
		}
		if err := st.QueryRowContext(ctx, `SELECT count(*) FROM learn_state WHERE account = ? AND status = 'suggested'`, a.Name).Scan(&n); err != nil {
			return out, err
		}
		if n > 0 {
			add(Finding{ID: "suggestions_pending", Account: a.Name, Severity: "info", What: fmt.Sprintf("%d learned rule suggestion(s) are waiting.", n),
				Why: "They come from mail you keep moving to Trash or Junk yourself.",
				Fix: "Review suggest_rules; create the ones you want or dismiss_suggestion the rest."})
		}
	}
	idle := 0
	cutoff := time.Now().AddDate(0, 0, -30).Unix()
	createdAt := map[int64]int64{}
	rows, err := st.QueryContext(ctx, `SELECT id, COALESCE(created_at, 0) FROM rules`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id, at int64
		if rows.Scan(&id, &at) == nil {
			createdAt[id] = at
		}
	}
	rows.Close()
	for _, r := range rules {
		if r.Active && !r.Conditions.NewSender && r.RunCount == 0 && createdAt[r.ID] > 0 && createdAt[r.ID] < cutoff {
			idle++
		}
	}
	if idle > 0 {
		add(Finding{ID: "rules_never_matched", Severity: "info", What: fmt.Sprintf("%d active rule(s) have never matched in 30+ days.", idle),
			Why: "Often the From or Subject doesn't match how the server searches: Gmail matches whole words, other servers match substrings.",
			Fix: "Dry-run them (run_rules dry_run) and adjust, or delete the ones you no longer need (docs/rules.md, whole-token matching)."})
	}
	if s.cfg.Intel.On() {
		stale, err := s.NeedsReply(ctx, ReplyParams{OlderThanDays: 30, WithinDays: defaultReplyWithinDays, Limit: maxReplyLimit})
		if err == nil && stale.Count > 0 {
			add(Finding{ID: "replies_stale", Severity: "info", What: fmt.Sprintf("%d conversation(s) have waited on you for over 30 days.", stale.Count),
				Why: "Old items crowd the list and the digest.",
				Fix: "Reply, or dismiss_reply the ones you won't answer."})
		}
	}
	out.Count = len(out.Findings)
	return out, nil
}

// syncsSent reports whether sync.folders includes the Sent folder (empty
// means the default, INBOX and \Sent).
func syncsSent(folders []string) bool {
	if len(folders) == 0 {
		return true
	}
	for _, f := range folders {
		if l := strings.ToLower(f); l == `\sent` || strings.Contains(l, "sent") {
			return true
		}
	}
	return false
}

// newFindings returns the findings not yet reported in a digest; a finding
// that went away is forgotten, so it is reported again if it comes back.
// markFindings records the ones a digest showed.
func (s *Service) newFindings(ctx context.Context) ([]Finding, error) {
	rep, err := s.SetupCheck(ctx)
	if err != nil {
		return nil, err
	}
	st := s.db.StateSQL()
	current := map[string]bool{}
	var fresh []Finding
	for _, f := range rep.Findings {
		key := f.ID + "|" + f.Account
		current[key] = true
		var seen int
		if err := st.QueryRowContext(ctx, `SELECT count(*) FROM setup_findings WHERE id = ?`, key).Scan(&seen); err != nil {
			return nil, err
		}
		if seen == 0 {
			fresh = append(fresh, f)
		}
	}
	rows, err := st.QueryContext(ctx, `SELECT id FROM setup_findings`)
	if err != nil {
		return nil, err
	}
	var gone []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && !current[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		if _, err := st.ExecContext(ctx, `DELETE FROM setup_findings WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return fresh, nil
}

func (s *Service) markFindings(ctx context.Context, fs []Finding, now time.Time) error {
	for _, f := range fs {
		if _, err := s.db.StateSQL().ExecContext(ctx, `INSERT OR IGNORE INTO setup_findings(id, first_seen, digested_at) VALUES(?,?,?)`,
			f.ID+"|"+f.Account, now.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	return nil
}

