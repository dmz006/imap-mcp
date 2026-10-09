package tools

import "testing"

func TestSweepFilterFromArgs(t *testing.T) {
	f, dry := SweepFilterFromArgs(map[string]any{})
	if !dry || !f.Empty() || f.All {
		t.Fatalf("defaults: %+v dry=%v", f, dry)
	}
	f, dry = SweepFilterFromArgs(map[string]any{"account": "a", "folder": "INBOX", "older_than_days": float64(7),
		"errors_only": true, "all": false, "dry_run": false})
	if dry || f.Account != "a" || f.Folder != "INBOX" || f.OlderThanDays != 7 || !f.ErrorsOnly {
		t.Fatalf("parsed: %+v dry=%v", f, dry)
	}
}
