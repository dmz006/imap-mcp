package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/query"
)

func querySvc(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	now := time.Now().Unix()
	for i, m := range []struct {
		from, subject, flags string
		age                  int64
	}{
		{"alice@example.com", "Invoice 50% off_", `["\\Seen"]`, 1},
		{"bob@example.com", "Hello", `[]`, 2},
		{"carol@example.org", "Report", `["\\Seen","\\Flagged"]`, 3},
		{"dave@example.com", "Old", `[]`, 400},
	} {
		if _, err := d.SQL().Exec(`INSERT INTO messages(account, folder, uid, from_addr, subject, flags, date, body_text) VALUES('acct','INBOX',?,?,?,?,?,?)`,
			i+1, m.from, m.subject, m.flags, now-m.age*86400, "secret body "+m.subject); err != nil {
			t.Fatal(err)
		}
	}
	return New(nil, nil, d, nil, nil)
}

func TestQueryRuns(t *testing.T) {
	s := querySvc(t)
	ctx := context.Background()

	r, err := s.Query(ctx, query.Query{View: "messages", GroupBy: []string{"from_domain"},
		Aggregate: []query.Aggregate{{Fn: "count", As: "n"}}, OrderBy: []query.Order{{Field: "n", Desc: true}},
		Where: []query.Filter{{Field: "date", Op: "gte", Value: "-30d"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Count != 2 || r.Rows[0][0] != "example.com" || r.Rows[0][1] != int64(2) {
		t.Fatalf("by domain = %+v", r.Rows)
	}

	r, err = s.Query(ctx, query.Query{View: "messages", Fields: []string{"uid", "seen", "date"},
		Where: []query.Filter{{Field: "subject", Op: "contains", Value: "50% off_"}}})
	if err != nil || r.Count != 1 || r.Rows[0][1] != true {
		t.Fatalf("contains with wildcards = %+v, %v", r, err)
	}
	if _, err := time.Parse(time.RFC3339, r.Rows[0][2].(string)); err != nil {
		t.Errorf("date not RFC3339: %v", r.Rows[0][2])
	}
	if r, _ = s.Query(ctx, query.Query{View: "messages", Where: []query.Filter{{Field: "subject", Op: "contains", Value: "%"}}}); r.Count != 1 {
		t.Errorf("literal %% matched %d rows", r.Count)
	}

	r, _ = s.Query(ctx, query.Query{View: "messages"})
	for _, row := range r.Rows {
		for _, v := range row {
			if str, ok := v.(string); ok && len(str) > 6 && str[:6] == "secret" {
				t.Fatal("body returned without being named")
			}
		}
	}
	if r, _ = s.Query(ctx, query.Query{View: "messages", Fields: []string{"body_text"}, Limit: 1}); r.Rows[0][0] == nil || !r.Truncated {
		t.Errorf("named body / truncation: %+v", r)
	}

	if _, err := s.Query(ctx, query.Query{View: "nope"}); KindOf(err) != KindInvalid {
		t.Errorf("bad query kind = %v", KindOf(err))
	}
}
