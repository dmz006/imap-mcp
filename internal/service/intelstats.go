package service

import (
	"context"
	"time"
)

// IntelStats is the header scanner's progress and the sender role breakdown
// (D19, D20), read from imap.db.
type IntelStats struct {
	Enabled          bool           `json:"enabled"`
	Folders          int            `json:"folders"`
	FoldersComplete  int            `json:"folders_complete"`
	BackfillComplete bool           `json:"backfill_complete"`
	MessagesIndexed  int64          `json:"messages_indexed"`
	Senders          int            `json:"senders"`
	Roles            map[string]int `json:"roles"`
	RepliesPaired    int64          `json:"replies_paired"`
	LastScan         string         `json:"last_scan,omitempty"`
}

// IntelStats reports scan progress. Counts only: no addresses.
func (s *Service) IntelStats(ctx context.Context) (IntelStats, error) {
	st := IntelStats{Enabled: s.cfg.Intel.On(), Roles: map[string]int{}}
	if s.db == nil || s.db.StateSQL() == nil {
		return st, unavailable("the state database is not open in this mode")
	}
	db := s.db.StateSQL()
	var last int64
	if err := db.QueryRowContext(ctx, `SELECT count(*), count(completed_at), COALESCE(max(updated_at),0) FROM intel_scan`).
		Scan(&st.Folders, &st.FoldersComplete, &last); err != nil {
		return st, err
	}
	st.BackfillComplete = st.Folders > 0 && st.Folders == st.FoldersComplete
	if last > 0 {
		st.LastScan = time.Unix(last, 0).UTC().Format(time.RFC3339)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(paired),0) FROM intel_messages`).Scan(&st.MessagesIndexed, &st.RepliesPaired); err != nil {
		return st, err
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(role,'unknown'), count(*) FROM senders GROUP BY 1`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		var n int
		if err := rows.Scan(&role, &n); err != nil {
			return st, err
		}
		st.Roles[role] = n
		st.Senders += n
	}
	return st, rows.Err()
}
