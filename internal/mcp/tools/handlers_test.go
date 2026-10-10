package tools

import "testing"

// TestResultStructuredContent: objects keep structuredContent; lists are text
// only, because MCP requires structuredContent to be an object.
func TestResultStructuredContent(t *testing.T) {
	r, err := result([]string{"INBOX", "Sent"}, nil)
	if err != nil || r.StructuredContent != nil || len(r.Content) != 1 {
		t.Fatalf("list result = %+v, %v", r, err)
	}
	r, err = result(map[string]int{"count": 1}, nil)
	if err != nil || r.StructuredContent == nil {
		t.Fatalf("object result = %+v, %v", r, err)
	}
	r, _ = result([]string(nil), nil)
	if r.StructuredContent != nil {
		t.Fatal("null result must not be structured")
	}
}
