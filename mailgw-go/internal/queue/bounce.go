package queue

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/dsn"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/uuidx"
)

// Bouncer turns a failed delivery into a notification back to the sender, and
// puts it in the queue.
//
// It is a type of its own rather than a method on Runner because nothing about
// it needs the delivery loop: the SMTP session is its second caller, refusing a
// recipient after the message was already accepted and so having no SMTP reply
// left to say so in.
type Bouncer struct {
	Spool *Spool

	// Enabled off means failures are recorded and logged but nobody is told.
	Enabled bool
	// Full quotes the whole original message rather than just its headers.
	Full bool
	// MaxReturnBytes caps a full return; past it, headers only.
	MaxReturnBytes int64

	// Postmaster is the address notifications are sent as, already resolved.
	Postmaster string
	// Hostname is this gateway, for Reporting-MTA and the Received header.
	Hostname string

	// Route decides which relay group carries a bounce, given the address it is
	// addressed to. Supplied by the gateway so the rule engine makes the call,
	// exactly as it does for ordinary mail. Nil falls straight through to
	// RelayGroup.
	Route func(to string) (group, rule string, ok bool)
	// RelayGroup is the configured fallback when no rule claims the bounce.
	RelayGroup string

	Metrics *obs.Metrics
	Log     *slog.Logger

	// now is overridable so tests can pin the rendered message.
	now func() time.Time
}

// Request is one notification to generate.
type Request struct {
	// Env is the envelope that failed. Its sender is the notification's
	// recipient, and its identity becomes the notification's parent.
	Env *Envelope
	// Rcpts are the recipients to report on, and why.
	Rcpts []dsn.Rcpt
	Kind  dsn.Kind
	// Original is the message being reported on — a header block, or the whole
	// message when Full is set. Nil omits it.
	Original io.Reader
	// RetryUntil, on a delay report, is when the gateway will give up.
	RetryUntil time.Time
}

func (b *Bouncer) obs() *obs.Metrics {
	if b == nil || b.Metrics == nil {
		return obs.Discard
	}
	return b.Metrics
}

func (b *Bouncer) log() *slog.Logger {
	if b == nil || b.Log == nil {
		return slog.Default()
	}
	return b.Log
}

func (b *Bouncer) clock() time.Time {
	if b == nil || b.now == nil {
		return time.Now()
	}
	return b.now()
}

// Suppress reports whether a failure of this envelope must not be answered with
// a notification.
//
// A message with a null sender is either a bounce already or something that
// deliberately asked not to be answered; either way there is no address to send
// to. Generating one anyway is how two mail systems end up bouncing at each
// other forever, which is why RFC 5321 §4.5.5 requires the null sender on a
// notification in the first place.
func Suppress(env *Envelope) bool {
	return env == nil || env.IsDSN || strings.TrimSpace(env.MailFrom) == ""
}

// Bounce builds a notification and enqueues it. It reports whether one was
// queued — false with a nil error means it was deliberately not sent.
func (b *Bouncer) Bounce(req Request) (bool, error) {
	if b == nil || b.Spool == nil || !b.Enabled {
		return false, nil
	}
	env := req.Env
	if len(req.Rcpts) == 0 {
		return false, nil
	}
	if Suppress(env) {
		// Counted, because "we chose not to bounce" and "we forgot to" look the
		// same from outside and only one of them is fine.
		b.obs().DSNSuppressed.Add(1)
		b.log().Info("not bouncing a message with no return path",
			"uuid", env.UUID, "is_dsn", env.IsDSN, "rcpts", len(req.Rcpts))
		return false, nil
	}

	group, rule, ok := b.route(env.MailFrom)
	if !ok {
		// Loud, because the sender is not going to hear about their failed mail
		// and nothing else in the system will say so.
		b.obs().DSNUnroutable.Add(1)
		b.log().Error("no relay group for a bounce; the sender will not be told their mail failed",
			"uuid", env.UUID, "sender", env.MailFrom,
			"hint", "set dsn.relay_group, or add a route rule matching msg.is_dsn")
		return false, nil
	}

	now := b.clock()

	// The notification hangs off the delivery that failed: conn X, txn X.1,
	// envelope X.1.2 produces a bounce X.1.2.<n>. The hierarchy is a literal
	// prefix chain, so `WHERE uuid LIKE 'X%'` still finds the whole tree and the
	// id itself says which delivery bounced. A freshly minted root would satisfy
	// the same validation and leave an audit row with no parent anywhere.
	env.DSNSeq++
	id := uuidx.ID(env.UUID).Child(env.DSNSeq)

	body, err := dsn.Build(dsn.Report{
		ReportingMTA: b.Hostname,
		From:         b.Postmaster,
		To:           env.MailFrom,
		Kind:         req.Kind,
		OriginalID:   env.UUID,
		// The sender's own ENVID, which is a different thing from OriginalID
		// above — see dsn.Report.
		EnvelopeID:  env.DSNEnvID,
		MessageID:   id.String() + "@" + b.Hostname,
		ArrivalDate: time.UnixMilli(env.QueuedAt),
		Now:         now,
		Rcpts:       req.Rcpts,
		Original:    req.Original,
		Full:        b.QuotesFullMessage(env, req.Kind),
		RetryUntil:  req.RetryUntil,
	})
	if err != nil {
		return false, fmt.Errorf("build notification for %s: %w", env.UUID, err)
	}

	// The body is named for the notification ITSELF, not for the envelope it
	// reports on. Both satisfy the ownership rule gcBody resolves referrers by,
	// but only this one is unique per notification — and an envelope can produce
	// more than one: a failure and a relayed report in the same attempt, or a
	// delay warning at four hours and a failure at four days. Named for the
	// parent, the second write silently replaced the first's bytes, so a sender
	// with a delay warning still queued received the failure report twice.
	name, size, err := b.Spool.WriteBody(id.String(), strings.NewReader(string(body)))
	if err != nil {
		return false, fmt.Errorf("spool notification for %s: %w", env.UUID, err)
	}

	out := &Envelope{
		Version:  EnvelopeVersion,
		UUID:     id.String(),
		TxnUUID:  env.UUID,
		ConnUUID: env.ConnUUID,
		Body:     name,
		BodySize: size,
		// RFC 5321 §4.5.5: a notification is sent with a null return path, so
		// that a failure to deliver IT cannot produce another one. This is also
		// what Suppress reads on the way back in.
		MailFrom: "",
		Rcpts:    []Recipient{{Addr: env.MailFrom, Status: StatusPending}},
		RelayGrp: group,
		QueuedAt: now.UnixMilli(),
		NextAt:   now.UnixMilli(),
		IsDSN:    true,
	}

	if err := b.Spool.Enqueue(out); err != nil {
		// The body would otherwise be an orphan until the next boot sweep.
		if rmErr := b.Spool.RemoveBody(name); rmErr != nil {
			b.log().Warn("cannot remove the body of a notification that failed to queue",
				"body", name, "err", rmErr)
		}
		return false, fmt.Errorf("queue notification for %s: %w", env.UUID, err)
	}

	b.obs().DSNGenerated.Add(1)
	b.log().Info("queued a delivery status notification",
		"uuid", out.UUID, "original", env.UUID, "to", env.MailFrom,
		"kind", req.Kind.Action(), "relay_group", group, "route_rule", rule,
		"rcpts", len(req.Rcpts))
	return true, nil
}

// route picks the relay group for a notification addressed to `to`.
//
// The rule engine is asked first, so a bounce is routed by the same declarative
// configuration as everything else and an operator can write `msg.is_dsn` rules
// against it. Only a relay decision counts: a route rule that discards or
// rejects is a decision about ordinary mail that would silently black-hole
// notifications, so it falls through to the configured group instead.
func (b *Bouncer) route(to string) (group, rule string, ok bool) {
	if b.Route != nil {
		if g, r, found := b.Route(to); found && g != "" {
			return g, r, true
		}
	}
	if b.RelayGroup != "" {
		return b.RelayGroup, "dsn.relay_group", true
	}
	return "", "", false
}

// QuotesFullMessage reports whether a notification about a message of this size
// should carry the whole thing rather than only its headers.
//
// Callers ask before reading, because the two answers need different readers and
// the decision cannot be made from a stream that has already been consumed. The
// cap is why: RFC 3461 §6.2 permits returning headers only, and without it one
// 25 MiB message failing across three relay groups would spool three 25 MiB
// notifications.
// It takes the envelope rather than a size so the sender's RET= is consulted in
// the same breath, and Bounce can ask the identical question when it labels the
// quoted part. Splitting those two — a caller choosing the reader from one rule
// and Bounce labelling it from another — is how a message over the cap came to
// be returned as headers under a message/rfc822 content type.
//
// Precedence, in one place so it is not re-derived at each site: an explicit
// RET wins over dsn.return, and MaxReturnBytes caps FULL either way. The cap
// still applies to RET=FULL deliberately — §6.2 always permits headers only,
// and a per-message parameter must not be a way to make one 25 MiB message
// spool three 25 MiB notifications.
func (b *Bouncer) QuotesFullMessage(env *Envelope, k dsn.Kind) bool {
	if b == nil || env == nil {
		return false
	}

	// RFC 3461 §5.2.1: a success notification returns headers only, whatever
	// RET says. Quoting a delivered message back at its own sender is not
	// evidence of anything they do not already have, and on a mailing list it
	// is the whole message multiplied by the membership.
	if k == dsn.KindRelayed {
		return false
	}

	full := b.Full
	switch strings.ToUpper(env.DSNRet) {
	case "FULL":
		full = true
	case "HDRS":
		full = false
	}
	if !full {
		return false
	}

	if b.MaxReturnBytes > 0 && env.BodySize > b.MaxReturnBytes {
		b.log().Info("quoting headers only in a notification; the message is over dsn.max_return_bytes",
			"size", env.BodySize, "max", b.MaxReturnBytes)
		return false
	}
	return true
}

// headerCap bounds how much of a message is read looking for the end of its
// headers. A message with no blank line is malformed; reading it all into memory
// to discover that is not the right response.
const headerCap = 1 << 20

// HeaderBlock reads the header portion of a message, up to the first blank line.
//
// Used when a notification quotes headers only, which is the default: the
// headers are what identifies the message to the sender, and returning the body
// to an address that may not have asked for it is a policy choice rather than a
// courtesy.
func HeaderBlock(r io.Reader) string {
	var b strings.Builder
	br := bufio.NewReader(io.LimitReader(r, headerCap))
	for {
		line, err := br.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			break
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return b.String()
}
