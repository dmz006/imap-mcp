package inbound

import (
	"context"
	"log/slog"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/imap"
	imaplib "github.com/emersion/go-imap/v2"
)

// Watcher polls each inbound-enabled account's watch folder for new mail,
// evaluates it against the account's trust gates, and lets the Processor emit
// verified-command / rejected events. It only marks messages handled when they
// actually carried a command envelope, so ordinary mail is left untouched.
type Watcher struct {
	cfg  *config.Config
	pool *imap.Pool
	proc *Processor
	log  *slog.Logger
	tick time.Duration
}

func NewWatcher(cfg *config.Config, pool *imap.Pool, proc *Processor, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{cfg: cfg, pool: pool, proc: proc, log: log, tick: 60 * time.Second}
}

// Accounts returns the names of accounts with an enabled inbound channel.
func (w *Watcher) Accounts() []string {
	var names []string
	for i := range w.cfg.Accounts {
		if in := w.cfg.Accounts[i].Inbound; in != nil && in.Enabled {
			names = append(names, w.cfg.Accounts[i].Name)
		}
	}
	return names
}

// Run polls until ctx is cancelled. It is a no-op if no account has inbound
// enabled, so it is always safe to start.
func (w *Watcher) Run(ctx context.Context) {
	accts := w.Accounts()
	if len(accts) == 0 {
		w.log.Debug("inbound watcher: no inbound-enabled accounts; not starting")
		return
	}
	w.log.Info("inbound watcher started", "accounts", accts, "interval", w.tick.String())
	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		w.pollAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Watcher) pollAll(ctx context.Context) {
	for i := range w.cfg.Accounts {
		acct := &w.cfg.Accounts[i]
		if acct.Inbound == nil || !acct.Inbound.Enabled {
			continue
		}
		if err := w.pollAccount(ctx, acct); err != nil {
			w.log.Warn("inbound poll failed", "account", acct.Name, "err", err)
		}
	}
}

func (w *Watcher) pollAccount(_ context.Context, acct *config.AccountConfig) error {
	folder := acct.Inbound.WatchFolder
	if folder == "" {
		folder = "INBOX"
	}
	conn, err := w.pool.Resolve(acct.Name)
	if err != nil {
		return err
	}
	conn.Lock()
	defer conn.Unlock()
	c := conn.Client()

	if _, err := c.Select(folder, nil).Wait(); err != nil {
		return err
	}

	// Find unseen messages.
	criteria := &imaplib.SearchCriteria{NotFlag: []imaplib.Flag{imaplib.FlagSeen}}
	sd, err := c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return err
	}
	uids := sd.AllUIDs()
	if len(uids) == 0 {
		return nil
	}

	fetchOpts := &imaplib.FetchOptions{
		Envelope: true,
		UID:      true,
		BodySection: []*imaplib.FetchItemBodySection{
			{Specifier: imaplib.PartSpecifierHeader, HeaderFields: []string{"Authentication-Results", "DKIM-Signature", "From"}},
			{Specifier: imaplib.PartSpecifierText},
		},
	}

	for _, uid := range uids {
		set := imaplib.UIDSetNum(uid)
		msgs, err := c.Fetch(set, fetchOpts).Collect()
		if err != nil || len(msgs) == 0 {
			continue
		}
		m := msgs[0]
		from := ""
		if m.Envelope != nil && len(m.Envelope.From) > 0 {
			from = m.Envelope.From[0].Addr()
		}
		var rawHeaders, body string
		for _, bs := range m.BodySection {
			if bs.Section == nil {
				continue
			}
			switch bs.Section.Specifier {
			case imaplib.PartSpecifierHeader:
				rawHeaders = string(bs.Bytes)
			case imaplib.PartSpecifierText:
				body = string(bs.Bytes)
			}
		}

		msg := buildMessage(from, rawHeaders, body)
		if !hasEnvelope(msg.Body) {
			continue // ordinary mail — never touched, never marked
		}
		res := w.proc.Process(acct, msg)
		w.log.Info("inbound command processed",
			"account", acct.Name, "from", from, "triggered", res.Triggered,
			"passed", res.Passed, "failed", res.Failed)

		// Mark command attempts as seen so they aren't reprocessed.
		_ = c.Store(set, &imaplib.StoreFlags{
			Op:    imaplib.StoreFlagsAdd,
			Flags: []imaplib.Flag{imaplib.FlagSeen},
		}, nil).Close()
	}
	return nil
}
