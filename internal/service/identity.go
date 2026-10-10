package service

import (
	"context"
	"strings"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/intel"
)

// The owner's other addresses (AGENT.md D50): logins, identity.also_me and
// confirmed identities all count as the owner. Detected candidates wait for
// confirm_identity or reject_identity.

// meSet holds the owner's addresses and "@domain" entries.
type meSet map[string]bool

func (m meSet) has(addr string) bool {
	addr = strings.ToLower(addr)
	if m[addr] {
		return true
	}
	d := domainOf(addr)
	return d != "" && m["@"+d]
}

// me returns every address and @domain that is the owner's, read now.
func (s *Service) me(ctx context.Context) meSet {
	m := meSet{}
	for _, a := range s.cfg.Accounts {
		for _, addr := range []string{a.Auth.Username, smtpFromOf(a.SMTP)} {
			if addr = bareAddr(addr); strings.Contains(addr, "@") {
				m[addr] = true
			}
		}
	}
	for _, e := range s.cfg.Identity.AlsoMe {
		if e = intel.NormalizeIdentity(e); e != "" {
			m[e] = true
		}
	}
	if s.db == nil || s.db.StateSQL() == nil {
		return m
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT address FROM identities WHERE status IN ('confirmed','config')`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			m[a] = true
		}
	}
	return m
}

// Identity is one candidate or known identity.
type Identity struct {
	Address  string `json:"address"`
	Status   string `json:"status"` // candidate | confirmed | rejected | config
	Evidence string `json:"evidence,omitempty"`
	Name     string `json:"name,omitempty"`
	Received int    `json:"received"`
	Sent     int    `json:"sent"`
}

// IdentityList is a suggest_identities result.
type IdentityList struct {
	Count      int        `json:"count"`
	Candidates []Identity `json:"candidates"`
	Known      []Identity `json:"known"` // config and confirmed entries
}

// SuggestIdentities lists addresses that look like the owner's, with the
// evidence, and the ones already known to be.
func (s *Service) SuggestIdentities(ctx context.Context) (IdentityList, error) {
	out := IdentityList{Candidates: []Identity{}, Known: []Identity{}}
	if s.db == nil || s.db.StateSQL() == nil {
		return out, unavailable("the state database is not open in this mode")
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT i.address, i.status, COALESCE(i.evidence,''), COALESCE(s.name,''),
			COALESCE(s.message_count,0), COALESCE(s.sent_count,0)
		FROM identities i LEFT JOIN senders s ON s.address = i.address
		WHERE i.status <> 'rejected' ORDER BY i.status, i.address`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.Address, &id.Status, &id.Evidence, &id.Name, &id.Received, &id.Sent); err != nil {
			return out, err
		}
		if id.Status == intel.IdentityCandidate {
			out.Candidates = append(out.Candidates, id)
		} else {
			out.Known = append(out.Known, id)
		}
	}
	out.Count = len(out.Candidates)
	return out, rows.Err()
}

// ConfirmIdentity records that an address (or @domain) is the owner's and
// rewrites the stored history for it (D50).
func (s *Service) ConfirmIdentity(ctx context.Context, address string) (map[string]any, error) {
	return s.setIdentity(ctx, address, intel.IdentityConfirmed)
}

// RejectIdentity records that a candidate is not the owner; it is never
// suggested again.
func (s *Service) RejectIdentity(ctx context.Context, address string) (map[string]any, error) {
	return s.setIdentity(ctx, address, intel.IdentityRejected)
}

func (s *Service) setIdentity(ctx context.Context, address, status string) (map[string]any, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return nil, unavailable("the state database is not open in this mode")
	}
	addr := intel.NormalizeIdentity(address)
	if addr == "" {
		return nil, invalid("address must be an email address or @domain")
	}
	st := s.db.StateSQL()
	if _, err := st.ExecContext(ctx, `INSERT INTO identities(address, status, updated_at) VALUES(?,?,unixepoch())
		ON CONFLICT(address) DO UPDATE SET status = excluded.status, updated_at = unixepoch()`, addr, status); err != nil {
		return nil, err
	}
	if status == intel.IdentityConfirmed {
		if err := intel.ApplyIdentity(ctx, st, addr); err != nil {
			return nil, err
		}
	}
	return map[string]any{"address": addr, "status": status}, nil
}

func smtpFromOf(c *config.SMTPConfig) string {
	if c == nil {
		return ""
	}
	return c.From
}
