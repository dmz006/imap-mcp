// Package intel builds sender intelligence (AGENT.md D19, D20, D28). A
// header-only scan of every folder — never bodies, never \Seen — fills sender
// profiles in imap.db: first and last contact, messages each way, list, bulk
// and auto-submitted counts, DKIM/DMARC results and reply times. The cache's
// enrichment tags and the classify model then assign each sender a role.
//
// The scan is resumable and rate-limited: the first pass walks all history,
// later ticks only read new UIDs. A per-message hash index (D28) keeps a
// message counted once across folders, labels and rescans.
package intel

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
)

// ClassifyFunc runs a classify prompt. It returns ErrGated when the model
// should not take optional work now (see enrichment.Pipeline.ClassifyWhenIdle).
type ClassifyFunc func(ctx context.Context, prompt string) (string, error)

// Scanner is the header scanner and role builder.
type Scanner struct {
	cfg        config.IntelConfig
	src        Source
	state      *sql.DB // imap.db
	cache      *sql.DB // cache.db; nil disables hall roles and the model pass
	st         *store
	classify   ClassifyFunc
	own        map[string]map[string]bool // account → own addresses
	ownDomains map[string]bool
	log        *slog.Logger
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration)
}

// New builds a scanner. classify may be nil (no model-assigned roles).
func New(cfg *config.Config, src Source, state, cache *sql.DB, classify ClassifyFunc, log *slog.Logger) *Scanner {
	sc := &Scanner{
		cfg: cfg.Intel, src: src, state: state, cache: cache, st: &store{db: state},
		own: map[string]map[string]bool{}, ownDomains: map[string]bool{},
		log: log, now: time.Now, sleep: sleepCtx,
	}
	if cfg.Intel.LLMRolesOn() {
		sc.classify = classify
	}
	for _, a := range cfg.Accounts {
		set := map[string]bool{}
		for _, addr := range []string{a.Auth.Username, smtpFrom(a)} {
			addr = strings.ToLower(strings.TrimSpace(addr))
			if i := strings.LastIndexByte(addr, '<'); i >= 0 { // "Name <a@b>"
				addr = strings.TrimSuffix(addr[i+1:], ">")
			}
			if strings.Contains(addr, "@") {
				set[addr] = true
				sc.ownDomains[domainOf(addr)] = true
			}
		}
		sc.own[a.Name] = set
	}
	return sc
}

func smtpFrom(a config.AccountConfig) string {
	if a.SMTP != nil {
		return a.SMTP.From
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Run scans immediately, then every scan_interval_minutes, until ctx ends.
func (sc *Scanner) Run(ctx context.Context) {
	if !sc.cfg.On() {
		return
	}
	interval := time.Duration(max(sc.cfg.ScanIntervalMinutes, 1)) * time.Minute
	for {
		if err := sc.Tick(ctx); err != nil && ctx.Err() == nil {
			sc.log.Warn("intel: scan tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// TickResult summarises one tick (for logs and tests).
type TickResult struct {
	Scanned, New, Duplicate, Paired, Roles, ModelRoles int
}

// Tick scans every account's folders for new UIDs, pairs replies, recomputes
// roles for changed senders and asks the model about a few unknown ones.
func (sc *Scanner) Tick(ctx context.Context) error {
	var res TickResult
	start := sc.now()
	for _, account := range sc.src.Accounts() {
		if err := sc.scanAccount(ctx, account, &res); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sc.log.Warn("intel: scan account", "account", account, "err", err)
			continue
		}
		n, err := sc.st.pairReplies(ctx, account)
		if err != nil {
			return err
		}
		res.Paired += n
	}
	n, err := sc.recomputeRoles(ctx)
	if err != nil {
		return err
	}
	res.Roles = n
	if res.ModelRoles, err = sc.modelRoles(ctx, sc.cfg.LLMRolesPerTick); err != nil {
		return err
	}
	if res.Scanned > 0 || res.Roles > 0 || res.ModelRoles > 0 {
		sc.log.Info("intel: scan tick", "scanned", res.Scanned, "new", res.New, "duplicates", res.Duplicate,
			"replies_paired", res.Paired, "roles", res.Roles, "model_roles", res.ModelRoles,
			"took", sc.now().Sub(start).Round(time.Millisecond))
	}
	return nil
}

func (sc *Scanner) scanAccount(ctx context.Context, account string, res *TickResult) error {
	all, err := sc.src.Folders(ctx, account)
	if err != nil {
		return err
	}
	for _, folder := range sc.scope(all) {
		if err := sc.scanFolder(ctx, account, folder, res); err != nil {
			if ctx.Err() != nil {
				return err
			}
			sc.log.Warn("intel: scan folder", "account", account, "folder", folder, "err", err)
		}
	}
	return nil
}

// scope is the folders to scan: the \All mailbox when there is one (Gmail's
// All Mail holds every message once), else every selectable folder except
// \Junk, \Drafts and intelligence.exclude_folders.
func (sc *Scanner) scope(all []Folder) []string {
	var out []string
	for _, f := range all {
		if slices.Contains(f.Attrs, `\All`) && !slices.Contains(f.Attrs, `\Noselect`) {
			return []string{f.Name}
		}
	}
	for _, f := range all {
		skip := slices.ContainsFunc(f.Attrs, func(a string) bool {
			return a == `\Noselect` || a == `\NonExistent` || a == `\Junk` || a == `\Drafts`
		})
		if skip || slices.ContainsFunc(sc.cfg.ExcludeFolders, func(x string) bool { return strings.EqualFold(x, f.Name) }) {
			continue
		}
		out = append(out, f.Name)
	}
	return out
}

func (sc *Scanner) scanFolder(ctx context.Context, account, folder string, res *TickResult) error {
	st, err := sc.st.state(ctx, account, folder)
	if err != nil {
		return err
	}
	own := sc.own[account]
	batch := max(sc.cfg.BatchSize, 1)
	after := st.LastUID
	for ctx.Err() == nil {
		b, err := sc.src.Fetch(ctx, account, folder, after, batch)
		if err != nil {
			return err
		}
		if st.UIDValidity != 0 && b.UIDValidity != st.UIDValidity {
			sc.log.Info("intel: UIDVALIDITY changed, rescanning folder", "account", account, "folder", folder)
			if err := sc.st.resetFolder(ctx, account, folder, b.UIDValidity); err != nil {
				return err
			}
			st.UIDValidity, after = b.UIDValidity, 0
			continue
		}
		st.UIDValidity = b.UIDValidity
		a, err := sc.st.apply(ctx, account, folder, b.UIDValidity, b, own)
		if err != nil {
			return err
		}
		res.Scanned += len(b.Headers)
		res.New += a.New
		res.Duplicate += a.Duplicate
		if len(b.Headers) > 0 {
			after = b.Headers[len(b.Headers)-1].UID
		}
		if !b.More {
			return nil
		}
		// Rate limit the first full scan: backfill_per_minute across the tick.
		sc.sleep(ctx, time.Duration(len(b.Headers))*time.Minute/time.Duration(max(sc.cfg.BackfillPerMinute, 1)))
	}
	return ctx.Err()
}
