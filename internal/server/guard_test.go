package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct {
		name     string
		bind     string
		method   string
		path     string
		host     string
		origin   string
		ctype    string
		wantCode int
	}{
		{"loopback GET", "127.0.0.1", "GET", "/api/health", "127.0.0.1:8765", "", "", 200},
		{"localhost GET", "127.0.0.1", "GET", "/api/health", "localhost:8765", "", "", 200},
		{"ipv6 loopback", "127.0.0.1", "GET", "/api/health", "[::1]:8765", "", "", 200},
		{"json POST (datawatch/curl)", "127.0.0.1", "POST", "/api/accounts/x/messages/send", "localhost:8765", "", "application/json", 200},
		{"json POST with charset", "127.0.0.1", "POST", "/api/accounts/x/messages/send", "localhost:8765", "", "application/json; charset=utf-8", 200},
		{"same-origin browser POST", "127.0.0.1", "POST", "/api/rules", "localhost:8765", "http://localhost:8765", "application/json", 200},

		{"DNS rebinding host", "127.0.0.1", "GET", "/api/accounts", "evil.example:8765", "", "", 403},
		{"DNS rebinding MCP", "127.0.0.1", "POST", "/mcp", "evil.example:8765", "http://evil.example:8765", "application/json", 403},
		{"cross-origin POST", "127.0.0.1", "POST", "/api/accounts/x/messages/send", "localhost:8765", "https://evil.example", "application/json", 403},
		{"null origin", "127.0.0.1", "POST", "/api/rules", "localhost:8765", "null", "application/json", 403},
		{"non-http origin", "127.0.0.1", "GET", "/api/health", "localhost:8765", "file://localhost", "", 403},
		{"text/plain CSRF POST", "127.0.0.1", "POST", "/api/accounts/x/messages/send", "localhost:8765", "", "text/plain", 415},
		{"form CSRF POST", "127.0.0.1", "POST", "/api/accounts/x/messages/send", "localhost:8765", "", "application/x-www-form-urlencoded", 415},
		{"missing content-type DELETE", "127.0.0.1", "DELETE", "/api/rules/1", "localhost:8765", "", "", 415},
		{"empty host", "127.0.0.1", "GET", "/api/health", "", "", "", 403},

		{"explicit bind host allowed", "10.0.0.5", "GET", "/api/health", "10.0.0.5:8765", "", "", 200},
		{"wildcard bind adds nothing", "0.0.0.0", "GET", "/api/health", "10.0.0.5:8765", "", "", 403},
		{"MCP path skips content-type rule", "127.0.0.1", "POST", "/mcp", "localhost:8765", "", "text/plain", 200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://placeholder"+tc.path, strings.NewReader("{}"))
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.ctype != "" {
				req.Header.Set("Content-Type", tc.ctype)
			}
			rec := httptest.NewRecorder()
			browserGuard(tc.bind, ok).ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("got %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}
