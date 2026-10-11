// Package rulepacks holds the starter rule packs (AGENT.md D52): generic,
// shareable rule templates compiled into the binary. Importing a pack
// creates its rules inactive for one account, so each can be previewed with
// a dry run before it is turned on.
package rulepacks

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dmz006/imap-mcp/internal/db"
)

//go:embed packs/*.yaml
var files embed.FS

// Pack is one rule pack.
type Pack struct {
	Name        string     `yaml:"name" json:"name"`
	Description string     `yaml:"description" json:"description"`
	Dest        string     `yaml:"dest,omitempty" json:"dest,omitempty"` // default folder for move rules
	Rules       []PackRule `yaml:"rules" json:"rules"`
}

// PackRule is one rule in a pack.
type PackRule struct {
	Name          string `yaml:"name" json:"name"`
	Description   string `yaml:"description,omitempty" json:"description,omitempty"`
	From          string `yaml:"from,omitempty" json:"from,omitempty"`
	Subject       string `yaml:"subject,omitempty" json:"subject,omitempty"`
	OlderThanDays int    `yaml:"older_than_days,omitempty" json:"older_than_days,omitempty"`
	Action        string `yaml:"action" json:"action"` // trash | move
}

// All returns every pack, by name.
func All() ([]Pack, error) {
	entries, err := fs.ReadDir(files, "packs")
	if err != nil {
		return nil, err
	}
	var out []Pack
	for _, e := range entries {
		b, err := files.ReadFile("packs/" + e.Name())
		if err != nil {
			return nil, err
		}
		var p Pack
		if err := yaml.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("rule pack %s: %w", e.Name(), err)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one pack by name.
func Get(name string) (Pack, error) {
	all, err := All()
	if err != nil {
		return Pack{}, err
	}
	for _, p := range all {
		if strings.EqualFold(p.Name, name) {
			return p, nil
		}
	}
	return Pack{}, fmt.Errorf("no rule pack %q", name)
}

// Build builds the pack's rules for an account, inactive. dest overrides the
// pack's default folder for move rules.
func (p Pack) Build(account, dest string) []db.Rule {
	if dest == "" {
		dest = p.Dest
	}
	out := make([]db.Rule, 0, len(p.Rules))
	for _, r := range p.Rules {
		act := db.RuleAction{Type: r.Action}
		if r.Action == "move" {
			act.Dest = dest
		}
		desc := p.Description
		if r.Description != "" {
			desc = r.Description
		}
		out = append(out, db.Rule{
			Name:        RuleName(p.Name, r.Name, account),
			Description: "Rule pack " + p.Name + ": " + desc,
			Conditions:  db.RuleConditions{Account: account, From: r.From, Subject: r.Subject, OlderThanDays: r.OlderThanDays},
			Actions:     []db.RuleAction{act},
			Active:      false,
		})
	}
	return out
}

// RuleName is the unique name an imported pack rule gets.
func RuleName(pack, rule, account string) string {
	return "pack:" + pack + "/" + rule + " (" + account + ")"
}
