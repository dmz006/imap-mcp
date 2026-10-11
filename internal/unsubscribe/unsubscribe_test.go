package unsubscribe

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

const body = "\r\nHello, this is the newsletter.\r\n"

func header(lu, lup string) string {
	h := "From: News <news@list.example.com>\r\nTo: jane@example.com\r\nSubject: Weekly\r\nDate: Sat, 10 Oct 2026 10:00:00 +0000\r\nMessage-ID: <1@list.example.com>\r\n"
	if lu != "" {
		h += "List-Unsubscribe: " + lu + "\r\n"
	}
	if lup != "" {
		h += "List-Unsubscribe-Post: " + lup + "\r\n"
	}
	return h
}

// signed signs msg for list.example.com over headers and returns the
// message and a LookupTXT that serves the key.
func signed(t *testing.T, msg string, headers []string) ([]byte, func(string) ([]string, error)) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{
		Domain: "list.example.com", Selector: "s1", Signer: priv, HeaderKeys: headers,
	}); err != nil {
		t.Fatal(err)
	}
	lookup := func(domain string) ([]string, error) {
		if domain != "s1._domainkey.list.example.com" {
			return nil, &net.DNSError{Err: "no such host", Name: domain, IsNotFound: true}
		}
		return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
	}
	return out.Bytes(), lookup
}

var both = []string{"From", "To", "Subject", "Date", "List-Unsubscribe", "List-Unsubscribe-Post"}

func TestAnalyzeOneClick(t *testing.T) {
	msg := header("<mailto:unsub@list.example.com?subject=stop>, <https://list.example.com/u/abc123>", "List-Unsubscribe=One-Click") + body
	raw, lookup := signed(t, msg, both)
	p := Analyze(raw, Options{LookupTXT: lookup})
	if p.Method != MethodOneClick || p.Host != "list.example.com" || p.SignedBy != "list.example.com" || p.URL != "https://list.example.com/u/abc123" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestAnalyzeFallsBackToMailto(t *testing.T) {
	cases := []struct {
		name, lu, lup string
		headers       []string
		tamper        bool
		reason        string
	}{
		{"mailto only", "<mailto:unsub@list.example.com?subject=stop>", "", both, false, "mailto only"},
		{"no post header", "<https://list.example.com/u/1>, <mailto:unsub@list.example.com>", "", both, false, "List-Unsubscribe-Post"},
		{"not covered", "<https://list.example.com/u/1>, <mailto:unsub@list.example.com>", "List-Unsubscribe=One-Click",
			[]string{"From", "To", "Subject"}, false, "does not cover"},
		{"tampered", "<https://list.example.com/u/1>, <mailto:unsub@list.example.com>", "List-Unsubscribe=One-Click", both, true, "does not verify"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, lookup := signed(t, header(c.lu, c.lup)+body, c.headers)
			if c.tamper {
				raw = bytes.Replace(raw, []byte("/u/1>"), []byte("/u/2>"), 1)
			}
			p := Analyze(raw, Options{LookupTXT: lookup})
			if p.Method != MethodMailto || !strings.Contains(p.Reason, c.reason) || p.Mailto == nil || p.Mailto.To != "unsub@list.example.com" {
				t.Fatalf("plan = %+v", p)
			}
		})
	}
}

func TestAnalyzeUnsignedAndNone(t *testing.T) {
	p := Analyze([]byte(header("<https://list.example.com/u/1>", "List-Unsubscribe=One-Click")+body), Options{})
	if p.Method != MethodNone || p.Reason != "no DKIM signature" {
		t.Fatalf("unsigned plan = %+v", p)
	}
	p = Analyze([]byte(header("", "")+body), Options{})
	if p.Method != MethodNone || p.Reason != "no List-Unsubscribe header" {
		t.Fatalf("no-header plan = %+v", p)
	}
	// An http (not https) URI is never used.
	p = Analyze([]byte(header("<http://list.example.com/u/1>", "List-Unsubscribe=One-Click")+body), Options{})
	if p.Method != MethodNone {
		t.Fatalf("http plan = %+v", p)
	}
}

func TestParseMailto(t *testing.T) {
	m, ok := parseMailto("mailto:unsub%2Bx@list.example.com?subject=Remove%20me&body=please")
	if !ok || m.To != "unsub+x@list.example.com" || m.Subject != "Remove me" || m.Body != "please" {
		t.Fatalf("got %+v %v", m, ok)
	}
	m, ok = parseMailto("mailto:unsub@list.example.com")
	if !ok || m.Subject != "unsubscribe" || m.Body != "unsubscribe" {
		t.Fatalf("defaults: %+v", m)
	}
	if _, ok := parseMailto("mailto:nobody"); ok {
		t.Fatal("a bare word must be refused")
	}
}

func TestPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.1.1", "100.64.0.1", "::1", "fd00::1", "fe80::1", "0.0.0.0"} {
		if publicIP(net.ParseIP(s)) {
			t.Errorf("%s counted as public", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "2606:2800:220:1::1"} {
		if !publicIP(net.ParseIP(s)) {
			t.Errorf("%s counted as private", s)
		}
	}
}

func TestOneClickRefusesPrivateAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := NewClient("test", 2*time.Second, false)
	res := c.OneClick(context.Background(), srv.URL+"/u/secret-token")
	if res.OK || !strings.Contains(res.Error, "non-public") || strings.Contains(res.Error, "secret-token") {
		t.Fatalf("result = %+v", res)
	}
}

func TestOneClickPost(t *testing.T) {
	var gotBody, gotType, gotCookie string
	var gotMethod string
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed a redirect to another host")
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/away" {
			http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody, gotType, gotMethod, gotCookie = string(b), r.Header.Get("Content-Type"), r.Method, r.Header.Get("Cookie")
		http.SetCookie(w, &http.Cookie{Name: "track", Value: "1"})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := NewClient("test", 2*time.Second, true)
	c.http.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	res := c.OneClick(context.Background(), srv.URL+"/u/1")
	if !res.OK || res.Status != 200 || gotMethod != http.MethodPost || gotBody != "List-Unsubscribe=One-Click" ||
		gotType != "application/x-www-form-urlencoded" || gotCookie != "" {
		t.Fatalf("res=%+v method=%s body=%q type=%q cookie=%q", res, gotMethod, gotBody, gotType, gotCookie)
	}
	res = c.OneClick(context.Background(), srv.URL+"/away")
	if res.OK || !strings.Contains(res.Error, "another host") {
		t.Fatalf("redirect result = %+v", res)
	}
	if res := c.OneClick(context.Background(), "http://list.example.com/u/1"); res.OK || res.Error == "" {
		t.Fatalf("http result = %+v", res)
	}
}
