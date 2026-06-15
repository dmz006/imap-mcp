package db

import (
	"encoding/json"
	"fmt"
)

// Rule is a persistent automation: match messages by criteria, apply actions.
type Rule struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Conditions  RuleConditions `json:"conditions"`
	Actions     []RuleAction   `json:"actions"`
	Active      bool           `json:"active"`
	Priority    int            `json:"priority"`
	RunCount    int            `json:"run_count"`
}

// RuleConditions is the match criteria (all non-empty fields are ANDed).
type RuleConditions struct {
	Account       string `json:"account,omitempty"` // empty = default account
	Folder        string `json:"folder,omitempty"`  // empty = INBOX
	From          string `json:"from,omitempty"`
	Subject       string `json:"subject,omitempty"`
	Text          string `json:"text,omitempty"`
	OlderThanDays int    `json:"older_than_days,omitempty"`
}

// RuleAction is what to do with matched messages.
type RuleAction struct {
	Type  string `json:"type"`            // trash | move | flag | seen
	Dest  string `json:"dest,omitempty"`  // for move
	Flags string `json:"flags,omitempty"` // for flag (comma-separated)
}

// Create inserts a new rule and returns its ID.
func (r *RuleRepo) Create(rule *Rule) (int64, error) {
	cond, err := json.Marshal(rule.Conditions)
	if err != nil {
		return 0, err
	}
	acts, err := json.Marshal(rule.Actions)
	if err != nil {
		return 0, err
	}
	active := 1
	if !rule.Active {
		active = 0
	}
	if rule.Priority == 0 {
		rule.Priority = 100
	}
	res, err := r.db.Exec(
		`INSERT INTO rules (name, description, conditions, actions, active, priority) VALUES (?,?,?,?,?,?)`,
		rule.Name, rule.Description, string(cond), string(acts), active, rule.Priority,
	)
	if err != nil {
		return 0, fmt.Errorf("insert rule: %w", err)
	}
	return res.LastInsertId()
}

// List returns all rules ordered by priority then id.
func (r *RuleRepo) List() ([]Rule, error) {
	rows, err := r.db.Query(
		`SELECT id, name, description, conditions, actions, active, priority, run_count FROM rules ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var rule Rule
		var cond, acts string
		var active int
		if err := rows.Scan(&rule.ID, &rule.Name, &rule.Description, &cond, &acts, &active, &rule.Priority, &rule.RunCount); err != nil {
			return nil, err
		}
		rule.Active = active != 0
		_ = json.Unmarshal([]byte(cond), &rule.Conditions)
		_ = json.Unmarshal([]byte(acts), &rule.Actions)
		out = append(out, rule)
	}
	return out, rows.Err()
}

// Delete removes a rule by id.
func (r *RuleRepo) Delete(id int64) error {
	res, err := r.db.Exec(`DELETE FROM rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("rule %d not found", id)
	}
	return nil
}

// IncrementRun bumps run_count by n.
func (r *RuleRepo) IncrementRun(id int64, n int) error {
	_, err := r.db.Exec(`UPDATE rules SET run_count = run_count + ?, updated_at = unixepoch() WHERE id = ?`, n, id)
	return err
}
