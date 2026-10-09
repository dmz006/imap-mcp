// Package imaptest runs an in-memory IMAP server (go-imap's imapmemserver)
// for tests, and builds an imap-mcp config/pool connected to it.
package imaptest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/imap"
)

// Username and Password of the test account.
const (
	Username = "user@example.com"
	Password = "pw"
)

// Server is a running in-memory IMAP server with one user.
type Server struct {
	Addr string
	User *imapmemserver.User
	Host string
	Port int
	srv  *imapserver.Server
}

// Close stops the server, e.g. to simulate an account going away.
func (s *Server) Close() { s.srv.Close() }

// Start runs a server with the given folders (INBOX is always created) and
// capabilities in addition to IMAP4rev1.
func Start(t testing.TB, folders []string, caps ...imaplib.Cap) *Server {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(Username, Password)
	for _, f := range append([]string{"INBOX"}, folders...) {
		if err := user.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	capSet := imaplib.CapSet{imaplib.CapIMAP4rev1: {}}
	for _, c := range caps {
		capSet[c] = struct{}{}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         capSet,
		InsecureAuth: true,
		Logger:       nopLogger{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return &Server{Addr: ln.Addr().String(), User: user, Host: host, Port: p, srv: srv}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return int64(l.Len()) }

// Append adds a raw message to folder.
func (s *Server) Append(t testing.TB, folder, raw string, when time.Time, flags ...imaplib.Flag) {
	t.Helper()
	if _, err := s.User.Append(folder, literal{bytes.NewReader([]byte(raw))}, &imaplib.AppendOptions{Time: when, Flags: flags}); err != nil {
		t.Fatal(err)
	}
}

// Plain returns a minimal text/plain message.
func Plain(messageID, from, subject, body string) string {
	return "From: " + from + "\r\nTo: " + Username + "\r\nSubject: " + subject +
		"\r\nMessage-ID: <" + messageID + ">\r\nContent-Type: text/plain\r\n\r\n" + body + "\r\n"
}

// Account returns an account config pointing at the server.
func (s *Server) Account(name string) config.AccountConfig {
	return config.AccountConfig{
		Name: name, Default: true,
		IMAP: config.IMAPConfig{Host: s.Host, Port: s.Port},
		Auth: config.AuthConfig{Type: "plain", Username: Username, Password: Password},
	}
}

// Pool connects a pool for cfg (whose accounts point at test servers).
func Pool(t testing.TB, cfg *config.Config, b *bus.Bus) *imap.Pool {
	t.Helper()
	if b == nil {
		b = bus.New()
	}
	pool := imap.NewPool(cfg, b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := pool.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
