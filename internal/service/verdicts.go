package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/intel"
	imapsync "github.com/dmz006/imap-mcp/internal/sync"
	"github.com/dmz006/imap-mcp/internal/verdict"
)

// The verdict model (AGENT.md D37, D38, D53, D54): a second opinion on
// borderline and first-contact mail, chosen by evaluation on the owner's own
// labeled mail.

// verdictModel returns the configured verdict model, or nil when none is set.
func (s *Service) verdictModel() verdict.Model {
	v := s.cfg.Intel.Verdict
	if !v.On() {
		return nil
	}
	if v.Provider == "datawatch" {
		return nil // the datawatch LLM proxy needs a peer token (open item); Ollama for now
	}
	url := v.URL
	if url == "" {
		url = s.cfg.Enrichment.OllamaURL
	}
	return verdict.NewOllama(url, v.Model)
}

// inputFromRaw builds the model's view of a raw message: sender, Reply-To,
// whether it is addressed to the owner, the receiving server's
// authentication results, subject and a body excerpt.
func inputFromRaw(raw []byte, me meSet, bodyChars int) verdict.Input {
	var in verdict.Input
	hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err == nil || hdr.Len() > 0 {
		h := mail.Header{Header: message.Header{Header: hdr}}
		if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
			in.FromName, in.FromAddr = from[0].Name, strings.ToLower(from[0].Address)
		}
		if rt, err := h.AddressList("Reply-To"); err == nil && len(rt) > 0 {
			in.ReplyTo = strings.ToLower(rt[0].Address)
		}
		for _, f := range []string{"To", "Cc"} {
			if list, err := h.AddressList(f); err == nil {
				for _, a := range list {
					if me.has(a.Address) {
						in.ToOwner = true
					}
				}
			}
		}
		in.Subject, _ = h.Subject()
		in.Auth = authSummary(hdr.Get("Authentication-Results"))
	}
	text, html := imapsync.PlainText(raw)
	if text == "" {
		text = stripTags(html)
	}
	in.Body = verdict.Excerpt(text, bodyChars)
	return in
}

// authSummary reduces the topmost Authentication-Results header to its DKIM,
// SPF and DMARC results.
func authSummary(ar string) string {
	if ar == "" {
		return "no results"
	}
	var parts []string
	for _, m := range []string{"dkim", "spf", "dmarc"} {
		if i := strings.Index(strings.ToLower(ar), m+"="); i >= 0 {
			v := strings.ToLower(ar[i+len(m)+1:])
			if j := strings.IndexAny(v, " ;()"); j >= 0 {
				v = v[:j]
			}
			parts = append(parts, strings.ToUpper(m)+" "+v)
		}
	}
	if len(parts) == 0 {
		return "no results"
	}
	return strings.Join(parts, ", ")
}

// labeled is one evaluation sample: a raw message and its label.
type labeled struct {
	spam bool
	in   verdict.Input
}

// evalSample builds a balanced labeled sample from the owner's mail (D53):
// mail the new-sender hold held and the owner never released is spam; the
// latest message from people the owner corresponds with is legitimate.
func (s *Service) evalSample(ctx context.Context, perClass int) ([]labeled, error) {
	st := s.db.StateSQL()
	me := s.me(ctx)
	bodyChars := s.cfg.Intel.Verdict.BodyCharsOrDefault()
	var out []labeled

	// Spam: held, not released. Located by Message-ID in the hold folder.
	type ref struct{ account, folder, id string }
	var spam []ref
	rows, err := st.QueryContext(ctx, `SELECT account, COALESCE(folder,''), COALESCE(message_ref,'') FROM held_messages
		WHERE released_at IS NULL AND COALESCE(message_ref,'') <> '' AND COALESCE(folder,'') <> '' ORDER BY random() LIMIT ?`, perClass*2)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r ref
		if rows.Scan(&r.account, &r.folder, &r.id) == nil {
			spam = append(spam, r)
		}
	}
	rows.Close()
	for _, r := range spam {
		if countLabel(out, true) >= perClass || ctx.Err() != nil {
			break
		}
		raw, err := s.rawByMessageID(r.account, r.folder, r.id)
		if err != nil || raw == nil {
			continue
		}
		out = append(out, labeled{spam: true, in: inputFromRaw(raw, me, bodyChars)})
	}

	// Legitimate: the latest message of conversations with correspondents.
	var legit []ExportRef
	// The same automated-sender filter as needs_reply (D51): store notices
	// and no-reply senders are not "legitimate correspondence" for scoring.
	rows, err = st.QueryContext(ctx, `SELECT t.account, t.folder, t.uid, t.counterpart FROM reply_threads t JOIN senders s ON s.address = t.counterpart
		WHERE t.outgoing = 0 AND t.direct = 1 AND COALESCE(t.uid,0) > 0 AND COALESCE(t.folder,'') <> ''
			AND (s.sent_count > 0 OR s.reply_count > 0) AND COALESCE(s.role,'') NOT IN ('bot','newsletter','vendor','self')
			AND NOT (COALESCE(s.message_count,0) > 0
				AND (COALESCE(s.list_count,0) + COALESCE(s.bulk_count,0) + COALESCE(s.auto_count,0)) * 2 > COALESCE(s.message_count,0))
		ORDER BY random() LIMIT ?`, perClass*4)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r ExportRef
		var counterpart string
		if rows.Scan(&r.Account, &r.Folder, &r.UID, &counterpart) == nil && !intel.IsNoReply(counterpart) {
			legit = append(legit, r)
		}
	}
	rows.Close()
	for _, r := range legit {
		if countLabel(out, false) >= perClass || ctx.Err() != nil {
			break
		}
		msgs, _, err := s.fetchRaw([]ExportRef{r}, 5<<20, true)
		if err != nil || len(msgs) == 0 {
			continue
		}
		out = append(out, labeled{spam: false, in: inputFromRaw(msgs[0].data, me, bodyChars)})
	}
	return out, nil
}

func countLabel(l []labeled, spam bool) int {
	n := 0
	for _, x := range l {
		if x.spam == spam {
			n++
		}
	}
	return n
}

// rawByMessageID reads one message (BODY.PEEK[]) found by Message-ID in a folder.
func (s *Service) rawByMessageID(account, folder, id string) ([]byte, error) {
	var raw []byte
	_, err := s.withFolder(account, folder, func(client *imapclient.Client) (int64, error) {
		sd, err := client.UIDSearch(&imaplib.SearchCriteria{Header: []imaplib.SearchCriteriaHeaderField{
			{Key: "Message-ID", Value: strings.Trim(id, "<>")}}}, nil).Wait()
		if err != nil {
			return 0, err
		}
		uids := sd.AllUIDs()
		if len(uids) == 0 {
			return 0, nil
		}
		msgs, err := client.Fetch(imaplib.UIDSetNum(uids[0]), &imaplib.FetchOptions{UID: true,
			BodySection: []*imaplib.FetchItemBodySection{{Peek: true}}}).Collect()
		if err != nil || len(msgs) == 0 || len(msgs[0].BodySection) == 0 {
			return 0, err
		}
		raw = msgs[0].BodySection[0].Bytes
		return 0, nil
	})
	return raw, err
}

// EvalParams starts a verdict evaluation.
type EvalParams struct {
	URL      string   `json:"url"`       // Ollama URL (default: the verdict URL, else enrichment.ollama_url)
	Models   []string `json:"models"`    // Ollama model names
	PerClass int      `json:"per_class"` // samples per label (default 50, max 200)
}

// EvalThreshold is one model's result at one confidence threshold.
type EvalThreshold struct {
	Threshold      float64 `json:"threshold"`
	SpamCaught     int     `json:"spam_caught"`          // spam judged scam/bulk at or above the threshold
	FalsePositives int     `json:"false_positives"`      // legitimate mail judged scam/bulk at or above it
	ScamFP         int     `json:"scam_false_positives"` // legitimate mail judged scam at or above it (what can hold a score-0 message)
	LegitVetted    int     `json:"legit_vetted"`         // legitimate mail judged legitimate at or above it
	SpamRecall     float64 `json:"spam_recall"`
	FPRate         float64 `json:"false_positive_rate"`
}

// EvalModel is one model's evaluation.
type EvalModel struct {
	Model      string          `json:"model"`
	Spam       int             `json:"spam"`
	Legit      int             `json:"legit"`
	Unparsed   int             `json:"unparsed"` // answers that failed the schema
	Errors     int             `json:"errors"`   // calls that failed
	AvgMillis  int64           `json:"avg_ms"`
	AsksOnSpam int             `json:"asks_on_spam"` // spam judged to ask for money or access
	Thresholds []EvalThreshold `json:"thresholds"`
}

// EvalRun is a verdict_evals row.
type EvalRun struct {
	ID         int64       `json:"id"`
	Status     string      `json:"status"`
	Params     EvalParams  `json:"params"`
	Results    []EvalModel `json:"results"`
	Error      string      `json:"error,omitempty"`
	StartedAt  string      `json:"started_at"`
	FinishedAt string      `json:"finished_at,omitempty"`
}

var evalThresholds = []float64{0.6, 0.7, 0.8, 0.9}

// StartVerdictEval starts an evaluation in the background and returns its id
// (D53, D54). Mail bodies go only to the named Ollama server; results are
// counts only.
func (s *Service) StartVerdictEval(ctx context.Context, p EvalParams) (EvalRun, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return EvalRun{}, unavailable("the state database is not open in this mode")
	}
	if len(p.Models) == 0 {
		return EvalRun{}, invalid("models is required (Ollama model names)")
	}
	if p.PerClass <= 0 {
		p.PerClass = 50
	}
	p.PerClass = min(p.PerClass, 200)
	if p.URL == "" {
		p.URL = s.cfg.Intel.Verdict.URL
	}
	if p.URL == "" {
		p.URL = s.cfg.Enrichment.OllamaURL
	}
	params, _ := json.Marshal(p)
	r, err := s.db.StateSQL().ExecContext(ctx, `INSERT INTO verdict_evals(status, params, started_at) VALUES('running', ?, ?)`,
		string(params), time.Now().Unix())
	if err != nil {
		return EvalRun{}, err
	}
	id, _ := r.LastInsertId()
	go s.runVerdictEval(id, p)
	return s.VerdictEval(ctx, id)
}

// RunVerdictEval runs an evaluation in the foreground (the CLI).
func (s *Service) RunVerdictEval(ctx context.Context, p EvalParams, progress func(string)) ([]EvalModel, error) {
	if p.PerClass <= 0 {
		p.PerClass = 50
	}
	sample, err := s.evalSample(ctx, p.PerClass)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		progress(fmt.Sprintf("sample: %d spam, %d legitimate", countLabel(sample, true), countLabel(sample, false)))
	}
	var out []EvalModel
	for _, name := range p.Models {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		res := evaluate(ctx, verdict.NewOllama(p.URL, name), sample)
		if progress != nil {
			progress(fmt.Sprintf("%s: done (%d ms average)", name, res.AvgMillis))
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Service) runVerdictEval(id int64, p EvalParams) {
	ctx := context.Background()
	results, err := s.RunVerdictEval(ctx, p, nil)
	status, errText := "done", ""
	if err != nil {
		status, errText = "failed", err.Error()
	}
	js, _ := json.Marshal(results)
	if _, uerr := s.db.StateSQL().Exec(`UPDATE verdict_evals SET status = ?, results = ?, error = NULLIF(?, ''), finished_at = ? WHERE id = ?`,
		status, string(js), errText, time.Now().Unix(), id); uerr != nil {
		slog.Warn("verdict eval: save results", "id", id, "err", uerr)
	}
}

// evaluate runs one model over the sample.
func evaluate(ctx context.Context, m verdict.Model, sample []labeled) EvalModel {
	res := EvalModel{Model: m.Name()}
	type answer struct {
		spam bool
		r    verdict.Result
		ok   bool
	}
	var answers []answer
	var total time.Duration
	calls := 0
	for _, x := range sample {
		if x.spam {
			res.Spam++
		} else {
			res.Legit++
		}
		start := time.Now()
		raw, err := m.Judge(ctx, verdict.Prompt(x.in))
		total += time.Since(start)
		calls++
		if err != nil {
			res.Errors++
			continue
		}
		r, err := verdict.Parse(raw)
		if err != nil {
			res.Unparsed++
			continue
		}
		answers = append(answers, answer{spam: x.spam, r: r, ok: true})
		if x.spam && r.Asks != "none" {
			res.AsksOnSpam++
		}
	}
	if calls > 0 {
		res.AvgMillis = (total / time.Duration(calls)).Milliseconds()
	}
	for _, t := range evalThresholds {
		th := EvalThreshold{Threshold: t}
		for _, a := range answers {
			flagged := (a.r.Verdict == "scam" || a.r.Verdict == "bulk") && a.r.Confidence >= t
			switch {
			case a.spam && flagged:
				th.SpamCaught++
			case !a.spam && flagged:
				th.FalsePositives++
				if a.r.Verdict == "scam" {
					th.ScamFP++
				}
			case !a.spam && a.r.Verdict == "legitimate" && a.r.Confidence >= t:
				th.LegitVetted++
			}
		}
		if res.Spam > 0 {
			th.SpamRecall = round2(float64(th.SpamCaught) / float64(res.Spam))
		}
		if res.Legit > 0 {
			th.FPRate = round2(float64(th.FalsePositives) / float64(res.Legit))
		}
		res.Thresholds = append(res.Thresholds, th)
	}
	return res
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// VerdictEval returns one evaluation run (0 = the latest).
func (s *Service) VerdictEval(ctx context.Context, id int64) (EvalRun, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return EvalRun{}, unavailable("the state database is not open in this mode")
	}
	q := `SELECT id, status, COALESCE(params,''), COALESCE(results,''), COALESCE(error,''), started_at, COALESCE(finished_at,0) FROM verdict_evals `
	args := []any{}
	if id > 0 {
		q += `WHERE id = ?`
		args = append(args, id)
	} else {
		q += `ORDER BY id DESC LIMIT 1`
	}
	var r EvalRun
	var params, results string
	var started, finished int64
	if err := s.db.StateSQL().QueryRowContext(ctx, q, args...).Scan(&r.ID, &r.Status, &params, &results, &r.Error, &started, &finished); err != nil {
		return r, notFound("no verdict evaluation %d", id)
	}
	_ = json.Unmarshal([]byte(params), &r.Params)
	_ = json.Unmarshal([]byte(results), &r.Results)
	r.StartedAt = time.Unix(started, 0).UTC().Format(time.RFC3339)
	if finished > 0 {
		r.FinishedAt = time.Unix(finished, 0).UTC().Format(time.RFC3339)
	}
	return r, nil
}

// storedVerdict is a message_verdicts row.
type storedVerdict struct {
	verdict, asks string
	confidence    float64
}

// verdictFor returns a message's stored verdict.
func (s *Service) verdictFor(ctx context.Context, account, messageID string) (storedVerdict, bool) {
	var v storedVerdict
	h := intelMsgHash(messageID)
	if h == 0 || s.db == nil || s.db.StateSQL() == nil {
		return v, false
	}
	err := s.db.StateSQL().QueryRowContext(ctx, `SELECT verdict, asks, confidence FROM message_verdicts WHERE account = ? AND msg_hash = ?`,
		account, h).Scan(&v.verdict, &v.asks, &v.confidence)
	return v, err == nil
}

// verdictCandidate is one message waiting for a verdict.
type verdictCandidate struct {
	account, messageID, sender string
	in                         verdict.Input
	ref                        *ExportRef // read live when the cache has no body
}

// VerdictTick judges up to intelligence.verdict.per_tick messages that have
// no verdict yet: cached INBOX mail from senders the owner has never written
// to, and first contacts waiting in reply tracking (D37, D38). With the
// verdict active, a message asking for money or access from such a sender
// raises a payment_request or credential_request anomaly.
func (s *Service) VerdictTick(ctx context.Context) error {
	m := s.verdictModel()
	if m == nil || s.db == nil || s.db.StateSQL() == nil {
		return nil
	}
	cands, err := s.verdictCandidates(ctx, s.cfg.Intel.Verdict.PerTickOrDefault())
	if err != nil {
		return err
	}
	me := s.me(ctx)
	bodyChars := s.cfg.Intel.Verdict.BodyCharsOrDefault()
	for _, c := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		in := c.in
		if c.ref != nil {
			msgs, _, err := s.fetchRaw([]ExportRef{*c.ref}, 5<<20, true)
			if err != nil || len(msgs) == 0 {
				continue
			}
			in = inputFromRaw(msgs[0].data, me, bodyChars)
		}
		raw, err := m.Judge(ctx, verdict.Prompt(in))
		if err != nil {
			slog.Debug("verdict: model call", "err", err)
			return nil // the model is unavailable; try again next tick
		}
		r, err := verdict.Parse(raw)
		if err != nil {
			slog.Debug("verdict: unparsed answer", "err", err)
			r = verdict.Result{Verdict: "legitimate", Asks: "none", Confidence: 0, Reasons: []string{"unparsed answer"}}
		}
		reasons, _ := json.Marshal(r.Reasons)
		if _, err := s.db.StateSQL().ExecContext(ctx, `INSERT OR REPLACE INTO message_verdicts(account, msg_hash, sender, verdict, asks,
				confidence, reasons, model, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			c.account, intelMsgHash(c.messageID), c.sender, r.Verdict, r.Asks, r.Confidence, string(reasons), m.Name(), time.Now().Unix()); err != nil {
			return err
		}
		if s.cfg.Intel.Verdict.Active && r.Asks != "none" && r.Confidence >= s.cfg.Intel.Verdict.ConfidenceOrDefault() {
			if err := s.askAnomaly(ctx, c, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// verdictCandidates picks messages without a verdict: cached INBOX mail from
// non-correspondents first (new mail the hold judges), then first contacts
// in reply tracking (read live when not cached).
func (s *Service) verdictCandidates(ctx context.Context, limit int) ([]verdictCandidate, error) {
	st := s.db.StateSQL()
	me := s.me(ctx)
	bodyChars := s.cfg.Intel.Verdict.BodyCharsOrDefault()
	var out []verdictCandidate
	seen := map[string]bool{}
	correspondent := func(addr string) bool {
		var n int
		_ = st.QueryRowContext(ctx, `SELECT count(*) FROM senders WHERE address = ? AND (sent_count > 0 OR reply_count > 0 OR COALESCE(trusted,0) = 1
			OR COALESCE(role,'') IN ('self','newsletter','bot'))`, addr).Scan(&n)
		return n > 0
	}
	judged := func(account, id string) bool {
		var n int
		_ = st.QueryRowContext(ctx, `SELECT count(*) FROM message_verdicts WHERE account = ? AND msg_hash = ?`, account, intelMsgHash(id)).Scan(&n)
		return n > 0
	}
	if cache := s.db.SQL(); cache != nil {
		rows, err := cache.QueryContext(ctx, `SELECT account, COALESCE(message_id,''), lower(from_addr), COALESCE(from_name,''), COALESCE(reply_to,''),
				COALESCE(to_addrs,''), COALESCE(cc_addrs,''), COALESCE(subject,''), COALESCE(body_text,''), COALESCE(body_html,'')
			FROM messages WHERE upper(folder) = 'INBOX' AND COALESCE(message_id,'') <> '' ORDER BY date DESC LIMIT ?`, limit*20)
		if err != nil {
			return nil, err
		}
		type row struct{ account, id, from, name, replyTo, to, cc, subject, text, html string }
		var all []row
		for rows.Next() {
			var r row
			if rows.Scan(&r.account, &r.id, &r.from, &r.name, &r.replyTo, &r.to, &r.cc, &r.subject, &r.text, &r.html) == nil {
				all = append(all, r)
			}
		}
		rows.Close()
		for _, r := range all {
			if len(out) >= limit {
				return out, nil
			}
			if me.has(r.from) || seen[r.account+r.id] || correspondent(r.from) || judged(r.account, r.id) {
				continue
			}
			seen[r.account+r.id] = true
			text := r.text
			if text == "" {
				text = stripTags(r.html)
			}
			in := verdict.Input{FromName: r.name, FromAddr: r.from, ReplyTo: bareAddr(r.replyTo), Subject: r.subject,
				Auth: s.senderAuth(ctx, r.from), Body: verdict.Excerpt(text, bodyChars)}
			for _, a := range strings.Split(strings.ToLower(r.to+","+r.cc), ",") {
				if me.has(bareAddr(strings.Trim(a, `"[] `))) {
					in.ToOwner = true
				}
			}
			out = append(out, verdictCandidate{account: r.account, messageID: r.id, sender: r.from, in: in})
		}
	}
	rows, err := st.QueryContext(ctx, `SELECT t.account, COALESCE(t.message_ref,''), COALESCE(t.counterpart,''), COALESCE(t.folder,''), COALESCE(t.uid,0)
		FROM reply_threads t WHERE t.outgoing = 0 AND t.direct = 1 AND t.last_date >= ? AND COALESCE(t.uid,0) > 0
		ORDER BY t.last_date DESC LIMIT ?`, time.Now().AddDate(0, 0, -defaultReplyWithinDays).Unix(), limit*20)
	if err != nil {
		return nil, err
	}
	type trow struct {
		account, id, from, folder string
		uid                       uint32
	}
	var threads []trow
	for rows.Next() {
		var r trow
		if rows.Scan(&r.account, &r.id, &r.from, &r.folder, &r.uid) == nil {
			threads = append(threads, r)
		}
	}
	rows.Close()
	for _, r := range threads {
		if len(out) >= limit {
			break
		}
		if r.id == "" || me.has(r.from) || seen[r.account+r.id] || correspondent(r.from) || judged(r.account, r.id) {
			continue
		}
		seen[r.account+r.id] = true
		out = append(out, verdictCandidate{account: r.account, messageID: r.id, sender: r.from,
			ref: &ExportRef{Account: r.account, Folder: r.folder, UID: r.uid}})
	}
	return out, nil
}

// senderAuth summarises a sender's authentication history for the prompt.
func (s *Service) senderAuth(ctx context.Context, addr string) string {
	var dkimP, dkimF, dmarcP, dmarcF int
	if err := s.db.StateSQL().QueryRowContext(ctx, `SELECT dkim_pass, dkim_fail, dmarc_pass, dmarc_fail FROM senders WHERE address = ?`, addr).
		Scan(&dkimP, &dkimF, &dmarcP, &dmarcF); err != nil {
		return "no results"
	}
	word := func(p, f int) string {
		switch {
		case p > 0 && f == 0:
			return "pass"
		case f > 0 && p == 0:
			return "fail"
		case p > 0 && f > 0:
			return "mixed"
		}
		return "none"
	}
	return "DKIM " + word(dkimP, dkimF) + ", DMARC " + word(dmarcP, dmarcF)
}

// askAnomaly records a payment or credential request from a sender the owner
// has never written to (D37): high severity alongside a look-alike domain,
// a Reply-To mismatch or an authentication failure, else medium.
func (s *Service) askAnomaly(ctx context.Context, c verdictCandidate, r verdict.Result) error {
	typ := "payment_request"
	if r.Asks == "credentials" {
		typ = "credential_request"
	}
	st := s.db.StateSQL()
	var risky int
	_ = st.QueryRowContext(ctx, `SELECT count(*) FROM anomalies WHERE sender = ? AND resolved = 0
		AND anomaly_type IN ('lookalike_domain','reply_to_mismatch','auth_failure')`, c.sender).Scan(&risky)
	severity := "medium"
	if risky > 0 {
		severity = "high"
	}
	details, _ := json.Marshal(map[string]any{"asks": r.Asks, "confidence": r.Confidence, "reasons": r.Reasons})
	res, err := st.ExecContext(ctx, `INSERT OR IGNORE INTO anomalies(account, sender, anomaly_type, description, severity, detected_at, message_ref, details)
		VALUES(?,?,?,?,?,?,?,?)`, c.account, c.sender, typ,
		fmt.Sprintf("Asks for %s, from a sender you have never written to", strings.ReplaceAll(r.Asks, "_", " ")),
		severity, time.Now().Unix(), strings.Trim(c.messageID, "<>"), string(details))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		id, _ := res.LastInsertId()
		if b := s.pool.Bus(); b != nil {
			b.Publish(busEvent(c.account, id, typ, severity))
		}
	}
	return nil
}

// busEvent is the anomaly.detected event for a verdict anomaly (D16 fields).
func busEvent(account string, id int64, typ, severity string) bus.Event {
	return bus.Event{Type: bus.EventAnomalyDetected, Account: account,
		Payload: map[string]any{"id": id, "type": typ, "severity": severity}}
}

func intelMsgHash(id string) int64 { return intel.MsgHash(id) }
