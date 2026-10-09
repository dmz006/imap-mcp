package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/enrichment"
)

// SemanticParams selects messages similar to a query or a reference message.
type SemanticParams struct {
	Account, Folder string
	Query           string
	ReferenceUID    uint32 // with Folder (and Account) identifies the reference message
	Limit           int
	Threshold       float64
}

// SemanticHit is one cached message ranked by similarity.
type SemanticHit struct {
	Account string  `json:"account"`
	Folder  string  `json:"folder"`
	UID     uint32  `json:"uid"`
	Subject string  `json:"subject"`
	From    string  `json:"from"`
	Date    string  `json:"date"`
	Hall    string  `json:"hall,omitempty"`
	Score   float64 `json:"score"`
}

// SemanticResult lists hits and how much of the cache was searchable.
type SemanticResult struct {
	Hits     []SemanticHit `json:"hits"`
	Searched int           `json:"searched"` // messages with vectors in scope
	Note     string        `json:"note,omitempty"`
}

// SemanticSearch ranks cached, enriched messages by cosine similarity.
// Only mail inside the sync window that has been enriched is searchable.
func (s *Service) SemanticSearch(ctx context.Context, p SemanticParams) (SemanticResult, error) {
	res := SemanticResult{Hits: []SemanticHit{}}
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 10
	}
	if p.Threshold <= 0 || p.Threshold > 1 {
		p.Threshold = 0.7
	}
	if s.db.SQL() == nil {
		return res, unavailable("the mail cache is not open in this mode")
	}
	var query []float32
	var refID int64
	switch {
	case p.Query != "":
		if s.pipeline == nil {
			return res, unavailable("enrichment pipeline is not running; query embedding unavailable")
		}
		v, err := s.pipeline.EmbedQuery(ctx, p.Query)
		if err != nil {
			return res, upstream("embed query", err)
		}
		query = v
	case p.ReferenceUID != 0:
		if p.Folder == "" {
			return res, invalid("reference_uid needs folder")
		}
		acct := s.accountName(p.Account)
		var blob []byte
		err := s.db.SQL().QueryRowContext(ctx, `SELECT m.id, v.vector FROM messages m JOIN message_vectors v ON v.message_id = m.id
			WHERE m.account = ? AND m.folder = ? AND m.uid = ?`, acct, p.Folder, p.ReferenceUID).Scan(&refID, &blob)
		if err == sql.ErrNoRows {
			return res, notFound("reference message uid=%d in %s is not cached and enriched", p.ReferenceUID, p.Folder)
		}
		if err != nil {
			return res, err
		}
		query = enrichment.BlobToFloat32s(blob)
	default:
		return res, invalid("query or reference_uid is required")
	}

	where, args := []string{"1=1"}, []any{}
	if p.Account != "" {
		where, args = append(where, "m.account = ?"), append(args, s.accountName(p.Account))
	}
	if p.Folder != "" && p.ReferenceUID == 0 {
		where, args = append(where, "m.folder = ?"), append(args, p.Folder)
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT m.id, m.account, m.folder, m.uid, COALESCE(m.subject,''), m.from_addr,
		COALESCE(m.internal_date, m.date), COALESCE(m.hall,''), v.vector
		FROM messages m JOIN message_vectors v ON v.message_id = m.id WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, date int64
		var h SemanticHit
		var blob []byte
		if err := rows.Scan(&id, &h.Account, &h.Folder, &h.UID, &h.Subject, &h.From, &date, &h.Hall, &blob); err != nil {
			return res, err
		}
		res.Searched++
		if id == refID {
			continue
		}
		h.Score = float64(enrichment.CosineSimilarity(query, enrichment.BlobToFloat32s(blob)))
		if h.Score >= p.Threshold {
			h.Date = time.Unix(date, 0).UTC().Format(time.RFC3339)
			res.Hits = append(res.Hits, h)
		}
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	sort.Slice(res.Hits, func(i, j int) bool { return res.Hits[i].Score > res.Hits[j].Score })
	if len(res.Hits) > p.Limit {
		res.Hits = res.Hits[:p.Limit]
	}
	if res.Searched == 0 {
		res.Note = "no enriched messages in scope yet; see enrichment_status"
	}
	return res, nil
}

func (s *Service) accountName(account string) string {
	if account == "" || account == "_default" {
		return s.cfg.DefaultAccount().Name
	}
	return account
}

// Sender is a sender profile row, built by the header scanner (internal/intel).
type Sender struct {
	Address      string          `json:"address"`
	Name         string          `json:"name,omitempty"`
	Domain       string          `json:"domain,omitempty"`
	Role         string          `json:"role"`
	RoleSource   string          `json:"role_source,omitempty"`
	FirstSeen    int64           `json:"first_seen,omitempty"`
	LastSeen     int64           `json:"last_seen,omitempty"`
	MessageCount int             `json:"message_count"`
	SentCount    int             `json:"sent_count"`
	AvgReplySecs int64           `json:"avg_reply_seconds,omitempty"`
	ReplyCount   int             `json:"reply_count"`
	ListCount    int             `json:"list_count"`
	BulkCount    int             `json:"bulk_count"`
	AutoCount    int             `json:"auto_count"`
	DKIMPass     int             `json:"dkim_pass"`
	DKIMFail     int             `json:"dkim_fail"`
	DMARCPass    int             `json:"dmarc_pass"`
	DMARCFail    int             `json:"dmarc_fail"`
	AnomalyScore float64         `json:"anomaly_score"`
	Profile      json.RawMessage `json:"profile,omitempty"`
}

const senderCols = `address, COALESCE(name,''), COALESCE(domain,''), COALESCE(role,'unknown'), COALESCE(role_source,''),
	COALESCE(first_seen,0), COALESCE(last_seen,0), COALESCE(message_count,0), COALESCE(sent_count,0), COALESCE(avg_reply_time,0),
	reply_count, list_count, bulk_count, auto_count, dkim_pass, dkim_fail, dmarc_pass, dmarc_fail,
	COALESCE(anomaly_score,0), COALESCE(profile_json,'')`

func scanSender(sc interface{ Scan(...any) error }) (Sender, error) {
	var x Sender
	var profile string
	err := sc.Scan(&x.Address, &x.Name, &x.Domain, &x.Role, &x.RoleSource, &x.FirstSeen, &x.LastSeen, &x.MessageCount, &x.SentCount,
		&x.AvgReplySecs, &x.ReplyCount, &x.ListCount, &x.BulkCount, &x.AutoCount, &x.DKIMPass, &x.DKIMFail, &x.DMARCPass, &x.DMARCFail,
		&x.AnomalyScore, &profile)
	if profile != "" && json.Valid([]byte(profile)) {
		x.Profile = json.RawMessage(profile)
	}
	return x, err
}

// ListSenders returns sender profiles, most active first.
func (s *Service) ListSenders(ctx context.Context, role, domain string, limit int) ([]Sender, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return nil, unavailable("the state database is not open in this mode")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	where, args := []string{"1=1"}, []any{}
	if role != "" {
		where, args = append(where, "role = ?"), append(args, role)
	}
	if domain != "" {
		where, args = append(where, "domain = ?"), append(args, strings.ToLower(domain))
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT `+senderCols+` FROM senders WHERE `+strings.Join(where, " AND ")+
		` ORDER BY message_count DESC, address LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sender{}
	for rows.Next() {
		x, err := scanSender(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SenderProfile is a sender with its relationships, anomalies and cached mail count.
type SenderProfile struct {
	Sender
	CachedMessages int `json:"cached_messages"`
	// ScanComplete is false while the first header scan of all history is
	// still running: counts and roles are partial until then.
	ScanComplete  bool      `json:"scan_complete"`
	Relationships []KGEdge  `json:"relationships"`
	Anomalies     []Anomaly `json:"anomalies"`
}

// GetSenderProfile returns a sender's profile. A sender with cached mail but
// no profile yet (intelligence not run) still returns counts.
func (s *Service) GetSenderProfile(ctx context.Context, address string) (SenderProfile, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" {
		return SenderProfile{}, invalid("address is required")
	}
	if s.db == nil || s.db.StateSQL() == nil {
		return SenderProfile{}, unavailable("the state database is not open in this mode")
	}
	p := SenderProfile{Relationships: []KGEdge{}, Anomalies: []Anomaly{}}
	x, err := scanSender(s.db.StateSQL().QueryRowContext(ctx, `SELECT `+senderCols+` FROM senders WHERE lower(address) = ?`, address))
	if err != nil && err != sql.ErrNoRows {
		return p, err
	}
	p.Sender = x
	if s.db.SQL() != nil {
		s.db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE lower(from_addr) = ?`, address).Scan(&p.CachedMessages) //nolint:errcheck
	}
	if err == sql.ErrNoRows && p.CachedMessages == 0 {
		return p, notFound("no profile or cached mail for %s", address)
	}
	p.Address = address
	if p.Role == "" {
		p.Role = "unknown"
	}
	if st, err := s.IntelStats(ctx); err == nil {
		p.ScanComplete = st.BackfillComplete
	}
	if p.Relationships, err = s.KGQuery(ctx, KGParams{Entity: address, Limit: 50}); err != nil {
		return p, err
	}
	if p.Anomalies, err = s.Anomalies(ctx, AnomalyParams{Sender: address, Limit: 20}); err != nil {
		return p, err
	}
	return p, nil
}

// SenderHistory lists cached messages from an address, newest first. The
// cache only holds mail inside the sync window.
func (s *Service) SenderHistory(ctx context.Context, account, address string, limit int) ([]SemanticHit, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if address == "" {
		return nil, invalid("address is required")
	}
	if s.db.SQL() == nil {
		return nil, unavailable("the mail cache is not open in this mode")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	where, args := "lower(from_addr) = ?", []any{address}
	if account != "" {
		where, args = where+" AND account = ?", append(args, s.accountName(account))
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT account, folder, uid, COALESCE(subject,''), from_addr, COALESCE(internal_date, date), COALESCE(hall,'')
		FROM messages WHERE `+where+` ORDER BY COALESCE(internal_date, date) DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SemanticHit{}
	for rows.Next() {
		var h SemanticHit
		var date int64
		if err := rows.Scan(&h.Account, &h.Folder, &h.UID, &h.Subject, &h.From, &date, &h.Hall); err != nil {
			return nil, err
		}
		h.Date = time.Unix(date, 0).UTC().Format(time.RFC3339)
		out = append(out, h)
	}
	return out, rows.Err()
}

// KGParams filters knowledge-graph edges.
type KGParams struct {
	Entity, Predicate, EntityType string
	Limit                         int
}

// KGEdge is one relationship between two entities.
type KGEdge struct {
	Subject     string  `json:"subject"`
	SubjectType string  `json:"subject_type"`
	Predicate   string  `json:"predicate"`
	Object      string  `json:"object"`
	ObjectType  string  `json:"object_type"`
	ValidFrom   int64   `json:"valid_from,omitempty"`
	ValidTo     int64   `json:"valid_to,omitempty"`
	Confidence  float64 `json:"confidence"`
	// Weight is the number of messages supporting the edge; LastSeen the
	// latest one. Current is false once valid_to is set (stale history).
	Weight     int             `json:"weight"`
	LastSeen   int64           `json:"last_seen,omitempty"`
	Current    bool            `json:"current"`
	Properties json.RawMessage `json:"properties,omitempty"`
}

// KGQuery returns relationships touching an entity (as subject or object).
func (s *Service) KGQuery(ctx context.Context, p KGParams) ([]KGEdge, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return nil, unavailable("the state database is not open in this mode")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 50
	}
	where, args := []string{"1=1"}, []any{}
	if p.Entity != "" {
		where, args = append(where, "(lower(a.name) = lower(?) OR lower(b.name) = lower(?))"), append(args, p.Entity, p.Entity)
	}
	if p.Predicate != "" {
		where, args = append(where, "r.predicate = ?"), append(args, p.Predicate)
	}
	if p.EntityType != "" {
		where, args = append(where, "(a.entity_type = ? OR b.entity_type = ?)"), append(args, p.EntityType, p.EntityType)
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT a.name, a.entity_type, r.predicate, b.name, b.entity_type,
		COALESCE(r.valid_from,0), COALESCE(r.valid_to,0), COALESCE(r.confidence,1), r.weight, COALESCE(r.last_seen,0), COALESCE(r.properties,'')
		FROM kg_relationships r JOIN kg_entities a ON a.id = r.subject_id JOIN kg_entities b ON b.id = r.object_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY r.weight DESC, r.last_seen DESC LIMIT ?`, append(args, p.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KGEdge{}
	for rows.Next() {
		var e KGEdge
		var props string
		if err := rows.Scan(&e.Subject, &e.SubjectType, &e.Predicate, &e.Object, &e.ObjectType, &e.ValidFrom, &e.ValidTo, &e.Confidence,
			&e.Weight, &e.LastSeen, &props); err != nil {
			return nil, err
		}
		e.Current = e.ValidTo == 0
		if props != "" && json.Valid([]byte(props)) {
			e.Properties = json.RawMessage(props)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AnomalyParams filters the anomaly log.
type AnomalyParams struct {
	Account, Severity, Sender string
	IncludeResolved           bool
	Limit                     int
}

// Anomaly is one detected behaviour change.
type Anomaly struct {
	ID          int64  `json:"id"`
	Account     string `json:"account"`
	Sender      string `json:"sender,omitempty"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Severity    string `json:"severity"`
	DetectedAt  int64  `json:"detected_at"`
	Resolved    bool   `json:"resolved"`
}

// Anomalies lists detected anomalies, newest first.
func (s *Service) Anomalies(ctx context.Context, p AnomalyParams) ([]Anomaly, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return nil, unavailable("the state database is not open in this mode")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 20
	}
	switch p.Severity {
	case "", "low", "medium", "high":
	default:
		return nil, invalid("severity must be low, medium or high")
	}
	where, args := []string{"1=1"}, []any{}
	if p.Account != "" {
		where, args = append(where, "account = ?"), append(args, s.accountName(p.Account))
	}
	if p.Severity != "" {
		where, args = append(where, "severity = ?"), append(args, p.Severity)
	}
	if p.Sender != "" {
		where, args = append(where, "lower(sender) = lower(?)"), append(args, p.Sender)
	}
	if !p.IncludeResolved {
		where = append(where, "resolved = 0")
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT id, account, COALESCE(sender,''), anomaly_type, COALESCE(description,''),
		COALESCE(severity,'low'), COALESCE(detected_at,0), resolved FROM anomalies WHERE `+strings.Join(where, " AND ")+
		` ORDER BY detected_at DESC LIMIT ?`, append(args, p.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Anomaly{}
	for rows.Next() {
		var a Anomaly
		var resolved int
		if err := rows.Scan(&a.ID, &a.Account, &a.Sender, &a.Type, &a.Description, &a.Severity, &a.DetectedAt, &resolved); err != nil {
			return nil, err
		}
		a.Resolved = resolved != 0
		out = append(out, a)
	}
	return out, rows.Err()
}
