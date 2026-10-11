package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/service"
	"github.com/dmz006/imap-mcp/internal/webhook"
)

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// runScreen is the screener from a terminal (AGENT.md Q5, D62): without
// decisions it lists held senders; with them it previews, and applies only
// with --confirm.
func runScreen(args []string) error {
	fs := flag.NewFlagSet("screen", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path")
	account := fs.String("account", "", "account name (default: the default account; all accounts when listing)")
	confirm := fs.Bool("confirm", false, "apply the decisions (default: preview)")
	asJSON := fs.Bool("json", false, "print JSON")
	var approve, block multiFlag
	fs.Var(&approve, "approve", "sender[:inbox|feed|receipts] or digest number[:dest] to approve (repeatable)")
	fs.Var(&block, "block", "sender or digest number to block (repeatable)")
	_ = fs.Parse(args)

	cfg, log, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	keys, err := cfg.ResolveDBKeys(true, false)
	if err != nil {
		return err
	}
	database, err := db.OpenState(db.Options{Path: cfg.DB.Path, Key: keys.State})
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer closeDB(log, database)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b := bus.New()
	b.Subscribe(bus.EventScreenerDecided, webhook.NewEnqueuer(database.Webhooks, log, nil).Handle)
	pool := imap.NewPool(cfg, b, log)
	if err := pool.Connect(ctx); err != nil {
		return fmt.Errorf("connect accounts: %w", err)
	}
	defer pool.Close()
	svc := service.New(cfg, pool, database, nil, nil)

	held, err := svc.HeldSenders(ctx, *account)
	if err != nil {
		return err
	}
	if len(approve) == 0 && len(block) == 0 {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(held)
		}
		for _, h := range held.Senders {
			fmt.Printf("[%d] %-8s %-40s %3d held  %s\n", h.Number, h.Account, h.Sender, h.Messages, h.Reasons)
		}
		fmt.Printf("screen: %d held sender(s). Decide with --approve SENDER[:feed|receipts] / --block SENDER, then --confirm.\n", held.Count)
		return nil
	}
	// A number refers to the list above for the chosen account.
	resolve := func(v string) string {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && fmt.Sprint(n) == v {
			for _, h := range held.Senders {
				if h.Number == n && (*account == "" || h.Account == *account) {
					return h.Sender
				}
			}
		}
		return v
	}
	var ds []service.ScreenDecision
	for _, a := range approve {
		sender, dest, _ := strings.Cut(a, ":")
		ds = append(ds, service.ScreenDecision{Sender: resolve(sender), Action: service.ScreenApprove, Dest: dest})
	}
	for _, s := range block {
		ds = append(ds, service.ScreenDecision{Sender: resolve(s), Action: service.ScreenBlock})
	}
	res, err := svc.ScreenVia(ctx, service.ScreenParams{Account: *account, Decisions: ds, Confirm: *confirm}, "cli")
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	for _, o := range res.Outcomes {
		line := fmt.Sprintf("  %-7s %-40s %d held -> %s", o.Action, o.Sender, o.Messages, o.Folder)
		if o.Rule != nil {
			line += fmt.Sprintf("; rule: %s from %q%s (active=%v, %d in inbox now)", o.Rule.Action, o.Rule.From,
				map[bool]string{true: " to " + o.Rule.Dest, false: ""}[o.Rule.Dest != ""], o.Rule.Active, o.Rule.Matches)
		}
		if o.Moved > 0 {
			line += fmt.Sprintf("; moved %d", o.Moved)
		}
		if o.Error != "" {
			line += "  ERROR: " + o.Error
		}
		fmt.Println(line)
	}
	if res.DryRun {
		fmt.Println("screen: preview only; add --confirm to apply.")
	}
	return nil
}
