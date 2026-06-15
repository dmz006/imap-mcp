// Package imap manages a pool of persistent IMAP connections, one per
// configured account. Connections are established at startup and
// automatically reconnected on failure.
package imap

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/imap/auth"
	imaplib "github.com/emersion/go-imap/v2/imapclient"
)

// Pool holds authenticated IMAP connections for all configured accounts.
type Pool struct {
	mu    sync.RWMutex
	conns map[string]*Conn
	cfg   *config.Config
	bus   *bus.Bus
	log   *slog.Logger
}

// Conn wraps a single account's IMAP connection with reconnect logic.
type Conn struct {
	account string
	cfg     config.AccountConfig
	auth    auth.Authenticator
	client  *imaplib.Client
	mu      sync.Mutex
	log     *slog.Logger
}

func NewPool(cfg *config.Config, b *bus.Bus, log *slog.Logger) *Pool {
	return &Pool{
		conns: make(map[string]*Conn),
		cfg:   cfg,
		bus:   b,
		log:   log,
	}
}

// Connect establishes connections to all configured accounts.
func (p *Pool) Connect(ctx context.Context) error {
	for _, a := range p.cfg.Accounts {
		conn, err := p.connect(ctx, a)
		if err != nil {
			p.log.Error("connect account", "account", a.Name, "err", err)
			p.bus.PublishAsync(bus.Event{
				Type:    bus.EventAccountError,
				Account: a.Name,
				Payload: err.Error(),
			})
			continue
		}
		p.mu.Lock()
		p.conns[a.Name] = conn
		p.mu.Unlock()
		p.bus.PublishAsync(bus.Event{
			Type:    bus.EventAccountConnected,
			Account: a.Name,
		})
		p.log.Info("account connected", "account", a.Name, "host", a.IMAP.Host)
	}
	return nil
}

func (p *Pool) connect(ctx context.Context, a config.AccountConfig) (*Conn, error) {
	authenticator, err := auth.New(auth.Options{
		Type:               a.Auth.Type,
		Username:           a.Auth.Username,
		Password:           a.Auth.Password,
		ClientID:           a.Auth.ClientID,
		ClientSecret:       a.Auth.ClientSecret,
		TokenFile:          a.Auth.TokenFile,
		Provider:           a.Auth.Provider,
		ServiceAccountFile: a.Auth.ServiceAccountFile,
		Subject:            a.Auth.Subject,
	})
	if err != nil {
		return nil, fmt.Errorf("build auth: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", a.IMAP.Host, imapPort(a.IMAP))
	opts := &imaplib.Options{}

	var client *imaplib.Client
	if a.IMAP.TLS {
		client, err = imaplib.DialTLS(addr, opts)
	} else {
		client, err = imaplib.DialInsecure(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	if err := authenticator.Authenticate(ctx, client); err != nil {
		client.Close()
		return nil, err
	}

	return &Conn{
		account: a.Name,
		cfg:     a,
		auth:    authenticator,
		client:  client,
		log:     p.log.With("account", a.Name),
	}, nil
}

// Get returns the connection for an account name. Returns error if not found or disconnected.
func (p *Pool) Get(account string) (*Conn, error) {
	p.mu.RLock()
	conn, ok := p.conns[account]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("account %q not connected", account)
	}
	return conn, nil
}

// Default returns the connection for the default account.
func (p *Pool) Default() (*Conn, error) {
	for _, a := range p.cfg.Accounts {
		if a.Default {
			return p.Get(a.Name)
		}
	}
	return p.Get(p.cfg.Accounts[0].Name)
}

// Resolve returns the connection for the given account name, or the default if empty.
func (p *Pool) Resolve(account string) (*Conn, error) {
	if account == "" {
		return p.Default()
	}
	return p.Get(account)
}

// AccountNames returns the names of all connected accounts.
func (p *Pool) AccountNames() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	names := make([]string, 0, len(p.conns))
	for n := range p.conns {
		names = append(names, n)
	}
	return names
}

// Reconnect attempts to re-establish a dropped connection with backoff.
func (p *Pool) Reconnect(ctx context.Context, accountName string) {
	a, err := p.cfg.Account(accountName)
	if err != nil {
		return
	}
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		p.log.Info("reconnecting", "account", accountName)
		conn, err := p.connect(ctx, *a)
		if err != nil {
			p.log.Warn("reconnect failed", "account", accountName, "err", err, "retry_in", backoff)
			if backoff < 5*time.Minute {
				backoff *= 2
			}
			continue
		}
		p.mu.Lock()
		p.conns[accountName] = conn
		p.mu.Unlock()
		p.bus.PublishAsync(bus.Event{
			Type:    bus.EventAccountConnected,
			Account: accountName,
		})
		p.log.Info("reconnected", "account", accountName)
		return
	}
}

// StartKeepalive issues a NOOP on every connection at the given interval and
// reconnects any that have dropped. IMAP servers close idle connections, so
// this keeps them warm and self-heals instead of failing on next use. Safe to
// run for the life of the process; blocks until ctx is cancelled.
func (p *Pool) StartKeepalive(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 4 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, name := range p.AccountNames() {
				conn, err := p.Get(name)
				if err != nil {
					continue
				}
				if err := conn.ping(); err != nil {
					p.log.Warn("keepalive: dead connection, reconnecting", "account", name, "err", err)
					p.reconnectOnce(ctx, name)
				}
			}
		}
	}
}

// ping issues a NOOP to check the connection is alive.
func (c *Conn) ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil {
		return fmt.Errorf("no client")
	}
	return c.client.Noop().Wait()
}

// Probe reports whether the account's connection is actually alive (live NOOP),
// not just present in the pool. Used by health checks so they don't lie.
func (p *Pool) Probe(account string) bool {
	conn, err := p.Get(account)
	if err != nil {
		return false
	}
	return conn.ping() == nil
}

// reconnectOnce makes a single reconnect attempt and swaps in the new client.
func (p *Pool) reconnectOnce(ctx context.Context, name string) {
	a, err := p.cfg.Account(name)
	if err != nil {
		return
	}
	conn, err := p.connect(ctx, *a)
	if err != nil {
		p.log.Warn("keepalive reconnect failed", "account", name, "err", err)
		p.bus.PublishAsync(bus.Event{Type: bus.EventAccountError, Account: name, Payload: err.Error()})
		return
	}
	p.mu.Lock()
	if old := p.conns[name]; old != nil {
		old.mu.Lock()
		old.client.Close() //nolint:errcheck
		old.mu.Unlock()
	}
	p.conns[name] = conn
	p.mu.Unlock()
	p.bus.PublishAsync(bus.Event{Type: bus.EventAccountConnected, Account: name})
	p.log.Info("keepalive reconnected", "account", name)
}

// Close disconnects all accounts gracefully.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, conn := range p.conns {
		conn.mu.Lock()
		conn.client.Logout().Wait() //nolint:errcheck
		conn.mu.Unlock()
		p.log.Info("account disconnected", "account", name)
	}
}

// Client returns the raw IMAP client. Callers must hold conn.mu when using it.
func (c *Conn) Client() *imaplib.Client {
	return c.client
}

// Account returns the account name.
func (c *Conn) Account() string { return c.account }

// Lock acquires the connection mutex for exclusive use.
func (c *Conn) Lock() { c.mu.Lock() }

// Unlock releases the connection mutex.
func (c *Conn) Unlock() { c.mu.Unlock() }

func imapPort(cfg config.IMAPConfig) int {
	if cfg.Port != 0 {
		return cfg.Port
	}
	if cfg.TLS {
		return 993
	}
	if cfg.StartTLS {
		return 143
	}
	return 143
}
