package service

import (
	"context"
	"sort"
	"strings"
	gosync "sync"
	"time"
)

// CrossSearchParams searches every account at once (AGENT.md D26).
type CrossSearchParams struct {
	From, Subject, Text string
	Since, Before       string // YYYY-MM-DD
	Limit               int    // per account (default 20, max 200)
	Live                bool   // IMAP SEARCH on each account instead of the cache
	Folder              string // live only; default INBOX
}

// CrossSearchHit is one matching message.
type CrossSearchHit struct {
	Account string `json:"account"`
	Folder  string `json:"folder"`
	UID     uint32 `json:"uid"`
	Subject string `json:"subject"`
	From    string `json:"from"`
	Date    string `json:"date"`
	// ThreadID can be passed to get_thread / export_message.
	ThreadID string `json:"thread_id,omitempty"`
	Source   string `json:"source"` // cache | live
}

// CrossSearchResult merges every account's hits, newest first.
type CrossSearchResult struct {
	Source   string           `json:"source"`
	Accounts int              `json:"accounts"`
	Count    int              `json:"count"`
	Hits     []CrossSearchHit `json:"hits"`
	// TotalMatches is per account, for live searches (IMAP gives the true count).
	TotalMatches map[string]int `json:"total_matches,omitempty"`
	Errors       []AccountError `json:"errors,omitempty"`
	Note         string         `json:"note,omitempty"`
}

// CrossAccountSearch searches the cache's full-text index across all accounts
// and cached folders, or with Live runs IMAP SEARCH on every account in
// parallel. One account failing adds an error entry; the rest still return.
func (s *Service) CrossAccountSearch(ctx context.Context, p CrossSearchParams) (CrossSearchResult, error) {
	if p.From == "" && p.Subject == "" && p.Text == "" && p.Since == "" && p.Before == "" {
		return CrossSearchResult{}, invalid("give at least one of from, subject, text, since, before")
	}
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 20
	}
	for _, d := range []struct{ v, n string }{{p.Since, "since"}, {p.Before, "before"}} {
		if d.v == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", d.v); err != nil {
			return CrossSearchResult{}, invalid("%s must be YYYY-MM-DD", d.n)
		}
	}
	accounts := s.pool.AccountNames()
	var res CrossSearchResult
	var err error
	if p.Live {
		res = s.liveCrossSearch(ctx, accounts, p)
	} else {
		res, err = s.cacheCrossSearch(ctx, p)
		if err != nil {
			return res, err
		}
		res.Note = "cache search: only mail inside the sync window; pass live: true for full history"
	}
	res.Accounts = len(accounts)
	if res.Hits == nil {
		res.Hits = []CrossSearchHit{}
	}
	sort.SliceStable(res.Hits, func(i, j int) bool { return res.Hits[i].Date > res.Hits[j].Date })
	res.Count = len(res.Hits)
	return res, nil
}

// ftsPhrase quotes free text as one FTS5 phrase, so no FTS syntax in it is
// interpreted.
func ftsPhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// likeContains builds a case-insensitive LIKE pattern with % and _ literal.
func likeContains(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(strings.ToLower(s)) + "%"
}

func (s *Service) cacheCrossSearch(ctx context.Context, p CrossSearchParams) (CrossSearchResult, error) {
	res := CrossSearchResult{Source: "cache"}
	if s.db == nil || s.db.SQL() == nil {
		return res, unavailable("the mail cache is not open in this mode; use live: true")
	}
	where, args := []string{"1=1"}, []any{}
	if p.Text != "" {
		where = append(where, "id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)")
		args = append(args, "{subject body_text} : "+ftsPhrase(p.Text))
	}
	if p.From != "" {
		where = append(where, `(lower(from_addr) LIKE ? ESCAPE '\' OR lower(COALESCE(from_name,'')) LIKE ? ESCAPE '\')`)
		args = append(args, likeContains(p.From), likeContains(p.From))
	}
	if p.Subject != "" {
		where = append(where, `lower(COALESCE(subject,'')) LIKE ? ESCAPE '\'`)
		args = append(args, likeContains(p.Subject))
	}
	if p.Since != "" {
		t, _ := time.Parse("2006-01-02", p.Since)
		where = append(where, "COALESCE(internal_date, date) >= ?")
		args = append(args, t.Unix())
	}
	if p.Before != "" {
		t, _ := time.Parse("2006-01-02", p.Before)
		where = append(where, "COALESCE(internal_date, date) < ?")
		args = append(args, t.Unix())
	}
	args = append(args, p.Limit)
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT account, folder, uid, subject, from_addr, from_name, d, tid FROM (
		SELECT account, folder, uid, COALESCE(subject,'') AS subject, from_addr, COALESCE(from_name,'') AS from_name,
			COALESCE(thread_id,'') AS tid,
			COALESCE(internal_date, date) AS d,
			ROW_NUMBER() OVER (PARTITION BY account ORDER BY COALESCE(internal_date, date) DESC) AS rn
		FROM messages WHERE `+strings.Join(where, " AND ")+`) WHERE rn <= ?`, args...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var h CrossSearchHit
		var name string
		var d int64
		if err := rows.Scan(&h.Account, &h.Folder, &h.UID, &h.Subject, &h.From, &name, &d, &h.ThreadID); err != nil {
			return res, err
		}
		if name != "" {
			h.From = name + " <" + h.From + ">"
		}
		h.Date = time.Unix(d, 0).UTC().Format(time.RFC3339)
		h.Source = "cache"
		res.Hits = append(res.Hits, h)
	}
	return res, rows.Err()
}

func (s *Service) liveCrossSearch(ctx context.Context, accounts []string, p CrossSearchParams) CrossSearchResult {
	res := CrossSearchResult{Source: "live", TotalMatches: map[string]int{}}
	folder := p.Folder
	if folder == "" {
		folder = "INBOX"
	}
	var mu gosync.Mutex
	var wg gosync.WaitGroup
	for _, acct := range accounts {
		wg.Add(1)
		go func(acct string) {
			defer wg.Done()
			r, err := s.Search(ctx, SearchParams{Account: acct, Folder: folder, From: p.From, Subject: p.Subject,
				Text: p.Text, Since: p.Since, Before: p.Before, Limit: p.Limit})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Errors = append(res.Errors, AccountError{Account: acct, Error: err.Error()})
				return
			}
			res.TotalMatches[acct] = r.TotalMatches
			for _, m := range r.Messages {
				res.Hits = append(res.Hits, CrossSearchHit{Account: acct, Folder: folder, UID: m.UID,
					Subject: m.Subject, From: m.From, Date: utcRFC3339(m.Date), ThreadID: m.ThreadID, Source: "live"})
			}
		}(acct)
	}
	wg.Wait()
	sort.Slice(res.Errors, func(i, j int) bool { return res.Errors[i].Account < res.Errors[j].Account })
	return res
}
