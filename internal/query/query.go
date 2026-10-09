// Package query compiles the /api/query JSON DSL (AGENT.md D17) into
// parameterized SQL over a fixed set of read-only views of the cache.
//
// A query never carries SQL: views, fields, operators and aggregate functions
// come from allowlists, every value is a bound parameter, and identifiers in
// the generated SQL come only from this package. Message bodies are returned
// only when a query names them in fields.
package query

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Limits.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
	maxFilters   = 32
	maxInValues  = 500
)

// Kind is a field's value type.
type Kind int

const (
	KindText Kind = iota
	KindInt
	KindReal
	KindBool
	KindTime // unix seconds in the DB; RFC3339, YYYY-MM-DD or -Nd/-Nh in queries
)

func (k Kind) String() string {
	return [...]string{"text", "int", "real", "bool", "time"}[k]
}

// Field is one queryable column of a view.
type Field struct {
	Name    string
	Expr    string // SQL expression, from this package only
	Kind    Kind
	Default bool // returned when a query names no fields
	Body    bool // message content: only returned when named explicitly
}

// View is a fixed, read-only projection of the cache.
type View struct {
	Name   string
	From   string // FROM clause
	Fields []Field
}

func (v *View) field(name string) (Field, bool) {
	for _, f := range v.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Views are the queryable views.
var Views = map[string]*View{
	"messages": {Name: "messages", From: "messages m", Fields: []Field{
		{"id", "m.id", KindInt, true, false},
		{"account", "m.account", KindText, true, false},
		{"folder", "m.folder", KindText, true, false},
		{"uid", "m.uid", KindInt, true, false},
		{"message_id", "m.message_id", KindText, false, false},
		{"thread_id", "m.thread_id", KindText, false, false},
		{"date", "m.date", KindTime, true, false},
		{"internal_date", "m.internal_date", KindTime, false, false},
		{"from_addr", "m.from_addr", KindText, true, false},
		{"from_name", "m.from_name", KindText, false, false},
		{"from_domain", "lower(substr(m.from_addr, instr(m.from_addr, '@') + 1))", KindText, false, false},
		{"to_addrs", "m.to_addrs", KindText, false, false},
		{"cc_addrs", "m.cc_addrs", KindText, false, false},
		{"subject", "m.subject", KindText, true, false},
		{"flags", "m.flags", KindText, false, false},
		{"seen", `(instr(COALESCE(m.flags,''), '\Seen') > 0)`, KindBool, false, false},
		{"flagged", `(instr(COALESCE(m.flags,''), '\Flagged') > 0)`, KindBool, false, false},
		{"answered", `(instr(COALESCE(m.flags,''), '\Answered') > 0)`, KindBool, false, false},
		{"size", "m.size", KindInt, false, false},
		{"has_attachments", "m.has_attachments", KindBool, false, false},
		{"hall", "m.hall", KindText, false, false},
		{"wing", "m.wing", KindText, false, false},
		{"room", "m.room", KindText, false, false},
		{"enrichment_status", "m.enrichment_status", KindText, false, false},
		{"body_text", "m.body_text", KindText, false, true},
		{"body_html", "m.body_html", KindText, false, true},
	}},
	"senders": {Name: "senders", From: "senders s", Fields: []Field{
		{"address", "s.address", KindText, true, false},
		{"name", "s.name", KindText, true, false},
		{"domain", "s.domain", KindText, true, false},
		{"role", "s.role", KindText, true, false},
		{"first_seen", "s.first_seen", KindTime, false, false},
		{"last_seen", "s.last_seen", KindTime, true, false},
		{"message_count", "s.message_count", KindInt, true, false},
		{"sent_count", "s.sent_count", KindInt, false, false},
		{"avg_reply_time", "s.avg_reply_time", KindInt, false, false},
		{"anomaly_score", "s.anomaly_score", KindReal, false, false},
		{"role_source", "s.role_source", KindText, true, false},
		{"reply_count", "s.reply_count", KindInt, false, false},
		{"list_count", "s.list_count", KindInt, false, false},
		{"bulk_count", "s.bulk_count", KindInt, false, false},
		{"auto_count", "s.auto_count", KindInt, false, false},
		{"dkim_pass", "s.dkim_pass", KindInt, false, false},
		{"dkim_fail", "s.dkim_fail", KindInt, false, false},
		{"dmarc_pass", "s.dmarc_pass", KindInt, false, false},
		{"dmarc_fail", "s.dmarc_fail", KindInt, false, false},
	}},
	"anomalies": {Name: "anomalies", From: "anomalies a", Fields: []Field{
		{"id", "a.id", KindInt, true, false},
		{"account", "a.account", KindText, true, false},
		{"message_id", "a.message_id", KindInt, false, false},
		{"sender", "a.sender", KindText, true, false},
		{"type", "a.anomaly_type", KindText, true, false},
		{"description", "a.description", KindText, false, false},
		{"severity", "a.severity", KindText, true, false},
		{"detected_at", "a.detected_at", KindTime, true, false},
		{"resolved", "a.resolved", KindBool, true, false},
	}},
	"kg": {Name: "kg", From: "kg_relationships r JOIN kg_entities se ON se.id = r.subject_id JOIN kg_entities oe ON oe.id = r.object_id", Fields: []Field{
		{"subject", "se.name", KindText, true, false},
		{"subject_type", "se.entity_type", KindText, true, false},
		{"predicate", "r.predicate", KindText, true, false},
		{"object", "oe.name", KindText, true, false},
		{"object_type", "oe.entity_type", KindText, true, false},
		{"valid_from", "r.valid_from", KindTime, false, false},
		{"valid_to", "r.valid_to", KindTime, false, false},
		{"current", "(r.valid_to IS NULL)", KindBool, false, false},
		{"confidence", "r.confidence", KindReal, true, false},
	}},
}

// Query is the request body of POST /api/query.
type Query struct {
	View      string      `json:"view"`
	Fields    []string    `json:"fields,omitempty"`
	Where     []Filter    `json:"where,omitempty"`
	GroupBy   []string    `json:"group_by,omitempty"`
	Aggregate []Aggregate `json:"aggregate,omitempty"`
	OrderBy   []Order     `json:"order_by,omitempty"`
	Limit     int         `json:"limit,omitempty"`
	Offset    int         `json:"offset,omitempty"`
}

// Filter is one condition; all filters are ANDed.
type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value any    `json:"value,omitempty"`
}

// Aggregate is one aggregate column. Field is required except for count.
type Aggregate struct {
	Fn    string `json:"fn"`
	Field string `json:"field,omitempty"`
	As    string `json:"as,omitempty"`
}

// Order sorts by a returned column (field name or aggregate alias).
type Order struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// Ops are the filter operators.
var Ops = []string{"eq", "ne", "in", "nin", "lt", "lte", "gt", "gte", "contains", "prefix", "is_null", "not_null"}

// Fns are the aggregate functions.
var Fns = []string{"count", "count_distinct", "min", "max", "sum", "avg"}

// Compiled is a ready-to-run statement.
type Compiled struct {
	SQL     string
	Args    []any
	Columns []string
	Kinds   []Kind // per column, for rendering (time → RFC3339)
	Limit   int
}

var aliasRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// Compile validates q and builds parameterized SQL. It fetches Limit+1 rows
// so the caller can report truncation.
func Compile(q Query, now time.Time) (*Compiled, error) {
	v, ok := Views[q.View]
	if !ok {
		return nil, fmt.Errorf("view must be one of %s", strings.Join(viewNames(), ", "))
	}
	c := &Compiled{}
	var sel, group []string

	// Group-by fields and aggregates, or plain fields.
	grouped := len(q.GroupBy) > 0 || len(q.Aggregate) > 0
	if grouped && len(q.Fields) > 0 {
		return nil, fmt.Errorf("use group_by + aggregate or fields, not both")
	}
	if grouped {
		for _, name := range q.GroupBy {
			f, ok := v.field(name)
			if !ok {
				return nil, unknownField(v, name)
			}
			if f.Body {
				return nil, fmt.Errorf("cannot group by %s", name)
			}
			if slices.Contains(c.Columns, name) {
				return nil, fmt.Errorf("duplicate group_by field %s", name)
			}
			sel = append(sel, f.Expr)
			group = append(group, f.Expr)
			c.Columns = append(c.Columns, name)
			c.Kinds = append(c.Kinds, f.Kind)
		}
		aggs := q.Aggregate
		if len(aggs) == 0 {
			aggs = []Aggregate{{Fn: "count"}}
		}
		for _, a := range aggs {
			expr, kind, alias, err := compileAgg(v, a)
			if err != nil {
				return nil, err
			}
			if slices.Contains(c.Columns, alias) {
				return nil, fmt.Errorf("duplicate column %s; set a distinct \"as\"", alias)
			}
			sel = append(sel, expr)
			c.Columns = append(c.Columns, alias)
			c.Kinds = append(c.Kinds, kind)
		}
	} else {
		names := q.Fields
		if len(names) == 0 {
			for _, f := range v.Fields {
				if f.Default {
					names = append(names, f.Name)
				}
			}
		}
		for _, name := range names {
			f, ok := v.field(name)
			if !ok {
				return nil, unknownField(v, name)
			}
			if slices.Contains(c.Columns, name) {
				return nil, fmt.Errorf("duplicate field %s", name)
			}
			sel = append(sel, f.Expr)
			c.Columns = append(c.Columns, name)
			c.Kinds = append(c.Kinds, f.Kind)
		}
	}

	// WHERE.
	if len(q.Where) > maxFilters {
		return nil, fmt.Errorf("at most %d filters", maxFilters)
	}
	var where []string
	for _, flt := range q.Where {
		cond, args, err := compileFilter(v, flt, now)
		if err != nil {
			return nil, err
		}
		where = append(where, cond)
		c.Args = append(c.Args, args...)
	}

	// ORDER BY: only returned columns, referenced by position.
	var order []string
	for _, o := range q.OrderBy {
		i := slices.Index(c.Columns, o.Field)
		if i < 0 {
			return nil, fmt.Errorf("order_by %q must be a returned column (%s)", o.Field, strings.Join(c.Columns, ", "))
		}
		dir := "ASC"
		if o.Desc {
			dir = "DESC"
		}
		order = append(order, strconv.Itoa(i+1)+" "+dir)
	}

	c.Limit = q.Limit
	if c.Limit <= 0 {
		c.Limit = DefaultLimit
	}
	if c.Limit > MaxLimit {
		return nil, fmt.Errorf("limit must be at most %d", MaxLimit)
	}
	if q.Offset < 0 {
		return nil, fmt.Errorf("offset must be >= 0")
	}

	var b strings.Builder
	b.WriteString("SELECT " + strings.Join(sel, ", ") + " FROM " + v.From)
	if len(where) > 0 {
		b.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	if len(group) > 0 {
		b.WriteString(" GROUP BY " + strings.Join(group, ", "))
	}
	if len(order) > 0 {
		b.WriteString(" ORDER BY " + strings.Join(order, ", "))
	}
	b.WriteString(" LIMIT ? OFFSET ?")
	c.Args = append(c.Args, c.Limit+1, q.Offset)
	c.SQL = b.String()
	return c, nil
}

func compileAgg(v *View, a Aggregate) (expr string, kind Kind, alias string, err error) {
	if !slices.Contains(Fns, a.Fn) {
		return "", 0, "", fmt.Errorf("aggregate fn must be one of %s", strings.Join(Fns, ", "))
	}
	alias = a.As
	if alias == "" {
		alias = a.Fn
		if a.Field != "" {
			alias += "_" + a.Field
		}
	}
	if !aliasRe.MatchString(alias) {
		return "", 0, "", fmt.Errorf("aggregate alias %q must match %s", alias, aliasRe)
	}
	if a.Fn == "count" && a.Field == "" {
		return "count(*)", KindInt, alias, nil
	}
	f, ok := v.field(a.Field)
	if !ok {
		return "", 0, "", unknownField(v, a.Field)
	}
	if f.Body && a.Fn != "count" && a.Fn != "count_distinct" {
		return "", 0, "", fmt.Errorf("cannot %s %s", a.Fn, a.Field)
	}
	switch a.Fn {
	case "count":
		return "count(" + f.Expr + ")", KindInt, alias, nil
	case "count_distinct":
		return "count(DISTINCT " + f.Expr + ")", KindInt, alias, nil
	case "min", "max":
		return a.Fn + "(" + f.Expr + ")", f.Kind, alias, nil
	default: // sum, avg
		if f.Kind != KindInt && f.Kind != KindReal && f.Kind != KindBool {
			return "", 0, "", fmt.Errorf("%s needs a numeric field; %s is %s", a.Fn, a.Field, f.Kind)
		}
		k := KindReal
		if a.Fn == "sum" && f.Kind != KindReal {
			k = KindInt
		}
		return a.Fn + "(" + f.Expr + ")", k, alias, nil
	}
}

func compileFilter(v *View, flt Filter, now time.Time) (string, []any, error) {
	f, ok := v.field(flt.Field)
	if !ok {
		return "", nil, unknownField(v, flt.Field)
	}
	if !slices.Contains(Ops, flt.Op) {
		return "", nil, fmt.Errorf("op must be one of %s", strings.Join(Ops, ", "))
	}
	switch flt.Op {
	case "is_null":
		return f.Expr + " IS NULL", nil, nil
	case "not_null":
		return f.Expr + " IS NOT NULL", nil, nil
	case "in", "nin":
		vals, ok := flt.Value.([]any)
		if !ok || len(vals) == 0 || len(vals) > maxInValues {
			return "", nil, fmt.Errorf("%s on %s needs a list of 1..%d values", flt.Op, f.Name, maxInValues)
		}
		args := make([]any, 0, len(vals))
		for _, x := range vals {
			a, err := coerce(f, x, now)
			if err != nil {
				return "", nil, err
			}
			args = append(args, a)
		}
		ph := strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")
		not := ""
		if flt.Op == "nin" {
			not = "NOT "
		}
		return f.Expr + " " + not + "IN (" + ph + ")", args, nil
	case "contains", "prefix":
		if f.Kind != KindText {
			return "", nil, fmt.Errorf("%s needs a text field; %s is %s", flt.Op, f.Name, f.Kind)
		}
		s, ok := flt.Value.(string)
		if !ok || s == "" {
			return "", nil, fmt.Errorf("%s on %s needs a non-empty string", flt.Op, f.Name)
		}
		pat := escapeLike(s) + "%"
		if flt.Op == "contains" {
			pat = "%" + pat
		}
		return f.Expr + ` LIKE ? ESCAPE '\'`, []any{pat}, nil
	}
	a, err := coerce(f, flt.Value, now)
	if err != nil {
		return "", nil, err
	}
	op := map[string]string{"eq": "=", "ne": "!=", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}[flt.Op]
	if f.Kind == KindBool && op != "=" && op != "!=" {
		return "", nil, fmt.Errorf("%s supports only eq/ne", f.Name)
	}
	return f.Expr + " " + op + " ?", []any{a}, nil
}

// coerce converts a JSON value to the field's DB representation.
func coerce(f Field, x any, now time.Time) (any, error) {
	bad := func() error { return fmt.Errorf("value %v is not a valid %s for %s", x, f.Kind, f.Name) }
	switch f.Kind {
	case KindText:
		if s, ok := x.(string); ok {
			return s, nil
		}
	case KindInt:
		if n, ok := x.(float64); ok && n == float64(int64(n)) {
			return int64(n), nil
		}
	case KindReal:
		if n, ok := x.(float64); ok {
			return n, nil
		}
	case KindBool:
		if b, ok := x.(bool); ok {
			if b {
				return 1, nil
			}
			return 0, nil
		}
	case KindTime:
		switch t := x.(type) {
		case float64:
			return int64(t), nil
		case string:
			if ts, err := ParseTime(t, now); err == nil {
				return ts.Unix(), nil
			}
		}
	}
	return nil, bad()
}

var relRe = regexp.MustCompile(`^-(\d{1,5})([dhm])$`)

// ParseTime accepts RFC3339, YYYY-MM-DD (UTC midnight) or a relative offset
// before now: -30d, -12h, -15m.
func ParseTime(s string, now time.Time) (time.Time, error) {
	if m := relRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}[m[2]]
		return now.Add(-time.Duration(n) * unit), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func unknownField(v *View, name string) error {
	names := make([]string, len(v.Fields))
	for i, f := range v.Fields {
		names[i] = f.Name
	}
	return fmt.Errorf("unknown field %q for view %s (fields: %s)", name, v.Name, strings.Join(names, ", "))
}

func viewNames() []string {
	out := make([]string, 0, len(Views))
	for n := range Views {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Describe returns the views and their fields, for GET /api/query.
func Describe() map[string]any {
	views := map[string]any{}
	for name, v := range Views {
		fields := []map[string]any{}
		for _, f := range v.Fields {
			m := map[string]any{"name": f.Name, "type": f.Kind.String()}
			if f.Default {
				m["default"] = true
			}
			if f.Body {
				m["body"] = true
			}
			fields = append(fields, m)
		}
		views[name] = fields
	}
	return map[string]any{"views": views, "ops": Ops, "aggregates": Fns, "max_limit": MaxLimit}
}
