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

	Response string        // the relay's reply to end-of-DATA
	Delay    time.Duration // wall time for the attempt

	Rcpts []RcptResult
	Err   error
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

	c, err := connect(ctx, relay, opts, res)
	if err != nil {
		res.Err = err
		return res
	}
	defer c.Close()

	if err := authenticate(c, relay, res); err != nil {
		res.Err = err
		return res
	}

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
		// Nothing to send; every recipient already has an outcome.
		_ = c.Quit()
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
	_ = c.Quit()
	return res
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
func connect(ctx context.Context, relay relays.Relay, opts Options, res *Result) (*smtp.Client, error) {
	policy := relay.TLSPolicy()

	if policy != relays.TLSNone {
		conn, err := dial(ctx, relay.Addr(), opts)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", relay.Addr(), err)
		}
		recordPeer(conn, res)

		c, err := smtp.NewClientStartTLS(conn, tlsConfigFor(relay, opts))
		if err == nil {
			res.TLS = true
			// STARTTLS resets the session, so EHLO again with our real name.
			if err := c.Hello(opts.LocalName); err != nil {
				c.Close()
				return nil, fmt.Errorf("EHLO after STARTTLS: %w", err)
			}
			return c, nil
		}

		// NewClientStartTLS closes the connection on failure.
		if policy == relays.TLSRequired {
			return nil, fmt.Errorf("relay %s requires TLS but STARTTLS failed: %w", relay.Name, err)
		}
		// Opportunistic: fall back to an unencrypted session.
		res.TLS = false
	}

	conn, err := dial(ctx, relay.Addr(), opts)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", relay.Addr(), err)
	}
	recordPeer(conn, res)

	c := smtp.NewClient(conn)
	if err := c.Hello(opts.LocalName); err != nil {
		c.Close()
		return nil, fmt.Errorf("EHLO: %w", err)
	}
	return c, nil
}

// recordPeer captures the relay's address for the audit event, before anything
// downstream can fail.
func recordPeer(conn net.Conn, res *Result) {
	if ap, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		res.IP = ap.AddrPort().Addr().Unmap().String()
	}
}

func tlsConfigFor(relay relays.Relay, opts Options) *tls.Config {
	cfg := &tls.Config{ServerName: relay.Exchange}
	if opts.TLSConfig != nil {
		cfg = opts.TLSConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = relay.Exchange
		}
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
