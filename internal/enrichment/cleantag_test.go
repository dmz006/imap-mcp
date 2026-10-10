package enrichment

import "testing"

// TestParseClassificationDropsPlaceholders: template text the model copies
// from the prompt never becomes a tag.
func TestParseClassificationDropsPlaceholders(t *testing.T) {
	var r EnrichResult
	parseClassification(`{"hall":"<transactional|conversation>","wing":"<project or context, empty string if unclear>","room":"<topic, empty string if unclear>"}`, &r)
	if r.Hall != "" || r.Wing != "" || r.Room != "" {
		t.Errorf("placeholders kept: %+v", r)
	}
	parseClassification(`{"hall":"Notification","wing":"Home renovation","room":"password reset"}`, &r)
	if r.Hall != "notification" || r.Wing != "Home renovation" || r.Room != "password reset" {
		t.Errorf("real tags lost: %+v", r)
	}
	for _, p := range []string{"project", "Topic", "n/a", "project or context", " <x> "} {
		if CleanTag(p) != "" {
			t.Errorf("CleanTag(%q) kept", p)
		}
	}
}
