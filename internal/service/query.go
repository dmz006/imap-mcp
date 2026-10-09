package service

import (
	"context"
	"database/sql"
	"time"

	"github.com/dmz006/imap-mcp/internal/query"
)

// QueryResult is the response of POST /api/query.
type QueryResult struct {
	View      string   `json:"view"`
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated"`
}

// queryTimeout bounds every query (D17).
const queryTimeout = 10 * time.Second

// Query runs a JSON DSL query (AGENT.md D17) against the cache, read-only.
func (s *Service) Query(ctx context.Context, q query.Query) (*QueryResult, error) {
	c, err := query.Compile(q, time.Now())
	if err != nil {
		return nil, invalid("%v", err)
	}
	if s.db == nil || s.db.SQL() == nil {
		return nil, unavailable("the cache is not open in this mode")
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	tx, err := s.db.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.QueryContext(ctx, c.SQL, c.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &QueryResult{View: q.View, Columns: c.Columns, Rows: [][]any{}}
	for rows.Next() {
		vals := make([]any, len(c.Columns))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		if len(res.Rows) == c.Limit {
			res.Truncated = true
			break
		}
		for i, v := range vals {
			vals[i] = render(c.Kinds[i], v)
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Count = len(res.Rows)
	return res, nil
}

// render converts DB values to JSON-friendly ones: times to RFC3339 UTC,
// booleans to true/false, []byte to string.
func render(k query.Kind, v any) any {
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	switch k {
	case query.KindTime:
		if n, ok := v.(int64); ok {
			return time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
	case query.KindBool:
		if n, ok := v.(int64); ok {
			return n != 0
		}
	}
	return v
}
