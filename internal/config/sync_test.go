package config

import "testing"

func TestWindowAndFolderResolution(t *testing.T) {
	c := defaults()
	if f := c.SyncFolders(nil); len(f) != 2 || f[0] != "INBOX" || f[1] != `\Sent` {
		t.Fatalf("default folders = %v", f)
	}
	if d := c.WindowDays(nil, "INBOX"); d != 30 {
		t.Fatalf("default window = %d", d)
	}
	c.Sync.FolderWindowDays = map[string]int{`\Sent`: 90}
	a := &AccountConfig{Sync: &AccountSyncConfig{WindowDays: 60, FolderWindowDays: map[string]int{"INBOX": 7}, Folders: []string{"INBOX"}}}
	cases := []struct {
		acct   *AccountConfig
		folder string
		want   int
	}{
		{nil, "INBOX", 30},
		{nil, `\Sent`, 90},
		{a, "INBOX", 7},    // account folder override
		{a, `\Sent`, 90},   // global folder override beats account window
		{a, "Archive", 60}, // account window
	}
	for _, tc := range cases {
		if got := c.WindowDays(tc.acct, tc.folder); got != tc.want {
			t.Errorf("WindowDays(%v, %q) = %d, want %d", tc.acct != nil, tc.folder, got, tc.want)
		}
	}
	if f := c.SyncFolders(a); len(f) != 1 || f[0] != "INBOX" {
		t.Errorf("account folders replace global: %v", f)
	}
	t.Setenv("IMAP_MCP_SYNC_WINDOW_DAYS", "14")
	t.Setenv("IMAP_MCP_SYNC_MAX_MESSAGE_MB", "5")
	t.Setenv("IMAP_MCP_SYNC_INTERVAL_MINUTES", "3")
	c = defaults()
	applyEnvOverrides(c)
	if c.Sync.WindowDays != 14 || c.Sync.MaxMessageMB != 5 || c.Sync.IntervalMinutes != 3 {
		t.Errorf("env overrides: %+v", c.Sync)
	}
}
