package service

import (
	"context"

	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/sync"
)

// AccountStats summarises one account's cache, last sync and enrichment.
type AccountStats struct {
	Account    string             `json:"account"`
	Connected  bool               `json:"connected"`
	Cached     []db.FolderCount   `json:"cached"`
	Sync       []sync.FolderStats `json:"sync,omitempty"`
	Enrichment map[string]int     `json:"enrichment"` // status → count for this account
}

// AccountStats reports cache counts, last sync results and enrichment states.
func (s *Service) AccountStats(ctx context.Context, account string) (AccountStats, error) {
	a := s.cfg.DefaultAccount()
	if account != "" && account != "_default" {
		var err error
		if a, err = s.cfg.Account(account); err != nil {
			return AccountStats{}, notFound("account %q not found", account)
		}
	}
	st := AccountStats{Account: a.Name, Connected: s.pool.Probe(a.Name), Cached: []db.FolderCount{}, Enrichment: map[string]int{}}
	if s.db.SQL() == nil {
		return st, nil
	}
	counts, err := s.db.Messages.Counts(ctx)
	if err != nil {
		return st, err
	}
	for _, c := range counts {
		if c.Account == a.Name {
			st.Cached = append(st.Cached, c)
		}
	}
	if s.syncer != nil {
		for _, f := range s.syncer.Stats() {
			if f.Account == a.Name {
				st.Sync = append(st.Sync, f)
			}
		}
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT COALESCE(enrichment_status, 'pending'), count(*) FROM messages WHERE account=? GROUP BY 1`, a.Name)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err == nil {
			st.Enrichment[k] = n
		}
	}
	return st, rows.Err()
}

// SyncAccount syncs one account now and returns its folder results.
func (s *Service) SyncAccount(ctx context.Context, account string) ([]sync.FolderStats, error) {
	if s.syncer == nil {
		return nil, unavailable("cache sync is not running in this mode")
	}
	a := s.cfg.DefaultAccount()
	if account != "" && account != "_default" {
		var err error
		if a, err = s.cfg.Account(account); err != nil {
			return nil, notFound("account %q not found", account)
		}
	}
	if err := s.syncer.SyncAccount(ctx, a.Name); err != nil {
		return nil, upstream("sync", err)
	}
	out := []sync.FolderStats{}
	for _, f := range s.syncer.Stats() {
		if f.Account == a.Name {
			out = append(out, f)
		}
	}
	return out, nil
}

// SweepCache is the on-demand cache_sweep. A filter (or All) is required.
func (s *Service) SweepCache(ctx context.Context, f db.SweepFilter, dryRun bool) (sync.SweepResult, error) {
	if s.syncer == nil {
		return sync.SweepResult{}, unavailable("cache sync is not running in this mode")
	}
	if f.Empty() && !f.All {
		return sync.SweepResult{}, invalid("refusing to sweep without a filter: set account, folder, older_than_days, errors_only, or all=true")
	}
	return s.syncer.Sweep(ctx, f, dryRun)
}

// EnrichmentStats reports the enrichment queue and load state.
func (s *Service) EnrichmentStats(ctx context.Context) (enrichment.Stats, error) {
	if s.pipeline == nil {
		return enrichment.Stats{}, unavailable("enrichment pipeline is not running in this mode")
	}
	return s.pipeline.Stats(ctx)
}

// TriggerEnrichment queues an immediate run of up to limit messages.
func (s *Service) TriggerEnrichment(limit int) (bool, error) {
	if s.pipeline == nil || !s.cfg.Enrichment.Enabled {
		return false, unavailable("enrichment is not running (disabled or not in this mode)")
	}
	if limit <= 0 {
		limit = 50
	}
	return s.pipeline.Trigger(limit), nil
}
