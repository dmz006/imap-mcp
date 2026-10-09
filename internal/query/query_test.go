package query

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func TestCompileRejects(t *testing.T) {
	cases := map[string]Query{
		"unknown view":          {View: "rules"},
		"unknown field":         {View: "messages", Fields: []string{"password"}},
		"sql in field":          {View: "messages", Fields: []string{"id; DROP TABLE messages"}},
		"unknown op":            {View: "messages", Where: []Filter{{Field: "uid", Op: "~", Value: 1.0}}},
		"wrong type":            {View: "messages", Where: []Filter{{Field: "uid", Op: "eq", Value: "1 OR 1=1"}}},
		"contains on int":       {View: "messages", Where: []Filter{{Field: "uid", Op: "contains", Value: "1"}}},
		"empty in":              {View: "messages", Where: []Filter{{Field: "uid", Op: "in", Value: []any{}}}},
		"bad alias":             {View: "messages", GroupBy: []string{"account"}, Aggregate: []Aggregate{{Fn: "count", As: "n) FROM rules --"}}},
		"bad fn":                {View: "messages", Aggregate: []Aggregate{{Fn: "group_concat", Field: "subject"}}},
		"sum text":              {View: "messages", Aggregate: []Aggregate{{Fn: "sum", Field: "subject"}}},
		"max body":              {View: "messages", Aggregate: []Aggregate{{Fn: "max", Field: "body_text"}}},
		"group by body":         {View: "messages", GroupBy: []string{"body_text"}},
		"fields and group":      {View: "messages", Fields: []string{"id"}, GroupBy: []string{"account"}},
		"order not returned":    {View: "messages", Fields: []string{"id"}, OrderBy: []Order{{Field: "date"}}},
		"limit too big":         {View: "messages", Limit: MaxLimit + 1},
		"negative offset":       {View: "messages", Offset: -1},
		"bool range":            {View: "messages", Where: []Filter{{Field: "seen", Op: "gt", Value: true}}},
		"bad time":              {View: "messages", Where: []Filter{{Field: "date", Op: "gte", Value: "yesterday"}}},
		"duplicate agg columns": {View: "messages", Aggregate: []Aggregate{{Fn: "count"}, {Fn: "count"}}},
	}
	for name, q := range cases {
		if _, err := Compile(q, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDefaultsExcludeBodies(t *testing.T) {
	c, err := Compile(Query{View: "messages"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.SQL, "body_") || strings.Join(c.Columns, ",") != "id,account,folder,uid,date,from_addr,subject" {
		t.Errorf("default columns = %v, sql = %s", c.Columns, c.SQL)
	}
	c, _ = Compile(Query{View: "messages", Fields: []string{"id", "body_text"}}, now)
	if !strings.Contains(c.SQL, "m.body_text") {
		t.Error("named body field not selected")
	}
}

func TestValuesAreParameters(t *testing.T) {
	evil := "x' OR '1'='1"
	c, err := Compile(Query{View: "messages",
		Where: []Filter{
			{Field: "from_addr", Op: "contains", Value: "50%_off\\"},
			{Field: "subject", Op: "eq", Value: evil},
			{Field: "account", Op: "in", Value: []any{"a", "b"}},
			{Field: "date", Op: "gte", Value: "-30d"},
			{Field: "seen", Op: "eq", Value: false},
		}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.SQL, evil) || strings.Contains(c.SQL, "50%") {
		t.Fatalf("value inlined: %s", c.SQL)
	}
	want := []any{`%50\%\_off\\%`, evil, "a", "b", now.Add(-30 * 24 * time.Hour).Unix(), 0, DefaultLimit + 1, 0}
	if len(c.Args) != len(want) {
		t.Fatalf("args = %v", c.Args)
	}
	for i := range want {
		if c.Args[i] != want[i] {
			t.Errorf("arg %d = %#v, want %#v", i, c.Args[i], want[i])
		}
	}
	if !strings.Contains(c.SQL, `LIKE ? ESCAPE '\'`) || !strings.Contains(c.SQL, "m.account IN (?, ?)") {
		t.Errorf("sql = %s", c.SQL)
	}
}

func TestGroupedQuery(t *testing.T) {
	c, err := Compile(Query{View: "messages", GroupBy: []string{"from_domain"},
		Aggregate: []Aggregate{{Fn: "count", As: "n"}, {Fn: "max", Field: "date", As: "last"}},
		OrderBy:   []Order{{Field: "n", Desc: true}}, Limit: 10}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Columns, ",") != "from_domain,n,last" || c.Kinds[2] != KindTime {
		t.Errorf("columns = %v kinds = %v", c.Columns, c.Kinds)
	}
	if !strings.Contains(c.SQL, "GROUP BY lower(substr(") || !strings.Contains(c.SQL, "ORDER BY 2 DESC") {
		t.Errorf("sql = %s", c.SQL)
	}
	// Aggregate only → one row.
	c, err = Compile(Query{View: "senders"}, now)
	if err != nil || strings.Contains(c.SQL, "GROUP BY") {
		t.Errorf("plain senders: %v %s", err, c.SQL)
	}
	if c, err = Compile(Query{View: "kg", Aggregate: []Aggregate{{Fn: "count"}}}, now); err != nil || !strings.Contains(c.SQL, "JOIN kg_entities") {
		t.Errorf("kg count: %v", err)
	}
}

func TestParseTime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"-2d":                  now.Add(-48 * time.Hour),
		"-3h":                  now.Add(-3 * time.Hour),
		"2026-09-01":           time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		"2026-09-01T10:00:00Z": time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	} {
		got, err := ParseTime(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s = %v, %v", in, got, err)
		}
	}
}

func TestDescribe(t *testing.T) {
	d := Describe()
	if len(d["views"].(map[string]any)) != len(Views) {
		t.Error("describe views")
	}
}
