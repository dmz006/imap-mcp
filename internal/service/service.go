// Package service is the single implementation of imap-mcp's operations,
// shared by the MCP tools and the REST API (AGENT.md D13): transports parse
// their input, call the service, and render its typed results and errors.
// No operation is implemented twice.
package service

import (
	"errors"
	"fmt"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/sync"
)

// Kind classifies a service error so each transport can map it (REST status
// code, MCP tool error).
type Kind int

const (
	KindInternal      Kind = iota // 500
	KindInvalid                   // 400: bad or missing input
	KindNotFound                  // 404: unknown account, message, rule
	KindUnprocessable             // 422: valid input the config can't serve (e.g. receive-only account)
	KindUnavailable               // 503: subsystem not running in this mode
	KindUpstream                  // 502: IMAP/SMTP server failure
)

// Error is a classified service error.
type Error struct {
	Kind Kind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the Kind of err (KindInternal for unclassified errors).
func KindOf(err error) Kind {
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	return KindInternal
}

func invalid(format string, a ...any) error {
	return &Error{Kind: KindInvalid, Msg: fmt.Sprintf(format, a...)}
}

func notFound(format string, a ...any) error {
	return &Error{Kind: KindNotFound, Msg: fmt.Sprintf(format, a...)}
}

func upstream(msg string, err error) error { return &Error{Kind: KindUpstream, Msg: msg, Err: err} }

func unavailable(msg string) error { return &Error{Kind: KindUnavailable, Msg: msg} }

// Service implements every operation. Optional subsystems (syncer, pipeline)
// may be nil, e.g. in the run-rules CLI.
type Service struct {
	cfg      *config.Config
	pool     *imap.Pool
	db       *db.DB
	syncer   *sync.Syncer
	pipeline *enrichment.Pipeline
}

// New builds the service.
func New(cfg *config.Config, pool *imap.Pool, database *db.DB, syncer *sync.Syncer, pipeline *enrichment.Pipeline) *Service {
	return &Service{cfg: cfg, pool: pool, db: database, syncer: syncer, pipeline: pipeline}
}

// conn resolves an account ("" or "_default" = the default account).
func (s *Service) conn(account string) (*imap.Conn, error) {
	if account == "_default" {
		account = ""
	}
	c, err := s.pool.Resolve(account)
	if err != nil {
		return nil, notFound("account %q: %v", account, err)
	}
	return c, nil
}

// SetPipeline attaches the enrichment pipeline after construction.
func (s *Service) SetPipeline(p *enrichment.Pipeline) { s.pipeline = p }
