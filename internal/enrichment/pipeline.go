package enrichment

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
)

// Pipeline processes messages from the enrichment queue: embeds, classifies,
// extracts KG entities, and detects anomalies using local Ollama models.
type Pipeline struct {
	cfg    config.EnrichmentConfig
	db     *db.DB
	ollama *OllamaClient
	bus    *bus.Bus
	log    *slog.Logger
}

// EnrichResult holds the output of enriching a single message.
type EnrichResult struct {
	MessageID int64
	Hall      string
	Wing      string
	Room      string
	Embedding []float32
	Entities  []KGEntity
	Anomalies []AnomalyHint
}

type KGEntity struct {
	Type      string
	Name      string
	Predicate string
	Object    string
}

type AnomalyHint struct {
	Type        string
	Description string
	Severity    string
}

func NewPipeline(cfg config.EnrichmentConfig, database *db.DB, b *bus.Bus, log *slog.Logger) *Pipeline {
	return &Pipeline{
		cfg:    cfg,
		db:     database,
		ollama: NewOllamaClient(cfg.OllamaURL),
		bus:    b,
		log:    log,
	}
}

// Run starts the background enrichment loop, processing pending messages in batches.
func (p *Pipeline) Run(ctx context.Context) {
	if !p.cfg.Enabled {
		return
	}
	p.log.Info("enrichment pipeline started", "model", p.cfg.LLMModel, "embed", p.cfg.EmbedModel)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.processBatch(ctx); err != nil {
				p.log.Error("enrichment batch failed", "err", err)
			}
		}
	}
}

func (p *Pipeline) processBatch(ctx context.Context) error {
	rows, err := p.db.SQL().QueryContext(ctx, `
		SELECT eq.id, eq.message_id, m.subject, m.body_text, m.from_addr, m.from_name, m.hall
		FROM enrichment_queue eq
		JOIN messages m ON m.id = eq.message_id
		WHERE eq.status = 'pending'
		ORDER BY eq.queued_at ASC
		LIMIT ?
	`, p.cfg.BatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()

	type queueItem struct {
		queueID   int64
		messageID int64
		subject   string
		body      string
		fromAddr  string
		fromName  string
		hall      string
	}

	var items []queueItem
	for rows.Next() {
		var it queueItem
		if err := rows.Scan(&it.queueID, &it.messageID, &it.subject, &it.body, &it.fromAddr, &it.fromName, &it.hall); err != nil {
			continue
		}
		items = append(items, it)
	}
	rows.Close()

	for _, it := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Mark processing
		p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='processing', attempts=attempts+1 WHERE id=?`, it.queueID) //nolint:errcheck

		result, err := p.enrich(ctx, it.messageID, it.subject, it.body, it.fromAddr, it.fromName)
		if err != nil {
			p.log.Warn("enrich message failed", "message_id", it.messageID, "err", err)
			p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='error', last_error=? WHERE id=?`, err.Error(), it.queueID) //nolint:errcheck
			p.db.SQL().ExecContext(ctx, `UPDATE messages SET enrichment_status='error', enrichment_error=? WHERE id=?`, err.Error(), it.messageID) //nolint:errcheck
			continue
		}

		if err := p.applyResult(ctx, result); err != nil {
			p.log.Warn("apply enrichment failed", "message_id", it.messageID, "err", err)
			continue
		}

		p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='done', processed_at=unixepoch() WHERE id=?`, it.queueID)         //nolint:errcheck
		p.db.SQL().ExecContext(ctx, `UPDATE messages SET enrichment_status='done', enriched_at=unixepoch() WHERE id=?`, it.messageID)     //nolint:errcheck

		p.bus.PublishAsync(bus.Event{
			Type:    bus.EventEnrichmentDone,
			Payload: result,
		})
		p.log.Debug("enriched", "message_id", it.messageID, "hall", result.Hall)
	}
	return nil
}

func (p *Pipeline) enrich(ctx context.Context, messageID int64, subject, body, fromAddr, fromName string) (*EnrichResult, error) {
	result := &EnrichResult{MessageID: messageID}

	// 1. Embed subject + first 500 chars of body for semantic search
	embedText := subject
	if len(body) > 0 {
		snip := body
		if len(snip) > 500 {
			snip = snip[:500]
		}
		embedText = subject + "\n" + snip
	}
	vec, err := p.ollama.Embed(ctx, p.cfg.EmbedModel, embedText)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	result.Embedding = vec

	// 2. Classify hall/wing/room via local LLM
	classPrompt := classificationPrompt(subject, body, fromAddr, fromName)
	classJSON, err := p.ollama.Generate(ctx, p.cfg.LLMModel, classPrompt)
	if err != nil {
		p.log.Warn("classification failed, using defaults", "err", err)
	} else {
		parseClassification(classJSON, result)
	}

	return result, nil
}

func (p *Pipeline) applyResult(ctx context.Context, r *EnrichResult) error {
	// Save embedding
	if len(r.Embedding) > 0 {
		blob := float32sToBlob(r.Embedding)
		_, err := p.db.SQL().ExecContext(ctx, `
			INSERT INTO message_vectors(message_id, vector, model, dims)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(message_id) DO UPDATE SET vector=excluded.vector, model=excluded.model, dims=excluded.dims
		`, r.MessageID, blob, p.cfg.EmbedModel, len(r.Embedding))
		if err != nil {
			return fmt.Errorf("save vector: %w", err)
		}
	}

	// Update hall/wing/room
	if r.Hall != "" || r.Wing != "" || r.Room != "" {
		_, err := p.db.SQL().ExecContext(ctx, `
			UPDATE messages SET hall=COALESCE(NULLIF(?,''),(SELECT hall FROM messages WHERE id=?)),
			wing=COALESCE(NULLIF(?,''),(SELECT wing FROM messages WHERE id=?)),
			room=COALESCE(NULLIF(?,''),(SELECT room FROM messages WHERE id=?))
			WHERE id=?
		`, r.Hall, r.MessageID, r.Wing, r.MessageID, r.Room, r.MessageID, r.MessageID)
		if err != nil {
			return fmt.Errorf("update tags: %w", err)
		}
	}
	return nil
}

// CosineSimilarity computes similarity between two vectors in Go (for query-time ranking).
func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}

func float32sToBlob(vs []float32) []byte {
	buf := make([]byte, len(vs)*4)
	for i, v := range vs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func BlobToFloat32s(b []byte) []float32 {
	vs := make([]float32, len(b)/4)
	for i := range vs {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		vs[i] = math.Float32frombits(bits)
	}
	return vs
}

func classificationPrompt(subject, body, fromAddr, fromName string) string {
	snip := body
	if len(snip) > 300 {
		snip = snip[:300]
	}
	return fmt.Sprintf(`Classify this email. Respond ONLY with valid JSON, no other text.

Email:
From: %s <%s>
Subject: %s
Body (snippet): %s

Respond with this exact JSON structure:
{"hall":"<transactional|conversation|newsletter|notification|alert|personal>","wing":"<project or context, empty string if unclear>","room":"<topic, empty string if unclear>"}`,
		fromName, fromAddr, subject, snip)
}

func parseClassification(raw string, result *EnrichResult) {
	// Extract JSON from potentially noisy LLM output
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return
	}
	var c struct {
		Hall string `json:"hall"`
		Wing string `json:"wing"`
		Room string `json:"room"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &c); err != nil {
		return
	}
	result.Hall = c.Hall
	result.Wing = c.Wing
	result.Room = c.Room
}
