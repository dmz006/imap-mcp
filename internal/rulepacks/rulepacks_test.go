package rulepacks

import (
	"testing"
)

// TestPacksAreValid: every built-in pack parses, and every rule it builds
// passes the same validation create_rule applies.
func TestPacksAreValid(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"bounces": true, "calendar-replies": true, "dev-notifications": true, "dmarc-reports": true}
	if len(all) != len(want) {
		t.Fatalf("packs = %d, want %d", len(all), len(want))
	}
	for _, p := range all {
		if !want[p.Name] || p.Description == "" || len(p.Rules) == 0 {
			t.Errorf("pack %+v", p)
		}
		for _, r := range p.Build("acct", "") {
			if r.Active || r.Conditions.Account != "acct" || (r.Conditions.From == "" && r.Conditions.Subject == "") {
				t.Errorf("%s: rule %+v", p.Name, r)
			}
			for _, a := range r.Actions {
				if a.Type == "move" && a.Dest == "" {
					t.Errorf("%s/%s: move without a folder", p.Name, r.Name)
				}
				if a.Type != "move" && a.Type != "trash" {
					t.Errorf("%s/%s: action %q", p.Name, r.Name, a.Type)
				}
			}
		}
	}
	if _, err := Get("nope"); err == nil {
		t.Error("unknown pack found")
	}
}
