package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
)

type httpHandlerFunc func(ctx context.Context)

func (f httpHandlerFunc) ServeHTTP(_ http.ResponseWriter, r *http.Request) { f(r.Context()) }

func serveWithToken(h http.Handler, token string) {
	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)
}
