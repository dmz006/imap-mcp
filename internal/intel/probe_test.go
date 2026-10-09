package intel

import (
	"context"
	"os"
	"testing"

	"github.com/dmz006/imap-mcp/internal/enrichment"
)

// TestProbeExtraction is a manual probe (skipped unless IMAP_MCP_PROBE_OLLAMA
// is set): it sends a synthetic example.com email through the real prompt to
// the real classify model and reports what parses.
func TestProbeExtraction(t *testing.T) {
	url, model := os.Getenv("IMAP_MCP_PROBE_OLLAMA"), os.Getenv("IMAP_MCP_PROBE_MODEL")
	if url == "" {
		t.Skip("manual probe")
	}
	c := enrichment.NewOllamaClassifier(url, model)
	body := "Hi team,\nStarting Monday, Dana Reyes will manage the Apollo migration and Sam Ortiz reports to Dana.\n" +
		"Sam now works at Example Logistics. The vendor contract renewal is due 2026-11-15.\nThanks, Ann"
	for i := 0; i < 3; i++ {
		ans, err := c.Classify(context.Background(), extractPrompt("ann@example.com", "2026-10-01", "Apollo staffing", body))
		if err != nil {
			t.Fatal(err)
		}
		rels := parseRelations(ans)
		t.Logf("attempt %d: answer %d chars, parsed %d relations: %+v", i, len(ans), len(rels), rels)
		if len(rels) == 0 {
			t.Logf("raw answer: %q", ans)
		}
	}
}
