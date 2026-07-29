package smtpsrv

import (
	"io"
	"log/slog"
	"sync/atomic"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// RulesFunc returns the ruleset currently in force.
//
// A session calls it once, at connect, and keeps that pointer for its whole
// life: a reload must never change the rules midway through a transaction, or
// a message could be accepted under one policy and routed under another.
type RulesFunc func() *ruleset.Ruleset

// Spooler stores message bodies and accepts routed envelopes for delivery.
type Spooler interface {
	WriteBody(txnUUID string, r io.Reader) (name string, size int64, err error)
	Enqueue(*queue.Envelope) error
	Quarantine(*queue.Envelope) error
	// RemoveBody drops a body that ended up with no envelope referencing it,
	// which happens when a data-stage rule refuses the message after it has
	// been spooled for scanning.
	RemoveBody(name string) error
}

// Notifier is the subset of the events client the session needs.
type Notifier interface {
	Send(events.Envelope)
}

// Backend builds a Session per connection.
type Backend struct {
	Cfg *config.Config
	// Allowlist returns the current allowlist. The connect-stage check happens
	// in the listener wrapper; this is here so a session can report what was in
	// force when it started.
	Allowlist AllowlistFunc
	Rules     RulesFunc
	Spool     Spooler
	Events    Notifier
	Log       *slog.Logger

	// OnQueued is invoked after a message is spooled, with its envelopes. M1
	// uses it to kick off an immediate delivery attempt; from M3 the scheduler
	// owns that and this becomes a nudge.
	OnQueued func([]*queue.Envelope)

	// connSeq only exists to make log lines easier to correlate.
	connSeq atomic.Uint64
}

var _ smtp.Backend = (*Backend)(nil)

// NewSession is called by go-smtp at EHLO/HELO, not at TCP accept — which is
// why the allowlist check lives in the listener rather than here.
//
// Returning an *smtp.SMTPError here answers EHLO with that exact code
// (conn.go:238-243 hands it to writeError, which passes an SMTPError through
// verbatim), so connect- and helo-stage policy can reject before MAIL FROM.
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	log := b.Log
	if log == nil {
		log = slog.Default()
	}

	s := newSession(b, c, log)
	b.connSeq.Add(1)

	if err := s.greetPolicy(); err != nil {
		return nil, err
	}
	return s, nil
}

// StaticRules returns a RulesFunc serving one fixed ruleset, for tests and for
// callers that do not support reload.
func StaticRules(rs *ruleset.Ruleset) RulesFunc { return func() *ruleset.Ruleset { return rs } }
