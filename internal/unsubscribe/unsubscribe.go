// Package unsubscribe implements safe unsubscribing (AGENT.md Q4, D39–D41).
//
// Only two methods exist. RFC 8058 one-click: the message carries an https
// List-Unsubscribe URI and List-Unsubscribe-Post: List-Unsubscribe=One-Click,
// both covered by a DKIM signature that verifies here (D41); the unsubscribe
// is one POST with a fixed body, no cookies, no credentials, no redirects to
// another host, and never to a private address. mailto: the address (and any
// subject/body the sender asked for) from the same header; sending it is the
// caller's job (D40). Links in the body are never followed or fetched.
package unsubscribe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// Methods.
const (
	MethodOneClick = "one-click"
	MethodMailto   = "mailto"
	MethodNone     = "none"
)

// Plan is what an unsubscribe would do for one message, or why it can't.
type Plan struct {
	Method string `json:"method"`           // one-click | mailto | none
	Reason string `json:"reason,omitempty"` // why one-click is not possible
	// URL is the one-click target. Unsubscribe URLs usually carry a token
	// that identifies the recipient, so it is shown to the caller but never
	// logged or stored (only Host is).
	URL  string `json:"url,omitempty"`
	Host string `json:"host,omitempty"`
	// Mailto is the message a mailto unsubscribe sends.
	Mailto *MailtoMessage `json:"mailto,omitempty"`
	// SignedBy is the DKIM domain whose signature covers both headers.
	SignedBy string `json:"signed_by,omitempty"`
	// SignatureExpired: that signature is past its x= expiry but otherwise
	// verifies, which is accepted (D58).
	SignatureExpired bool `json:"signature_expired,omitempty"`
}

// MailtoMessage is the unsubscribe email a mailto: URI asks for.
type MailtoMessage struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// Options control analysis; zero values are production defaults.
type Options struct {
	// LookupTXT replaces DNS for DKIM key lookups (tests).
	LookupTXT func(domain string) ([]string, error)
}

// Analyze reads a raw message and decides the method. A one-click plan
// needs the https URI, the One-Click post header and a verified DKIM
// signature covering both. Otherwise a mailto URI gives a mailto plan, with
// Reason saying why one-click is not possible; with neither, Method is none.
func Analyze(raw []byte, opt Options) Plan {
	hdr, err := readHeader(raw)
	if err != nil {
		return Plan{Method: MethodNone, Reason: "the message headers could not be read"}
	}
	https, mailto := parseListUnsubscribe(hdr.Get("List-Unsubscribe"))
	if https == "" && mailto == "" {
		return Plan{Method: MethodNone, Reason: "no List-Unsubscribe header"}
	}
	reason := ""
	switch {
	case https == "":
		reason = "mailto only: no https List-Unsubscribe URI"
	case !strings.EqualFold(strings.TrimSpace(hdr.Get("List-Unsubscribe-Post")), "List-Unsubscribe=One-Click"):
		reason = "no List-Unsubscribe-Post: List-Unsubscribe=One-Click header (RFC 8058)"
	default:
		u, perr := url.Parse(https)
		if perr != nil || u.Hostname() == "" {
			reason = "the https List-Unsubscribe URI is malformed"
			break
		}
		domain, expired, derr := signedBoth(raw, opt)
		if derr != nil {
			reason = derr.Error()
			break
		}
		return Plan{Method: MethodOneClick, URL: https, Host: u.Hostname(), SignedBy: domain, SignatureExpired: expired}
	}
	if mailto != "" {
		if m, ok := parseMailto(mailto); ok {
			return Plan{Method: MethodMailto, Reason: reason, Mailto: m}
		}
	}
	return Plan{Method: MethodNone, Reason: reason}
}

func readHeader(raw []byte) (textproto.MIMEHeader, error) {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	h, err := r.ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return nil, err
	}
	return h, nil
}

// parseListUnsubscribe returns the first https and mailto URIs of a
// List-Unsubscribe header (RFC 2369: a comma-separated list of <URI>).
func parseListUnsubscribe(v string) (https, mailto string) {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "<") || !strings.HasSuffix(part, ">") {
			continue
		}
		u := strings.TrimSpace(part[1 : len(part)-1])
		lu := strings.ToLower(u)
		switch {
		case strings.HasPrefix(lu, "https://") && https == "":
			https = u
		case strings.HasPrefix(lu, "mailto:") && mailto == "":
			mailto = u
		}
	}
	return https, mailto
}

// parseMailto turns a mailto: URI into the message to send (RFC 6068). A
// missing subject or body becomes "unsubscribe".
func parseMailto(s string) (*MailtoMessage, bool) {
	u, err := url.Parse(s)
	if err != nil || !strings.EqualFold(u.Scheme, "mailto") {
		return nil, false
	}
	to := u.Opaque
	if to == "" {
		to = u.Path
	}
	if dec, err := url.PathUnescape(to); err == nil {
		to = dec
	}
	q := u.Query()
	if to == "" {
		to = q.Get("to")
	}
	if strings.Contains(to, ",") {
		to = strings.TrimSpace(strings.Split(to, ",")[0])
	}
	a, err := mail.ParseAddress(to)
	if err != nil || !strings.Contains(a.Address[strings.LastIndexByte(a.Address, '@')+1:], ".") {
		return nil, false
	}
	m := &MailtoMessage{To: a.Address, Subject: q.Get("subject"), Body: q.Get("body")}
	if strings.TrimSpace(m.Subject) == "" {
		m.Subject = "unsubscribe"
	}
	if strings.TrimSpace(m.Body) == "" {
		m.Body = "unsubscribe"
	}
	return m, true
}

// signedBoth returns the domain of a DKIM signature that verifies and signs
// both List-Unsubscribe and List-Unsubscribe-Post (D41), and whether that
// signature is past its expiry (accepted, D58).
func signedBoth(raw []byte, opt Options) (string, bool, error) {
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: opt.LookupTXT, MaxVerifications: 5})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		return "", false, fmt.Errorf("DKIM could not be checked")
	}
	if len(vs) == 0 {
		return "", false, fmt.Errorf("no DKIM signature")
	}
	verified := false
	for _, v := range vs {
		if v.Err != nil {
			continue
		}
		verified = true
		var lu, lup bool
		for _, k := range v.HeaderKeys {
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "list-unsubscribe":
				lu = true
			case "list-unsubscribe-post":
				lup = true
			}
		}
		if lu && lup {
			return v.Domain, !v.Expiration.IsZero() && time.Now().After(v.Expiration), nil
		}
	}
	if !verified {
		return "", false, fmt.Errorf("DKIM signature does not verify")
	}
	return "", false, fmt.Errorf("DKIM signature does not cover List-Unsubscribe and List-Unsubscribe-Post")
}

// Result is the outcome of a one-click POST.
type Result struct {
	Status int    `json:"status"`          // HTTP status code; 0 when no response
	OK     bool   `json:"ok"`              // a 2xx response
	Error  string `json:"error,omitempty"` // why it failed, without the URL
}

// Client posts one-click unsubscribes.
type Client struct {
	http      *http.Client
	userAgent string
}

// NewClient returns a client with the outbound guard: https only, public
// addresses only, no cookies, redirects only within the same host, and a
// short timeout. allowPrivate is for tests against a local server.
func NewClient(version string, timeout time.Duration, allowPrivate bool) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !publicIP(ip) {
				return fmt.Errorf("refusing to connect to a non-public address")
			}
			return nil
		}
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          1,
		DisableKeepAlives:     true,
	}
	c := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return fmt.Errorf("refusing a redirect to another host")
			}
			return nil
		},
	}
	return &Client{http: c, userAgent: "imap-mcp/" + version}
}

// TrustTLS replaces the client's TLS settings, for tests against a local
// server with its own certificate.
func (c *Client) TrustTLS(cfg *tls.Config) { c.http.Transport.(*http.Transport).TLSClientConfig = cfg }

// publicIP reports whether ip is a routable public address.
func publicIP(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsInterfaceLocalMulticast() && !ip.IsMulticast() &&
		!cgnat.Contains(ip) && !ip.Equal(net.IPv4bcast)
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// OneClick sends the RFC 8058 POST to target.
func (c *Client) OneClick(ctx context.Context, target string) Result {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return Result{Error: "not an https URI"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("List-Unsubscribe=One-Click"))
	if err != nil {
		return Result{Error: "bad request"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{Error: scrub(err, target)}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return Result{Status: resp.StatusCode, OK: resp.StatusCode >= 200 && resp.StatusCode < 300}
}

// scrub removes the URL (which may identify the recipient) from an error.
func scrub(err error, target string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return strings.ReplaceAll(err.Error(), target, "<url>")
}
