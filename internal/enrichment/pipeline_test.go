package enrichment

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
)

// TestProcessBatchHandlesNullColumns guards against rows with NULL body or
// sender name (headers-only oversize mail, address-only senders) being
// skipped forever and starving the queue.
func TestProcessBatchHandlesNullColumns(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embeddings":
			json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{0.1, 0.2, 0.3}})
		case "/api/generate":
			json.NewEncoder(w).Encode(map[string]any{"response": `{"hall":"notification","wing":"w","room":"r"}`})
		}
	}))
	defer ollama.Close()

	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for i, m := range []*db.CachedMessage{
		{Account: "a", Folder: "INBOX", UID: 1, FromAddr: "x@example.com", BodySkipped: true, InternalDate: time.Now()},
		{Account: "a", Folder: "INBOX", UID: 2, FromAddr: "y@example.com", Subject: "s", BodyText: "b", InternalDate: time.Now()},
	} {
		if _, err := d.Messages.Insert(ctx, m); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	p := NewPipeline(config.EnrichmentConfig{OllamaURL: ollama.URL, EmbedModel: "e", LLMModel: "l", BatchSize: 1}, d, bus.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 0; i < 2; i++ {
		if err := p.processBatch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var pending, done, vectors int
	d.SQL().QueryRow(`SELECT count(*) FROM enrichment_queue WHERE status='pending'`).Scan(&pending)
	d.SQL().QueryRow(`SELECT count(*) FROM enrichment_queue WHERE status='done'`).Scan(&done)
	d.SQL().QueryRow(`SELECT count(*) FROM message_vectors`).Scan(&vectors)
	if pending != 0 || done != 2 || vectors != 2 {
		t.Fatalf("pending=%d done=%d vectors=%d; NULL-column rows must be processed", pending, done, vectors)
	}
}
