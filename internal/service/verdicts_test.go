package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/intel"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
	"github.com/dmz006/imap-mcp/internal/verdict"
)

// fakeOllama answers every generate call with answer.
func fakeOllama(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"response": answer})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVerdictHold covers D37: with the verdict active, score-1 ties go to the
// model, a confident scam verdict holds even a score-0 first contact, and no
// verdict yet means the message stays.
func TestVerdictHold(t *testing.T) {
	f := newHoldFixture(t)
	ctx := context.Background()
	me := imaptest.Username
	now := time.Now()
	for _, m := range []string{
		raw("v0s@x", "Dr. Ann <ann@health.example>", me, "A ritual for memory"),                              // score 0, model: scam
		raw("v0l@x", "Bob <bob@friendly.example>", me, "Hello"),                                              // score 0, model: legitimate
		raw("v1b@x", "Promo <p@single.example>", me, "News", "List-Unsubscribe: <mailto:u@single.example>"),  // score 1, model: bulk
		raw("v1n@x", "Promo2 <p@otherone.example>", me, "News", "List-Unsubscribe: <mailto:u@otherone.example>"), // score 1, no verdict
		raw("v0w@x", "Weak <w@weak.example>", me, "Hi"),                                                      // score 0, model: scam below threshold
	} {
		f.srv.Append(t, "INBOX", m, now)
	}
	st := f.d.StateSQL()
	if _, err := st.Exec(`INSERT INTO intel_scan(account, folder, completed_at) VALUES('test','INBOX',1)`); err != nil {
		t.Fatal(err)
	}
	for id, v := range map[string][2]any{"v0s@x": {"scam", 0.95}, "v0l@x": {"legitimate", 0.97}, "v1b@x": {"bulk", 0.9}, "v0w@x": {"scam", 0.5}} {
		if _, err := st.Exec(`INSERT INTO message_verdicts(account, msg_hash, verdict, asks, confidence, model, created_at) VALUES('test',?,?,'none',?,'m',1)`,
			intel.MsgHash(id), v[0], v[1]); err != nil {
			t.Fatal(err)
		}
	}
	f.s.cfg.Intel.Verdict = verdictCfg("m", true)
	id, err := f.s.CreateRule(ctx, &db.Rule{Name: "hold", Active: true, Conditions: db.RuleConditions{NewSender: true},
		Actions: []db.RuleAction{{Type: "move", Dest: "Held"}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.s.TestRule(ctx, id)
	if err != nil || res.Matched != 2 {
		t.Fatalf("held = %+v, %v; want 2 (v0s scam, v1b bulk)", res, err)
	}
	for _, p := range res.Preview {
		if !strings.Contains(p.Reasons, "model:") {
			t.Errorf("reason without the model: %+v", p)
		}
	}
	// Shadow mode: verdicts recorded but not acted on.
	f.s.cfg.Intel.Verdict.Active = false
	if res, _ := f.s.TestRule(ctx, id); res.Matched == 2 {
		t.Errorf("shadow mode acted on verdicts: %+v", res)
	}
}

func verdictCfg(model string, active bool) config.VerdictConfig {
	return config.VerdictConfig{Model: model, Active: active, Confidence: 0.8}
}

// TestVerdictTick covers the verdict job: cached INBOX mail from a stranger
// gets a verdict; with the verdict active, a request for money raises a
// payment_request anomaly.
func TestVerdictTick(t *testing.T) {
	f := newHoldFixture(t)
	ctx := context.Background()
	srv := fakeOllama(t, `{"verdict":"scam","asks":"payment","confidence":0.95,"reasons":["urgent wire"]}`)
	f.s.cfg.Intel.Verdict = verdictCfg("fake", true)
	f.s.cfg.Intel.Verdict.URL = srv.URL
	if _, err := f.d.SQL().Exec(`INSERT INTO messages(account, folder, uid, message_id, from_addr, from_name, to_addrs, subject, date, body_text)
		VALUES('test','INBOX',1,'pay@x','ceo@lookalike.example','CEO','["` + imaptest.Username + `"]','Urgent wire',1,'Please wire $9,800 today.')`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.VerdictTick(ctx); err != nil {
		t.Fatal(err)
	}
	v, ok := f.s.verdictFor(ctx, "test", "pay@x")
	if !ok || v.verdict != "scam" || v.asks != "payment" {
		t.Fatalf("verdict = %+v, %v", v, ok)
	}
	var n int
	f.d.StateSQL().QueryRow(`SELECT count(*) FROM anomalies WHERE anomaly_type = 'payment_request' AND message_ref = 'pay@x'`).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("payment_request anomalies = %d", n)
	}
	// Judged once.
	if err := f.s.VerdictTick(ctx); err != nil {
		t.Fatal(err)
	}
	f.d.StateSQL().QueryRow(`SELECT count(*) FROM message_verdicts`).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("verdicts = %d after a second tick", n)
	}
}

func TestEvaluate(t *testing.T) {
	srv := fakeOllama(t, `{"verdict":"scam","asks":"none","confidence":0.85,"reasons":[]}`)
	sample := []labeled{{spam: true}, {spam: true}, {spam: false}}
	res := evaluate(context.Background(), verdict.NewOllama(srv.URL, "fake"), sample)
	if res.Spam != 2 || res.Legit != 1 || len(res.Thresholds) != 4 {
		t.Fatalf("result = %+v", res)
	}
	at := map[float64]EvalThreshold{}
	for _, th := range res.Thresholds {
		at[th.Threshold] = th
	}
	if at[0.8].SpamCaught != 2 || at[0.8].FalsePositives != 1 || at[0.9].SpamCaught != 0 || at[0.8].FPRate != 1 {
		t.Errorf("thresholds = %+v", res.Thresholds)
	}
}
