package smtpsrv

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/attach"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ratelimit"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// RulesFunc returns the ruleset currently in force.
//
// A session calls it once, at connect, and keeps that pointer for its whole
// life: a reload must never change the rules midway through a transaction, or
// a message could be accepted under one policy and routed under another.
type RulesFunc func() *ruleset.Ruleset

// AuthFunc returns the inbound SMTP AUTH credentials currently in force.
type AuthFunc func() *config.Auth

// Spooler stores message bodies and accepts routed envelopes for delivery.
type Spooler interface {
	WriteBody(txnUUID string, r io.Reader) (name string, size int64, err error)
	Enqueue(*queue.Envelope) error
	Quarantine(*queue.Envelope) error
	// RemoveBody drops a body that ended up with no envelope referencing it,
	// which happens when a data-stage rule refuses the message after it has
	// been spooled for scanning.
	RemoveBody(name string) error
	// ReadBody reopens a spooled body, for the MIME walk. bodyScan collects its
	// facts as the message streams past, but it deliberately does not buffer the
	// body — so the only way to walk the MIME structure is to read back what was
	// just written, and only a configuration that asked for it pays that cost.
	ReadBody(name string) (io.ReadCloser, error)
}

// Notifier is the subset of the events client the session needs.
type Notifier interface {
	Send(events.Envelope)
}

// AttachScanner checks a message's attachments against the logservice MD5
// blocklist.
//
// Nil means scanning is off, which is both the shipped default and what every
// test in this package gets — so "is attach.enabled set?" is answered by a nil
// check rather than by re-reading configuration on the mail path.
type AttachScanner interface {
	Check(ctx context.Context, txnUUID string, parts []attach.Part) (attach.Verdict, error)
}

// Backend builds a Session per connection.
type Backend struct {
	Cfg *config.Config

	// Ctx is the process's serve context, cancelled when shutdown begins.
	//
	// It exists because a smtp.Session has no context of its own — Data is handed
	// only an io.Reader — and the attachment scan is an HTTP call made inside the
	// DATA reply. Without it that call ran on context.Background(), so a hung
	// logservice pinned the session and its goroutine straight through
	// server.shutdown_timeout, which is the same defect M8 had to fix in
	// events.Client. Nil is tolerated and means "no cancellation"; production
	// always sets it.
	Ctx context.Context
	// Allowlist returns the current allowlist. The connect-stage check happens
	// in the listener wrapper; this is here so a session can report what was in
	// force when it started.
	Allowlist AllowlistFunc
	Rules     RulesFunc
	// Auth returns the current inbound credential set, or nil when none is
	// configured — in which case AUTH is never advertised.
	//
	// A func for the same reason Rules and Allowlist are: credentials hot-swap
	// when Central Management deploys a new bundle, and reading them through
	// Backend.Cfg would pin them to whatever was in force at bring-up. Unlike
	// Rules it is read per AUTH command rather than snapshotted per session:
	// there is no transaction to keep consistent, and a credential an operator
	// has just revoked should stop working on the next attempt.
	Auth   AuthFunc
	Spool  Spooler
	Events Notifier
	// Attach is nil unless attach.enabled. Built once at bring-up so its HTTP
	// client's connections are reused across messages.
	Attach AttachScanner
	// MsgAuth is nil unless something wants SPF, DKIM or DMARC — either a
	// msgauth.* configuration key or a rule reading spf.*, dkim.* or dmarc.*.
	// Built once at bring-up, on the Attach precedent, so the mail path answers
	// with a nil check rather than by re-reading configuration per message.
	MsgAuth *MsgAuth
	// Limiter returns the rate limiter in force. Nil, or a func returning nil,
	// means nothing is limited — Limiter.Allow is nil-safe, so the session does
	// not branch on it.
	//
	// A func for the reason Rules, Allowlist and Auth are: rate limits are read
	// live so they can be retuned during an incident without a restart. Unlike
	// Rules it is NOT snapshotted per session — a limit an operator has just
	// lowered should bite the connection that is already open, which is the one
	// they lowered it for.
	Limiter LimiterFunc
	Log     *slog.Logger
	// Metrics may be nil; see obs(). Production always sets it.
	Metrics *obs.Metrics
	// Gateway labels the audit rows a session writes, so the log tables can say
	// which box handled a message. Resolved once at bring-up.
	Gateway string

	// OnQueued is invoked after a message is spooled, with its envelopes. M1
	// uses it to kick off an immediate delivery attempt; from M3 the scheduler
	// owns that and this becomes a nudge.
	OnQueued func([]*queue.Envelope)

	// Bounce generates a delivery status notification, for recipients refused
	// after the message was already accepted — where there is no SMTP reply left
	// to refuse them in.
	//
	// A callback rather than a widened Spooler: building a notification needs the
	// postmaster address, the return policy and a relay group for the bounce,
	// none of which this package has any other reason to know. Nil disables it,
	// which is what every test in this package gets.
	Bounce func(queue.Request) (bool, error)

	// connSeq only exists to make log lines easier to correlate.
	connSeq atomic.Uint64
}

// obs returns the counter registry, or a shared discard when none was supplied.
//
// A Backend built by literal — which is how every test in this package builds
// one — would otherwise nil-dereference inside a live session.
// ctx returns the serve context, or a background context when none was supplied
// — the same nil-tolerance obs() has, and for the same reason: every test in
// this package builds a Backend by literal.
func (b *Backend) ctx() context.Context {
	if b.Ctx != nil {
		return b.Ctx
	}
	return context.Background()
}

func (b *Backend) obs() *obs.Metrics {
	if b.Metrics != nil {
		return b.Metrics
	}
	return obs.Discard
}

// limiter returns the rate limiter, or nil when none was supplied — which every
// test in this package that builds a Backend by literal gets, and which
// Limiter.Allow treats as "not limited".
func (b *Backend) limiter() *ratelimit.Limiter {
	if b.Limiter == nil {
		return nil
	}
	return b.Limiter()
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
		// Reported here because go-smtp never takes ownership of a session we
		// refuse: it discards the error's session and never calls Logout, so
		// this is the only place a connection refused at the greeting can be
		// recorded. Without it, the one thing a connect- or helo-stage reject
		// rule exists to catch would leave nothing in the log tables.
		s.postConnection()
		return nil, err
	}
	return s, nil
}

// StaticRules returns a RulesFunc serving one fixed ruleset, for tests and for
// callers that do not support reload.
func StaticRules(rs *ruleset.Ruleset) RulesFunc { return func() *ruleset.Ruleset { return rs } }
