package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/db"
)

// ValidateRule checks a rule before it is stored.
func ValidateRule(r *db.Rule) error {
	if strings.TrimSpace(r.Name) == "" {
		return invalid("name is required")
	}
	if len(r.Actions) == 0 {
		return invalid("an action (trash|move|flag|seen) is required")
	}
	for _, a := range r.Actions {
		switch a.Type {
		case "trash", "seen":
		case "move":
			if a.Dest == "" {
				return invalid("action=move requires dest")
			}
		case "flag":
			if a.Flags == "" {
				return invalid("action=flag requires flags")
			}
		default:
			return invalid("unknown action %q (trash|move|flag|seen)", a.Type)
		}
	}
	c := r.Conditions
	if c.From == "" && c.Subject == "" && c.Text == "" && c.OlderThanDays == 0 && !c.NewSender {
		return invalid("a rule needs at least one condition (from, subject, text, older_than_days, or new_sender)")
	}
	if c.NewSenderDays < 0 || (c.NewSenderDays > 0 && !c.NewSender) {
		return invalid("new_sender_days needs new_sender and must be positive")
	}
	return nil
}

// ListRules returns all rules.
func (s *Service) ListRules(ctx context.Context) ([]db.Rule, error) {
	rules, err := s.db.Rules.List()
	if err != nil {
		return nil, err
	}
	if rules == nil {
		rules = []db.Rule{}
	}
	return rules, nil
}

// GetRule returns one rule.
func (s *Service) GetRule(ctx context.Context, id int64) (*db.Rule, error) {
	r, err := s.db.Rules.Get(id)
	if errors.Is(err, db.ErrRuleNotFound) {
		return nil, notFound("rule %d not found", id)
	}
	return r, err
}

// CreateRule validates and stores a rule, returning its id.
func (s *Service) CreateRule(ctx context.Context, r *db.Rule) (int64, error) {
	if err := ValidateRule(r); err != nil {
		return 0, err
	}
	id, err := s.db.Rules.Create(r)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return 0, invalid("a rule named %q already exists", r.Name)
	}
	return id, err
}

// UpdateRule validates and replaces a rule.
func (s *Service) UpdateRule(ctx context.Context, r *db.Rule) error {
	if err := ValidateRule(r); err != nil {
		return err
	}
	err := s.db.Rules.Update(r)
	switch {
	case errors.Is(err, db.ErrRuleNotFound):
		return notFound("rule %d not found", r.ID)
	case err != nil && strings.Contains(err.Error(), "UNIQUE"):
		return invalid("a rule named %q already exists", r.Name)
	}
	return err
}

// DeleteRule removes a rule.
func (s *Service) DeleteRule(ctx context.Context, id int64) error {
	if id == 0 {
		return invalid("id is required")
	}
	if err := s.db.Rules.Delete(id); err != nil {
		return notFound("%v", err)
	}
	return nil
}

// RuleRunResult is the outcome of applying one rule.
type RuleRunResult struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Matched int    `json:"matched"`
	Action  string `json:"action"`
	Error   string `json:"error,omitempty"`
}

// TestRule dry-runs one rule (active or not): counts matches, changes nothing.
func (s *Service) TestRule(ctx context.Context, id int64) (RuleRunResult, error) {
	r, err := s.GetRule(ctx, id)
	if err != nil {
		return RuleRunResult{}, err
	}
	res := RuleRunResult{ID: r.ID, Name: r.Name}
	if len(r.Actions) > 0 {
		res.Action = r.Actions[0].Type
	}
	n, err := s.applyRule(*r, true)
	res.Matched = n
	if err != nil {
		res.Error = err.Error()
	}
	return res, nil
}

// RunActiveRules applies all active rules (or one by id) and returns per-rule
// results. Used by run_rules and the run-rules CLI.
func (s *Service) RunActiveRules(onlyID int64, dryRun bool) ([]RuleRunResult, error) {
	rules, err := s.db.Rules.List()
	if err != nil {
		return nil, err
	}
	var results []RuleRunResult
	for _, rule := range rules {
		if !rule.Active && onlyID == 0 {
			continue
		}
		if onlyID != 0 && rule.ID != onlyID {
			continue
		}
		res := RuleRunResult{ID: rule.ID, Name: rule.Name}
		if len(rule.Actions) > 0 {
			res.Action = rule.Actions[0].Type
		}
		n, aerr := s.applyRule(rule, dryRun)
		res.Matched = n
		if aerr != nil {
			res.Error = aerr.Error()
		} else if !dryRun && n > 0 {
			_ = s.db.Rules.IncrementRun(rule.ID, n)
			// Synchronous so the run-rules CLI enqueues webhooks before exit.
			if b := s.pool.Bus(); b != nil {
				b.Publish(bus.Event{Type: bus.EventRuleFired, Account: rule.Conditions.Account,
					Payload: map[string]any{"rule_id": rule.ID, "action": res.Action, "matched": n}})
			}
		}
		results = append(results, res)
	}
	if !dryRun && onlyID == 0 {
		// A full run is the hourly job: send the day's held-mail digest when due.
		if err := s.sendHoldDigests(time.Now()); err != nil {
			results = append(results, RuleRunResult{Name: "hold-digest", Error: err.Error()})
		}
	}
	return results, nil
}

// applyRule searches a rule's folder by its conditions and applies its actions.
// Returns the number of matched messages. With dryRun it only counts.
func (s *Service) applyRule(rule db.Rule, dryRun bool) (int, error) {
	folder := rule.Conditions.Folder
	if folder == "" {
		folder = "INBOX"
	}
	conn, err := s.pool.Resolve(rule.Conditions.Account)
	if err != nil {
		return 0, err
	}
	conn.Lock()
	defer conn.Unlock()
	client := conn.Client()

	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return 0, fmt.Errorf("select %s: %w", folder, err)
	}

	criteria := &imaplib.SearchCriteria{}
	c := rule.Conditions
	if c.From != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "From", Value: c.From})
	}
	if c.Subject != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "Subject", Value: c.Subject})
	}
	if c.Text != "" {
		criteria.Body = append(criteria.Body, c.Text)
	}
	if c.OlderThanDays > 0 {
		criteria.Before = time.Now().AddDate(0, 0, -c.OlderThanDays)
	}
	var gate *newSenderGate
	if c.NewSender {
		if gate, err = s.newSenderGate(conn.Account(), c.NewSenderDays); err != nil {
			return 0, err
		}
		// A new sender's mail is all inside the window.
		criteria.Since = time.Unix(gate.cutoff, 0)
	}

	sd, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return 0, fmt.Errorf("search: %w", err)
	}
	uids := sd.AllUIDs()
	var held map[imaplib.UID]heldInfo
	if gate != nil {
		if uids, held, err = gate.filter(client, uids, dryRun); err != nil {
			return 0, err
		}
	}
	if len(uids) == 0 || dryRun {
		return len(uids), nil
	}

	set := imaplib.UIDSetNum(uids...)
	for _, act := range rule.Actions {
		switch act.Type {
		case "trash":
			trash, err := ResolveTrash(client)
			if err != nil {
				return len(uids), fmt.Errorf("resolve trash: %w", err)
			}
			if err := MoveUIDs(client, set, trash); err != nil {
				return len(uids), fmt.Errorf("move to trash: %w", err)
			}
		case "move":
			if err := MoveUIDs(client, set, act.Dest); err != nil {
				return len(uids), fmt.Errorf("move to %s: %w", act.Dest, err)
			}
		case "flag":
			if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: ParseFlags(act.Flags)}, nil).Close(); err != nil {
				return len(uids), fmt.Errorf("flag: %w", err)
			}
		case "seen":
			if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagSeen}}, nil).Close(); err != nil {
				return len(uids), fmt.Errorf("mark seen: %w", err)
			}
		default:
			return len(uids), fmt.Errorf("unknown action %q", act.Type)
		}
	}
	if gate != nil {
		dest := rule.Actions[0].Dest
		if rule.Actions[0].Type == "trash" {
			dest = "Trash"
		}
		if err := gate.record(held, dest); err != nil {
			return len(uids), fmt.Errorf("record held mail: %w", err)
		}
	}
	return len(uids), nil
}

// ResolveTrash finds the account's Trash folder: the \Trash SPECIAL-USE
// mailbox, else a conventional name ([Gmail]/Trash, Trash, */Trash).
func ResolveTrash(client *imapclient.Client) (string, error) {
	boxes, err := client.List("", "*", &imaplib.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return "", err
	}
	var nameMatch string
	for _, b := range boxes {
		for _, attr := range b.Attrs {
			if attr == imaplib.MailboxAttrTrash {
				return b.Mailbox, nil
			}
		}
		n := b.Mailbox
		if n == "[Gmail]/Trash" || n == "Trash" || strings.HasSuffix(n, "/Trash") {
			nameMatch = n
		}
	}
	if nameMatch != "" {
		return nameMatch, nil
	}
	return "", fmt.Errorf("no \\Trash mailbox found")
}
