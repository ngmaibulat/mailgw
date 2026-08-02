// Package deliver performs one outbound SMTP delivery attempt against a relay.
package deliver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// Outcome classifies what happened to one recipient.
type Outcome int

const (
	// OutcomeDelivered — the relay accepted this recipient and the message.
	OutcomeDelivered Outcome = iota
	// OutcomeDeferred — a 4xx; try again later.
	OutcomeDeferred
	// OutcomeRejected — a 5xx; give up and bounce.
	OutcomeRejected
)

// RcptResult is the per-recipient outcome of an attempt.
type RcptResult struct {
	Addr     string
	Outcome  Outcome
	Code     int
	Enhanced string
	Message  string
}

// Result is the outcome of one attempt against one relay.
//
// Err being non-nil means the failure was connection-level (dial, TLS, AUTH,
// or a rejected MAIL FROM) and the caller should try the next relay in the
// group. When Err is nil, every recipient carries its own outcome.
type Result struct {
	Relay relays.Relay
	Host  string
	IP    string
	Port  string

	TLS       bool // the session was encrypted
	TLSForced bool // encryption was required by policy
	Auth      bool // the session authenticated

	// TLSDowngraded records that an opportunistic STARTTLS upgrade was attempted
	// and failed, so the message went out in the clear. TLSDowngradeReason is
	// why.
	//
	// Reported rather than counted here because this package has no obs.Metrics
	// and no logger — every delivery counter is incremented by the caller from
	// this struct, the way Reused and TLS already are. The silence was the
	// defect: a stripped session left no trace but TLS=false on the audit row.
	TLSDowngraded      bool
	TLSDowngradeReason string

	Response string        // the relay's reply to end-of-DATA
	Delay    time.Duration // wall time for the attempt

	// Reused reports that this attempt was carried by an already-open connection
	// rather than a fresh dial.
	Reused bool

	Rcpts []RcptResult
	Err   error

	// poolable records that the session ended in a state a further message could
	// be sent on. Unexported: it is a conversation between Deliver and the pool,
	// not something a caller should act on.
	poolable bool
	// messages is how many messages this connection has carried, in and out.
	messages int
}

// Accepted lists the recipients the relay took.
func (r *Result) Accepted() []string {
	var out []string
	for _, rr := range r.Rcpts {
		if rr.Outcome == OutcomeDelivered {
			out = append(out, rr.Addr)
		}
	}
	return out
}

// Message is what to deliver.
type Message struct {
	From     string // "" is a null sender
	Rcpts    []string
	Body     io.Reader
	SMTPUTF8 bool
	Body8Bit bool
}

// Options tunes an attempt.
type Options struct {
	// LocalName is the EHLO name we present.
	LocalName      string
	ConnectTimeout time.Duration
	DataTimeout    time.Duration
	// TLSConfig is the base config for STARTTLS; ServerName is filled in.
	TLSConfig *tls.Config
	// Dial is overridable so tests can inject a connection.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Pool reuses relay connections across envelopes. Nil dials every time,
	// which is the default and every existing deployment.
	Pool *Pool
}

// Deliver makes one attempt against one relay.
//
// It never returns an error directly: every failure is reported through Result,
// because the caller needs the connection metadata (host, ip, tls, auth) for the
// audit event whether the attempt succeeded or not.
func Deliver(ctx context.Context, relay relays.Relay, msg Message, opts Options) *Result {
	start := time.Now()
	res := &Result{
		Relay:     relay,
		Host:      relay.Exchange,
		Port:      relay.Port.String(),
		TLSForced: relay.TLSPolicy() == relays.TLSRequired,
	}
	defer func() { res.Delay = time.Since(start) }()

	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 30 * time.Second
	}
	if opts.DataTimeout <= 0 {
		opts.DataTimeout = 10 * time.Minute
	}
	if opts.LocalName == "" {
		opts.LocalName = "localhost"
	}

	// Armed before the first dial, so a cancelled context interrupts the
	// handshake as well as the long DATA phase.
	guard := newConnGuard(ctx)
	defer guard.release()

	// A pooled connection is already greeted, TLS-negotiated and authenticated,
	// and has just answered RSET. Everything below is identical either way —
	// including the guard, which is armed on the borrowed socket too. Without
	// that, cancellation simply did not reach a reused connection and an
	// in-flight DATA ran to SubmissionTimeout.
	c, conn := acquire(relay, opts, res, guard)
	reused := c != nil
	if c == nil {
		var err error
		c, conn, err = connect(ctx, relay, opts, res, guard)
		if err != nil {
			res.Err = err
			return res
		}
		if err := authenticate(c, relay, res); err != nil {
			res.Err = err
			// Never pooled: a connection that failed AUTH is not one to keep.
			_ = c.Close()
			return res
		}
	} else {
		guard.use(conn)
	}
	res.Reused = reused

	// One place decides the connection's fate, whichever way this function
	// returns. Anything that ended in an error is discarded rather than pooled:
	// after a failed command the protocol state is unknown, and reusing it would
	// carry this message's failure into the next one.
	defer func() {
		// Disarm FIRST. Defers run LIFO, so this one runs BEFORE the
		// `defer guard.release()` registered above — which would otherwise leave
		// a window where the connection is back in the pool and the guard is
		// still armed on it, and a cancellation landing there would close a
		// connection this attempt no longer owns. release is safe to call twice.
		guard.release()

		switch {
		case opts.Pool == nil:
			// No pooling: QUIT and close, exactly as before this existed.
			_ = quietQuit(c)
		case res.Err == nil && res.poolable:
			opts.Pool.Put(relay, opts, c, conn, res.messages)
		default:
			opts.Pool.Discard(c)
		}
	}()

	mailOpts := &smtp.MailOptions{}
	if msg.SMTPUTF8 {
		if ok, _ := c.Extension("SMTPUTF8"); ok {
			mailOpts.UTF8 = true
		}
	}
	if msg.Body8Bit {
		if ok, _ := c.Extension("8BITMIME"); ok {
			mailOpts.Body = smtp.Body8BitMIME
		}
	}
	if err := c.Mail(msg.From, mailOpts); err != nil {
		res.Err = fmt.Errorf("MAIL FROM: %w", err)
		return res
	}

	// Per-recipient outcomes: one bad address must not sink the others.
	var accepted []string
	for _, addr := range msg.Rcpts {
		if err := c.Rcpt(addr, nil); err != nil {
			res.Rcpts = append(res.Rcpts, classify(addr, err))
			continue
		}
		accepted = append(accepted, addr)
	}
	if len(accepted) == 0 {
		// Nothing to send; every recipient already has an outcome. The session
		// itself is healthy — the relay answered every command — so it can carry
		// the next message once RSET clears the aborted transaction.
		//
		// Counted all the same: the relay counted this transaction, and relays
		// that cap invalid recipients per connection answer the next message
		// with a 421 — which is exactly what MaxMessages exists to get ahead of.
		res.messages++
		res.poolable = true
		return res
	}

	w, err := c.Data()
	if err != nil {
		// DATA was refused for the whole message; apply it to each accepted
		// recipient so they are not silently lost.
		for _, addr := range accepted {
			res.Rcpts = append(res.Rcpts, classify(addr, err))
		}
		return res
	}
	if _, err := io.Copy(w, msg.Body); err != nil {
		_ = w.Close()
		res.Err = fmt.Errorf("write body: %w", err)
		return res
	}

	resp, err := w.CloseWithResponse()
	if err != nil {
		for _, addr := range accepted {
			res.Rcpts = append(res.Rcpts, classify(addr, err))
		}
		return res
	}

	res.Response = strings.TrimSpace(resp.StatusText)
	if res.Response == "" {
		res.Response = "250 OK"
	}
	for _, addr := range accepted {
		res.Rcpts = append(res.Rcpts, RcptResult{
			Addr:    addr,
			Outcome: OutcomeDelivered,
			Code:    250,
			Message: res.Response,
		})
	}

	// The message completed cleanly, so the connection is in a known-good state
	// and has now carried one more message.
	res.messages++
	res.poolable = true
	return res
}

// acquire tries to borrow an open connection for this relay, returning it with
// the socket underneath it.
//
// Returns nils whenever pooling is off or nothing usable is available. A pool
// miss is never an error and is deliberately indistinguishable from pooling
// being disabled: the caller dials, exactly as it always did.
func acquire(relay relays.Relay, opts Options, res *Result, guard *connGuard) (*smtp.Client, net.Conn) {
	if opts.Pool == nil {
		return nil, nil
	}
	// Armed and bounded before the pool probes the candidate with RSET, not
	// after: the probe is a network round trip on a socket that may already be
	// black-holing, and until M16 neither the context nor the configured
	// timeouts reached it.
	c, conn, messages := opts.Pool.Get(relay, opts, func(client *smtp.Client, raw net.Conn) {
		guard.use(raw)
		applyTimeouts(client, opts)
	})
	if c == nil {
		return nil, nil
	}

	// A reused connection carries no fresh EHLO or handshake, so the audit
	// event is filled in from what the relay configuration already states.
	res.Host = relay.Exchange
	res.Port = relay.Port.String()
	res.TLS = relay.TLSPolicy() != relays.TLSNone
	res.Auth = relay.AuthUser != ""
	res.messages = messages

	// The peer IP used to be left blank here, on the grounds that claiming an
	// address this attempt never resolved would be a guess. With the socket in
	// hand it is not a guess — it is the address this message is being sent
	// over — so the audit row gains a fact it previously omitted.
	recordPeer(conn, res)
	return c, conn
}

func dial(ctx context.Context, addr string, opts Options) (net.Conn, error) {
	if opts.Dial != nil {
		return opts.Dial(ctx, "tcp", addr)
	}
	d := &net.Dialer{Timeout: opts.ConnectTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// connect dials the relay, applies its TLS policy, and sends EHLO.
//
// go-smtp only exposes STARTTLS through constructors that take the raw
// connection (NewClientStartTLS), so the policy has to be decided before the
// greeting rather than after probing EHLO. For an opportunistic relay that
// turns out not to offer STARTTLS, that means one wasted connection and a
// redial in the clear — acceptable because these are configured smarthosts
// whose capabilities are stable, not hosts discovered per message.
// Each dialled connection is published to guard so a context cancellation can
// close it: go-smtp's client calls take no context, and closing the connection
// underneath them is the only way to interrupt one in flight.
// It returns the raw net.Conn alongside the client so the caller can hand it to
// the pool: a borrowed connection needs the same guard a dialled one gets, and
// the pool is the only thing that can carry the socket between attempts.
func connect(ctx context.Context, relay relays.Relay, opts Options, res *Result, guard *connGuard) (*smtp.Client, net.Conn, error) {
	policy := relay.TLSPolicy()

	if policy != relays.TLSNone {
		conn, err := dial(ctx, relay.Addr(), opts)
		if err != nil {
			return nil, nil, fmt.Errorf("dial %s: %w", relay.Addr(), err)
		}
		recordPeer(conn, res)
		guard.use(conn)

		// NewClientStartTLS builds its own client and runs the greeting and the
		// STARTTLS handshake before we can reach it, so that one window keeps
		// go-smtp's 5-minute default; everything after it is bounded below.
		c, err := smtp.NewClientStartTLS(conn, tlsConfigFor(relay, opts))
		if err == nil {
			applyTimeouts(c, opts)
			// STARTTLS resets the session, so EHLO again with our real name.
			if err := c.Hello(opts.LocalName); err != nil {
				c.Close()
				return nil, nil, fmt.Errorf("EHLO after STARTTLS: %w", err)
			}
			// Recorded only once the encrypted session has carried a command.
			// Go completes the handshake lazily, so a certificate that
			// `required` rejects surfaces on the EHLO above rather than out of
			// NewClientStartTLS — setting this any earlier stamped TLS=true on
			// the audit row for a session that never authenticated the peer.
			res.TLS = true
			return c, conn, nil
		}

		// NewClientStartTLS closes the connection on failure.
		if policy == relays.TLSRequired {
			return nil, nil, fmt.Errorf("relay %s requires TLS but STARTTLS failed: %w", relay.Name, err)
		}
		// Opportunistic: fall back to an unencrypted session, and say so. With
		// verification off above, reaching here means the relay would not or
		// could not encrypt at all — not that its certificate was unacceptable.
		res.TLS = false
		res.TLSDowngraded = true
		res.TLSDowngradeReason = err.Error()
	}

	conn, err := dial(ctx, relay.Addr(), opts)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", relay.Addr(), err)
	}
	recordPeer(conn, res)
	guard.use(conn)

	c := smtp.NewClient(conn)
	applyTimeouts(c, opts)
	if err := c.Hello(opts.LocalName); err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("EHLO: %w", err)
	}
	return c, conn, nil
}

// connGuard closes whichever connection is currently live when the context is
// cancelled.
//
// It exists because go-smtp's client API takes no context: once a command is
// blocked on a read, only closing the connection unblocks it. The guard is armed
// before the first dial rather than after connect returns, because the greeting
// and the STARTTLS handshake can stall just as easily as the DATA phase.
type connGuard struct {
	mu   sync.Mutex
	conn net.Conn
	stop func() bool
}

func newConnGuard(ctx context.Context) *connGuard {
	g := &connGuard{}
	if ctx != nil {
		g.stop = context.AfterFunc(ctx, g.closeConn)
	}
	return g
}

// use publishes the connection the attempt is now running on. An opportunistic
// relay that turns out not to offer STARTTLS is redialled, so this can be called
// more than once; the previous conn has already been closed by go-smtp.
func (g *connGuard) use(c net.Conn) {
	g.mu.Lock()
	g.conn = c
	g.mu.Unlock()
}

func (g *connGuard) closeConn() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn != nil {
		_ = g.conn.Close()
	}
}

// release detaches the guard from the context once the attempt is over, so a
// later cancellation cannot close a connection this attempt no longer owns.
//
// Stopping the AfterFunc is not enough on its own. Its contract is that a false
// return means the context has ALREADY ended and closeConn has already been
// started on its own goroutine — where it may simply be blocked on g.mu. So the
// connection is cleared here, under that same mutex: either closeConn gets there
// first (and the connection is dead before it is offered to the pool, which the
// checkout probe catches) or this does, and there is nothing left for it to
// close. Without it, a cancellation landing in that window closes a socket that
// by then belongs to another delivery.
func (g *connGuard) release() {
	if g.stop != nil {
		g.stop()
	}
	g.mu.Lock()
	g.conn = nil
	g.mu.Unlock()
}

// applyTimeouts maps the configured budgets onto the go-smtp client.
//
// This is what makes `outbound.connect_timeout` and `outbound.data_timeout` in
// server.yaml mean anything past the dial. Both were threaded all the way into
// deliver.Options and then never applied, so the effective timeouts were
// go-smtp's own defaults — 5 minutes per command, 12 minutes for the DATA
// submission — no matter what the operator configured.
//
// Setting a deadline on the raw conn does NOT work and must not be reintroduced:
// go-smtp sets its own deadline around every command and *clears* it afterwards
// (client.go:190-191, `defer c.conn.SetDeadline(time.Time{})`), so an external
// deadline is wiped by the first command. CommandTimeout/SubmissionTimeout are
// the supported control.
//
// `connect_timeout` covering each command round trip is a slight stretch of the
// name, but it is the right budget: both describe how long we are willing to
// wait on this relay for one answer. Splitting them would mean a new config key.
func applyTimeouts(c *smtp.Client, opts Options) {
	if opts.ConnectTimeout > 0 {
		c.CommandTimeout = opts.ConnectTimeout
	}
	if opts.DataTimeout > 0 {
		c.SubmissionTimeout = opts.DataTimeout
	}
}

// recordPeer captures the relay's address for the audit event, before anything
// downstream can fail.
func recordPeer(conn net.Conn, res *Result) {
	if ap, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		res.IP = ap.AddrPort().Addr().Unmap().String()
	}
}

// tlsConfigFor builds the STARTTLS configuration for one relay, and what it does
// depends on the relay's policy.
//
// Verification used to be on for both policies, and any failure — an expired
// certificate, a name mismatch, a private CA, or an active attacker — fell
// through to a cleartext redial. So the common case, a smarthost or an MX with a
// self-signed certificate, silently delivered in the clear. use_mx made it
// broader still, since Expand points ServerName at each exchanger's own name.
//
// RFC 7435: opportunistic security is encryption WITHOUT authentication. There
// is nothing to fall back to that is better than an unauthenticated session, and
// the alternative this code actually took was plaintext. So opportunistic does
// not verify, and `required` — the policy an operator sets deliberately, for a
// relay whose certificate they trust — verifies fully and never falls back.
func tlsConfigFor(relay relays.Relay, opts Options) *tls.Config {
	cfg := &tls.Config{ServerName: relay.Exchange}
	if opts.TLSConfig != nil {
		cfg = opts.TLSConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = relay.Exchange
		}
	}

	if relay.TLSPolicy() == relays.TLSOpportunistic {
		cfg.InsecureSkipVerify = true
	}

	// TLS 1.0 and 1.1 are what a peer reaches for only when it has nothing
	// better. The inbound side (smtpsrv.NewTLSConfig) and internal/central both
	// pin 1.2 already; this was the last place that would still negotiate 1.0.
	// A `tls: required` relay stuck on 1.0 will now fail rather than connect.
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}

// authenticate performs AUTH when the relay has credentials.
func authenticate(c *smtp.Client, relay relays.Relay, res *Result) error {
	if relay.AuthUser == "" {
		return nil
	}
	// Sending a password over an unencrypted link should be a deliberate
	// choice, not the default.
	if !res.TLS && !relay.AllowInsecureAuth {
		return fmt.Errorf("relay %s: refusing to AUTH over an unencrypted connection (set allow_insecure_auth to override)", relay.Name)
	}
	if ok, _ := c.Extension("AUTH"); !ok {
		return fmt.Errorf("relay %s: credentials configured but AUTH is not offered", relay.Name)
	}
	if err := c.Auth(sasl.NewPlainClient("", relay.AuthUser, relay.Password())); err != nil {
		return fmt.Errorf("AUTH with %s: %w", relay.Name, err)
	}
	res.Auth = true
	return nil
}

// classify turns an SMTP error into a per-recipient outcome. A 4xx is
// retryable, a 5xx is not; anything unclassifiable is treated as retryable,
// because deferring mail is recoverable and bouncing it is not.
func classify(addr string, err error) RcptResult {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		out := OutcomeDeferred
		if se.Code >= 500 {
			out = OutcomeRejected
		}
		return RcptResult{
			Addr:     addr,
			Outcome:  out,
			Code:     se.Code,
			Enhanced: fmt.Sprintf("%d.%d.%d", se.EnhancedCode[0], se.EnhancedCode[1], se.EnhancedCode[2]),
			Message:  strings.TrimSpace(se.Message),
		}
	}
	return RcptResult{
		Addr:    addr,
		Outcome: OutcomeDeferred,
		Code:    451,
		Message: err.Error(),
	}
}
