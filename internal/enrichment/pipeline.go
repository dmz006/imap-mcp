package enrichment

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	gosync "sync"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
)

// Queue lanes (AGENT.md D11b): new mail is always served before backfill.
const (
	LaneNew      = 0
	LaneBackfill = 1
)

// Pipeline processes the enrichment queue: embed, classify, store. It serves
// the new-mail lane before backfill, caps in-flight calls per provider, rate
// limits backfill, backs off on transient provider errors, and lets backfill
// yield to other GPU work through LoadGates. New mail is never gated.
type Pipeline struct {
	cfg      config.EnrichmentConfig
	db       *db.DB
	bus      *bus.Bus
	log      *slog.Logger
	cleaner  Cleaner
	embedder Embedder
	classify Classifier
	gates    []LoadGate
	backfill *rateLimiter
	now      func() time.Time

	embedSem, classifySem chan struct{}

	mu           gosync.Mutex
	failures     int
	backoffUntil time.Time
	paused       string // last backfill pause reason ("" = running)
	lastErr      string
	trigger      chan int
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

// Option customises a Pipeline.
type Option func(*Pipeline)

// WithDatawatch enables the datawatch classify provider (when configured) and
// the datawatch capacity gate, using the datawatch API URL and token.
func WithDatawatch(apiURL, token string) Option {
	return func(p *Pipeline) {
		if apiURL == "" {
			return
		}
		if p.cfg.ClassifyProvider() == "datawatch" {
			p.classify = NewDatawatchClassifier(apiURL, p.cfg.Classify.DatawatchLLM, token)
		}
		if p.cfg.Yield.Enabled {
			pools := append([]string(nil), p.cfg.Yield.DatawatchPools...)
			if p.cfg.ClassifyProvider() == "datawatch" {
				pools = append(pools, "llm:"+p.cfg.Classify.DatawatchLLM)
			}
			if len(pools) > 0 {
				p.gates = append(p.gates, cached(NewDatawatchGate(apiURL, token, pools), 30*time.Second))
			}
		}
	}
}

// WithProviders overrides the embedder and classifier (tests, plugins).
func WithProviders(e Embedder, c Classifier) Option {
	return func(p *Pipeline) { p.embedder, p.classify = e, c }
}

// WithGates replaces the load gates (tests, plugins).
func WithGates(g ...LoadGate) Option { return func(p *Pipeline) { p.gates = g } }

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(p *Pipeline) { p.now = now } }

func NewPipeline(cfg config.EnrichmentConfig, database *db.DB, b *bus.Bus, log *slog.Logger, opts ...Option) *Pipeline {
	conc := max(cfg.Concurrency, 1)
	p := &Pipeline{
		cfg:         cfg,
		db:          database,
		bus:         b,
		log:         log,
		cleaner:     NopCleaner{},
		embedder:    NewOllamaEmbedder(cfg.EmbedURL(), cfg.EmbedModelName()),
		classify:    NewOllamaClassifier(cfg.ClassifyURL(), cfg.ClassifyModel()),
		backfill:    newRateLimiter(cfg.BackfillPerMinute),
		now:         time.Now,
		embedSem:    make(chan struct{}, conc),
		classifySem: make(chan struct{}, conc),
		trigger:     make(chan int, 1),
	}
	if start, end, err := config.ParseWindow(cfg.BackfillWindow); err == nil && start >= 0 {
		p.gates = append(p.gates, NewWindowGate(start, end))
	}
	if cfg.Yield.Enabled {
		p.gates = append(p.gates, cached(NewOllamaGate(cfg.EmbedURL(), []string{cfg.EmbedModelName(), cfg.ClassifyModel()}, cfg.Yield.MaxForeignResidentGB), 30*time.Second))
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Run starts the background enrichment loop.
func (p *Pipeline) Run(ctx context.Context) {
	if !p.cfg.Enabled {
		return
	}
	p.log.Info("enrichment pipeline started", "embed", p.embedder.Name(), "classify", p.classify.Name(),
		"concurrency", cap(p.embedSem), "backfill_per_minute", p.cfg.BackfillPerMinute, "gates", len(p.gates))

	// Rows left "processing" by a crash or restart go back to the queue.
	if _, err := p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='pending' WHERE status='processing'`); err != nil {
		p.log.Warn("requeue interrupted enrichment", "err", err)
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		limit := 0
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case limit = <-p.trigger:
		}
		if _, err := p.runOnce(ctx, limit); err != nil && ctx.Err() == nil {
			p.log.Error("enrichment batch failed", "err", err)
		}
	}
}

// Trigger asks the running pipeline to process up to limit messages now,
// bypassing the backfill window, yield gates and rate limit (but not backoff
// or concurrency caps). It returns false when a trigger is already pending.
func (p *Pipeline) Trigger(limit int) bool {
	select {
	case p.trigger <- max(limit, 1):
		return true
	default:
		return false
	}
}

// processBatch runs one scheduled batch (kept for tests and callers).
func (p *Pipeline) processBatch(ctx context.Context) error {
	_, err := p.runOnce(ctx, 0)
	return err
}

type queueItem struct {
	queueID   int64
	messageID int64
	lane      int
	attempts  int
	subject   string
	body      string
	fromAddr  string
	fromName  string
}

// runOnce selects and processes one batch. forced > 0 is a manual trigger of
// up to forced messages that bypasses backfill gating.
func (p *Pipeline) runOnce(ctx context.Context, forced int) (int, error) {
	now := p.now()
	p.mu.Lock()
	wait := p.backoffUntil.Sub(now)
	p.mu.Unlock()
	if wait > 0 {
		return 0, nil
	}

	batch := max(p.cfg.BatchSize, 1)
	if forced > 0 {
		batch = forced
	}
	items, err := p.selectLane(ctx, LaneNew, batch)
	if err != nil {
		return 0, err
	}
	if room := batch - len(items); room > 0 {
		allowed := forced > 0
		if !allowed {
			allowed = p.backfillAllowed(ctx, now)
		}
		if allowed {
			n := room
			if forced == 0 {
				n = p.backfill.take(now, room)
			}
			if n > 0 {
				more, err := p.selectLane(ctx, LaneBackfill, n)
				if err != nil {
					return 0, err
				}
				items = append(items, more...)
			}
		}
	}
	if len(items) == 0 {
		return 0, nil
	}
	return p.process(ctx, items), nil
}

// backfillAllowed consults every gate; the first refusal pauses backfill.
func (p *Pipeline) backfillAllowed(ctx context.Context, now time.Time) bool {
	reason := ""
	for _, g := range p.gates {
		if ok, why := g.Allow(ctx, now); !ok {
			reason = g.Name() + ": " + why
			break
		}
	}
	p.mu.Lock()
	changed := reason != p.paused
	p.paused = reason
	p.mu.Unlock()
	if changed {
		if reason != "" {
			p.log.Info("backfill enrichment paused", "reason", reason)
		} else {
			p.log.Info("backfill enrichment resumed")
		}
	}
	return reason == ""
}

func (p *Pipeline) selectLane(ctx context.Context, lane, limit int) ([]queueItem, error) {
	rows, err := p.db.SQL().QueryContext(ctx, `
		SELECT eq.id, eq.message_id, eq.lane, eq.attempts, COALESCE(m.subject, ''), COALESCE(m.body_text, ''), m.from_addr,
		       COALESCE(m.from_name, '')
		FROM enrichment_queue eq
		JOIN messages m ON m.id = eq.message_id
		WHERE eq.status = 'pending' AND eq.lane = ?
		ORDER BY eq.queued_at ASC, eq.id ASC
		LIMIT ?
	`, lane, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []queueItem
	for rows.Next() {
		var it queueItem
		if err := rows.Scan(&it.queueID, &it.messageID, &it.lane, &it.attempts, &it.subject, &it.body, &it.fromAddr, &it.fromName); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// process enriches items concurrently (bounded by the provider semaphores)
// and returns how many succeeded.
func (p *Pipeline) process(ctx context.Context, items []queueItem) int {
	var wg gosync.WaitGroup
	var mu gosync.Mutex
	ok := 0
	for _, it := range items {
		if _, err := p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='processing', attempts=attempts+1 WHERE id=?`, it.queueID); err != nil {
			return ok
		}
		wg.Add(1)
		go func(it queueItem) {
			defer wg.Done()
			if p.handle(ctx, it) {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(it)
	}
	wg.Wait()
	return ok
}

// handle enriches one item and records the outcome.
func (p *Pipeline) handle(ctx context.Context, it queueItem) bool {
	result, err := p.enrich(ctx, it.messageID, it.subject, it.body, it.fromAddr, it.fromName)
	if err == nil {
		err = p.applyResult(ctx, result)
	}
	if err != nil {
		p.fail(ctx, it, err)
		return false
	}
	p.succeed()
	p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='done', processed_at=unixepoch(), last_error=NULL WHERE id=?`, it.queueID)           //nolint:errcheck
	p.db.SQL().ExecContext(ctx, `UPDATE messages SET enrichment_status='done', enriched_at=unixepoch(), enrichment_error=NULL WHERE id=?`, it.messageID) //nolint:errcheck
	p.bus.PublishAsync(bus.Event{Type: bus.EventEnrichmentDone, Payload: result})
	p.log.Debug("enriched", "message_id", it.messageID, "lane", it.lane, "hall", result.Hall)
	return true
}

// fail requeues the item (or marks it error after max attempts) and, for
// transient provider errors, extends the exponential backoff.
func (p *Pipeline) fail(ctx context.Context, it queueItem, err error) {
	attempts := it.attempts + 1
	maxAttempts := max(p.cfg.MaxAttempts, 1)
	p.log.Warn("enrich message failed", "message_id", it.messageID, "attempt", attempts, "err", err)
	if attempts >= maxAttempts {
		p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='error', last_error=? WHERE id=?`, err.Error(), it.queueID)            //nolint:errcheck
		p.db.SQL().ExecContext(ctx, `UPDATE messages SET enrichment_status='error', enrichment_error=? WHERE id=?`, err.Error(), it.messageID) //nolint:errcheck
		p.bus.PublishAsync(bus.Event{Type: bus.EventEnrichmentError, Payload: map[string]any{"message_id": it.messageID, "error": err.Error()}})
	} else {
		p.db.SQL().ExecContext(ctx, `UPDATE enrichment_queue SET status='pending', last_error=? WHERE id=?`, err.Error(), it.queueID) //nolint:errcheck
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastErr = err.Error()
	if IsTransient(err) {
		p.failures++
		d := time.Duration(1<<min(p.failures-1, 16)) * 5 * time.Second
		if limit := time.Duration(max(p.cfg.BackoffMaxSeconds, 1)) * time.Second; d > limit {
			d = limit
		}
		if until := p.now().Add(d); until.After(p.backoffUntil) {
			p.backoffUntil = until
			p.log.Warn("enrichment backing off", "for", d, "consecutive_failures", p.failures)
		}
	}
}

func (p *Pipeline) succeed() {
	p.mu.Lock()
	p.failures = 0
	p.backoffUntil = time.Time{}
	p.mu.Unlock()
}

func (p *Pipeline) enrich(ctx context.Context, messageID int64, subject, body, fromAddr, fromName string) (*EnrichResult, error) {
	result := &EnrichResult{MessageID: messageID}
	subject, body = p.cleaner.Clean(subject, body)

	// 1. Embed subject + first 500 chars of body for semantic search.
	embedText := subject
	if len(body) > 0 {
		snip := body
		if len(snip) > 500 {
			snip = snip[:500]
		}
		embedText = subject + "\n" + snip
	}
	p.embedSem <- struct{}{}
	vec, err := p.embedder.Embed(ctx, embedText)
	<-p.embedSem
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	result.Embedding = vec

	// 2. Classify hall/wing/room. A transient classifier failure retries the
	// message; a bad answer keeps the defaults.
	p.classifySem <- struct{}{}
	classJSON, err := p.classify.Classify(ctx, classificationPrompt(subject, body, fromAddr, fromName))
	<-p.classifySem
	if err != nil {
		if IsTransient(err) {
			return nil, fmt.Errorf("classify: %w", err)
		}
		p.log.Warn("classification failed, using defaults", "err", err)
	} else {
		parseClassification(classJSON, result)
	}
	return result, nil
}

// Stats is the enrichment queue and load state for /api/health and
// enrichment_status.
type Stats struct {
	Enabled             bool           `json:"enabled"`
	Embed               string         `json:"embed_provider"`
	Classify            string         `json:"classify_provider"`
	Pending             map[string]int `json:"pending"` // by lane: new, backfill
	Processing          int            `json:"processing"`
	Done                int            `json:"done"`
	Errors              int            `json:"errors"`
	Duplicates          int            `json:"duplicates"`
	DoneLastHour        int            `json:"done_last_hour"`
	OldestPendingSecs   map[string]int `json:"oldest_pending_seconds,omitempty"`
	BackfillPaused      string         `json:"backfill_paused,omitempty"`
	BackoffSeconds      int            `json:"backoff_seconds,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures,omitempty"`
	LastError           string         `json:"last_error,omitempty"`
}

// Stats reports queue depth per lane, throughput, lag and load state.
func (p *Pipeline) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Enabled: p.cfg.Enabled, Embed: p.embedder.Name(), Classify: p.classify.Name(),
		Pending: map[string]int{"new": 0, "backfill": 0}, OldestPendingSecs: map[string]int{}}
	laneName := map[int]string{LaneNew: "new", LaneBackfill: "backfill"}
	now := p.now().Unix()
	rows, err := p.db.SQL().QueryContext(ctx, `SELECT status, lane, count(*), MIN(queued_at) FROM enrichment_queue GROUP BY status, lane`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var status string
		var lane, n int
		var oldest sql.NullInt64
		if err := rows.Scan(&status, &lane, &n, &oldest); err != nil {
			rows.Close()
			return st, err
		}
		switch status {
		case "pending":
			st.Pending[laneName[lane]] += n
			if oldest.Valid {
				st.OldestPendingSecs[laneName[lane]] = int(now - oldest.Int64)
			}
		case "processing":
			st.Processing += n
		case "done":
			st.Done += n
		case "error":
			st.Errors += n
		}
	}
	rows.Close()
	p.db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM enrichment_queue WHERE status='done' AND processed_at >= ?`, now-3600).Scan(&st.DoneLastHour) //nolint:errcheck
	p.db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE enrichment_status='duplicate'`).Scan(&st.Duplicates)                           //nolint:errcheck
	p.mu.Lock()
	st.BackfillPaused = p.paused
	if d := p.backoffUntil.Sub(p.now()); d > 0 {
		st.BackoffSeconds = int(d.Seconds())
	}
	st.ConsecutiveFailures = p.failures
	st.LastError = p.lastErr
	p.mu.Unlock()
	return st, nil
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
