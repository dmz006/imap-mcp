package server

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// browserGuard blocks requests a web browser can be tricked into making against
// the local server:
//
//   - DNS rebinding: a hostile domain resolved to 127.0.0.1 makes the browser
//     treat requests as same-origin. The Host header still carries the hostile
//     name, so only loopback names or the configured bind host are accepted.
//   - Cross-site requests: an Origin header, when present, must also name an
//     allowed host ("null" origins from sandboxed frames/file:// are refused).
//   - CSRF via "simple" requests: browsers send text/plain POSTs cross-origin
//     without a CORS preflight. Unsafe /api methods must declare
//     application/json, which forces a preflight the server never grants.
//
// Non-browser clients (curl, datawatch, Claude Code) send loopback Host headers,
// no Origin, and application/json, so they are unaffected.
func browserGuard(bindHost string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host, bindHost) {
			http.Error(w, "forbidden: host not allowed", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if origin == "null" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || !allowedHost(u.Host, bindHost) {
				http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !safeMethod(r.Method) {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				http.Error(w, "unsupported media type: Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// allowedHost reports whether hostport (a Host header or Origin authority)
// names a loopback host or the configured bind host. Wildcard binds (0.0.0.0,
// ::) add nothing beyond loopback.
func allowedHost(hostport, bindHost string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	bind := strings.ToLower(strings.Trim(bindHost, "[]"))
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		return false
	}
	return host == bind
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}
