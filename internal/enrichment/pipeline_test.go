package enrichment

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

type upperCleaner struct{ seen *string }

func (u upperCleaner) Clean(subject, body string) (string, string) {
	*u.seen = subject + "|" + body
	return "CLEAN " + subject, "CLEAN"
}

func TestCleanerHookFeedsModelsNotCache(t *testing.T) {
	var embedded string
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path == "/api/embeddings" {
			embedded, _ = req["prompt"].(string)
			json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"response": `{}`})
	}))
	defer ollama.Close()
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err := d.Messages.Insert(ctx, &db.CachedMessage{Account: "a", Folder: "INBOX", UID: 1, FromAddr: "x@example.com",
		Subject: "hi", BodyText: "raw body", InternalDate: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var seen string
	p := NewPipeline(config.EnrichmentConfig{OllamaURL: ollama.URL, BatchSize: 5}, d, bus.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.SetCleaner(upperCleaner{&seen})
	if err := p.processBatch(ctx); err != nil {
		t.Fatal(err)
	}
	if seen != "hi|raw body" {
		t.Errorf("cleaner got %q", seen)
	}
	if !strings.HasPrefix(embedded, "CLEAN hi") {
		t.Errorf("embedder got %q, want cleaned text", embedded)
	}
	var stored string
	d.SQL().QueryRow(`SELECT body_text FROM messages WHERE uid=1`).Scan(&stored)
	if stored != "raw body" {
		t.Errorf("cache body changed to %q; cleaning must not touch the cache", stored)
	}
}
