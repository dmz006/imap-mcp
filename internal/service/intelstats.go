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
	KGEntities       int64          `json:"kg_entities"`
	KGRelationships  int64          `json:"kg_relationships"`
	KGModelMessages  int64          `json:"kg_model_messages"` // bodies read by the extraction model
	LastScan         string         `json:"last_scan,omitempty"`
	// ReplyHistoryComplete: every account's scan, including the one-time
	// 0.16 rescan for reply tracking, has caught up (D45).
	ReplyHistoryComplete bool `json:"reply_history_complete"`
	// Accounts is the scan progress per account, in config order (D29).
	Accounts []AccountScan `json:"accounts"`
}

// AccountScan is one account's header-scan progress.
type AccountScan struct {
	Index            int    `json:"index"`             // 1-based, config order
	Account          string `json:"account,omitempty"` // empty in /api/health (D29)
	Folders          int    `json:"folders"`
	FoldersComplete  int    `json:"folders_complete"`
	Scanned          int64  `json:"scanned"` // headers read in the current pass
	BackfillComplete bool   `json:"backfill_complete"`
	// RescanComplete and RescanFoldersRemaining track the one-time 0.16
	// rescan that fills reply tracking (D45).
	RescanComplete         bool   `json:"rescan_complete"`
	RescanFoldersRemaining int    `json:"rescan_folders_remaining"`
	LastScan               string `json:"last_scan,omitempty"`
}

// Anonymous returns a copy without account names, for unauthenticated
// endpoints (D29).
func (st IntelStats) Anonymous() IntelStats {
	accts := make([]AccountScan, len(st.Accounts))
	for i, a := range st.Accounts {
		a.Account = ""
		accts[i] = a
	}
	st.Accounts = accts
	return st
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
	if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(paired),0), COALESCE(sum(kg_llm_done),0) FROM intel_messages`).
		Scan(&st.MessagesIndexed, &st.RepliesPaired, &st.KGModelMessages); err != nil {
		return st, err
	}
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM kg_entities), (SELECT count(*) FROM kg_relationships)`).
		Scan(&st.KGEntities, &st.KGRelationships); err != nil {
		return st, err
	}
	st.Accounts = []AccountScan{}
	for i, a := range s.cfg.Accounts {
		as := AccountScan{Index: i + 1, Account: a.Name}
		var last int64
		if err := db.QueryRowContext(ctx, `SELECT count(*), count(completed_at), COALESCE(sum(scanned),0), COALESCE(max(updated_at),0),
				count(*) FILTER (WHERE completed_at IS NULL OR last_uid < COALESCE(rescan_until, 0))
			FROM intel_scan WHERE account = ?`, a.Name).Scan(&as.Folders, &as.FoldersComplete, &as.Scanned, &last, &as.RescanFoldersRemaining); err != nil {
			return st, err
		}
		as.BackfillComplete = as.Folders > 0 && as.Folders == as.FoldersComplete
		as.RescanComplete = as.Folders > 0 && as.RescanFoldersRemaining == 0
		if last > 0 {
			as.LastScan = time.Unix(last, 0).UTC().Format(time.RFC3339)
		}
		st.Accounts = append(st.Accounts, as)
	}
	st.ReplyHistoryComplete = len(st.Accounts) > 0
	for _, a := range st.Accounts {
		st.ReplyHistoryComplete = st.ReplyHistoryComplete && a.RescanComplete
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
