package service

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
)

type vecEmbed struct{ v []float32 }

func (e vecEmbed) Name() string                                     { return "fake" }
func (e vecEmbed) Embed(context.Context, string) ([]float32, error) { return e.v, nil }

type noClass struct{}

func (noClass) Name() string                                     { return "fake" }
func (noClass) Classify(context.Context, string) (string, error) { return "{}", nil }

func blob(v ...float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func intelSvc(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "personal", Default: true}, {Name: "work"}}}
	p := enrichment.NewPipeline(config.EnrichmentConfig{Concurrency: 1}, d, bus.New(), slog.New(slog.NewTextHandler(io.Discard, nil)),
		enrichment.WithProviders(vecEmbed{[]float32{1, 0}}, noClass{}))
	ctx := context.Background()
	for _, m := range []struct {
		acct, folder string
		uid          uint32
		from         string
		vec          []byte
	}{
		{"personal", "INBOX", 1, "alice@example.com", blob(1, 0)},     // identical to query
		{"personal", "INBOX", 2, "alice@example.com", blob(0.8, 0.6)}, // cos 0.8
		{"personal", "INBOX", 3, "bob@example.com", blob(0, 1)},       // cos 0
		{"work", "INBOX", 4, "carol@example.com", blob(1, 0.1)},       // cos ~0.995
		{"personal", "INBOX", 5, "dave@example.com", nil},             // not enriched yet
	} {
		res, err := d.Messages.Insert(ctx, &db.CachedMessage{Account: m.acct, Folder: m.folder, UID: m.uid, FromAddr: m.from,
			Subject: "s", InternalDate: time.Now().Add(-time.Duration(m.uid) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if m.vec != nil {
			if _, err := d.SQL().Exec(`INSERT INTO message_vectors(message_id, vector, model, dims) VALUES(?,?,?,2)`, res.ID, m.vec, "e"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, q := range []string{
		`INSERT INTO senders(address, domain, role, message_count) VALUES('alice@example.com','example.com','colleague',2),('bob@example.com','example.com','vendor',1)`,
		`INSERT INTO kg_entities(entity_type, name) VALUES('person','alice@example.com'),('organization','Example Inc')`,
		`INSERT INTO kg_relationships(subject_id, predicate, object_id) VALUES(1,'belongs_to',2)`,
		`INSERT INTO anomalies(account, sender, anomaly_type, severity, resolved) VALUES('personal','alice@example.com','reply_spike','high',0),('personal','bob@example.com','silence','low',1)`,
	} {
		if _, err := d.StateSQL().Exec(q); err != nil { // intelligence lives in imap.db (D19)
			t.Fatal(err)
		}
	}
	return New(cfg, nil, d, nil, p), d
}

func TestSemanticSearch(t *testing.T) {
	s, _ := intelSvc(t)
	ctx := context.Background()
	res, err := s.SemanticSearch(ctx, SemanticParams{Query: "invoices", Threshold: 0.75})
	if err != nil {
		t.Fatal(err)
	}
	if res.Searched != 4 || len(res.Hits) != 3 || res.Hits[0].UID != 1 || res.Hits[1].UID != 4 || res.Hits[2].UID != 2 {
		t.Fatalf("hits = %+v searched=%d", res.Hits, res.Searched)
	}
	res, _ = s.SemanticSearch(ctx, SemanticParams{Query: "x", Account: "work"})
	if res.Searched != 1 || len(res.Hits) != 1 || res.Hits[0].Account != "work" {
		t.Fatalf("account scope = %+v", res)
	}
	res, err = s.SemanticSearch(ctx, SemanticParams{ReferenceUID: 1, Folder: "INBOX", Threshold: 0.5, Limit: 1})
	if err != nil || len(res.Hits) != 1 || res.Hits[0].UID == 1 {
		t.Fatalf("reference search must exclude itself: %+v %v", res, err)
	}
	if _, err := s.SemanticSearch(ctx, SemanticParams{ReferenceUID: 5, Folder: "INBOX"}); KindOf(err) != KindNotFound {
		t.Errorf("unenriched reference: %v", err)
	}
	if _, err := s.SemanticSearch(ctx, SemanticParams{}); KindOf(err) != KindInvalid {
		t.Errorf("no query: %v", err)
	}
	if _, err := s.SemanticSearch(ctx, SemanticParams{ReferenceUID: 1}); KindOf(err) != KindInvalid {
		t.Errorf("reference without folder: %v", err)
	}
	s.pipeline = nil
	if _, err := s.SemanticSearch(ctx, SemanticParams{Query: "q"}); KindOf(err) != KindUnavailable {
		t.Errorf("no pipeline: %v", err)
	}
}

func TestSenderKGAnomalies(t *testing.T) {
	s, _ := intelSvc(t)
	ctx := context.Background()
	senders, err := s.ListSenders(ctx, "", "", 0)
	if err != nil || len(senders) != 2 || senders[0].Address != "alice@example.com" {
		t.Fatalf("senders = %+v %v", senders, err)
	}
	if v, _ := s.ListSenders(ctx, "vendor", "", 0); len(v) != 1 {
		t.Errorf("role filter = %+v", v)
	}
	p, err := s.GetSenderProfile(ctx, " Alice@Example.com ")
	if err != nil || p.Role != "colleague" || p.CachedMessages != 2 || len(p.Relationships) != 1 || len(p.Anomalies) != 1 {
		t.Fatalf("profile = %+v %v", p, err)
	}
	p, err = s.GetSenderProfile(ctx, "dave@example.com") // cached mail, no profile row yet
	if err != nil || p.Role != "unknown" || p.CachedMessages != 1 {
		t.Fatalf("profile without row = %+v %v", p, err)
	}
	if _, err := s.GetSenderProfile(ctx, "nobody@example.com"); KindOf(err) != KindNotFound {
		t.Errorf("unknown sender: %v", err)
	}
	hist, err := s.SenderHistory(ctx, "", "alice@example.com", 0)
	if err != nil || len(hist) != 2 || hist[0].UID != 1 {
		t.Fatalf("history = %+v %v", hist, err)
	}
	edges, _ := s.KGQuery(ctx, KGParams{Entity: "Example Inc"})
	if len(edges) != 1 || edges[0].Predicate != "belongs_to" {
		t.Fatalf("kg = %+v", edges)
	}
	if e, _ := s.KGQuery(ctx, KGParams{Predicate: "manages"}); len(e) != 0 {
		t.Errorf("predicate filter = %+v", e)
	}
	open, _ := s.Anomalies(ctx, AnomalyParams{})
	all, _ := s.Anomalies(ctx, AnomalyParams{IncludeResolved: true})
	if len(open) != 1 || len(all) != 2 {
		t.Fatalf("anomalies open=%d all=%d", len(open), len(all))
	}
	if _, err := s.Anomalies(ctx, AnomalyParams{Severity: "extreme"}); KindOf(err) != KindInvalid {
		t.Errorf("bad severity: %v", err)
	}
}

func TestIntelStats(t *testing.T) {
	s, d := intelSvc(t)
	s.cfg = &config.Config{}
	ctx := context.Background()
	st, err := s.IntelStats(ctx)
	if err != nil || st.Folders != 0 || st.BackfillComplete || st.Senders != 2 || st.Roles["colleague"] != 1 || !st.Enabled {
		t.Fatalf("empty scan = %+v %v", st, err)
	}
	for _, q := range []string{
		`INSERT INTO intel_scan(account, folder, uidvalidity, last_uid, scanned, completed_at) VALUES('a','INBOX',1,10,10,unixepoch()),('a','Sent',1,4,4,NULL)`,
		`INSERT INTO intel_messages(account, msg_hash, date, outgoing, paired) VALUES('a',1,0,0,0),('a',2,0,1,1)`,
	} {
		if _, err := d.StateSQL().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = s.IntelStats(ctx)
	if st.Folders != 2 || st.FoldersComplete != 1 || st.BackfillComplete || st.MessagesIndexed != 2 || st.RepliesPaired != 1 || st.LastScan == "" {
		t.Fatalf("partial scan = %+v", st)
	}
	d.StateSQL().Exec(`UPDATE intel_scan SET completed_at = unixepoch()`) //nolint:errcheck
	if st, _ = s.IntelStats(ctx); !st.BackfillComplete {
		t.Error("all folders complete must report backfill_complete")
	}
	if p, err := s.GetSenderProfile(ctx, "alice@example.com"); err != nil || !p.ScanComplete {
		t.Errorf("profile scan_complete = %+v %v", p.ScanComplete, err)
	}
}
