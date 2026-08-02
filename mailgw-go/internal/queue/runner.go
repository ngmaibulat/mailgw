package queue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/dsn"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// DeliverFunc performs one attempt against one relay. It is a field rather than
// a direct call so tests can substitute a fake without opening sockets.
type DeliverFunc func(context.Context, relays.Relay, deliver.Message, deliver.Options) *deliver.Result

// Notifier is the subset of the events client the runner uses.
type Notifier interface {
	Send(events.Envelope)
}

// RunnerConfig configures the delivery loop.
type RunnerConfig struct {
	Relays       *relays.Table
	PollInterval time.Duration
	Concurrency  int
	// PerGroup bounds concurrent connections to a single relay group, so one
	// slow relay cannot consume the whole worker pool.
	PerGroup       int
	LocalName      string
	ConnectTimeout time.Duration
	DataTimeout    time.Duration

	// Backoff returns the delay before attempt n (1-based).
	Backoff func(attempt int) time.Duration
	// Jitter spreads retries; 0.15 means +/-15%.
	Jitter      float64
	MaxLifetime time.Duration
	// DelayWarnAfter is how long a message may sit in the queue before the
	// sender is told it is late. Zero disables the warning.
	DelayWarnAfter time.Duration

	// Bouncer generates delivery status notifications. Nil disables them, which
	// is what a test constructing a RunnerConfig by literal gets.
	Bouncer *Bouncer

	// MX resolves use_mx relays. Nil is fine when no relay uses one.
	MX *deliver.Resolver
	// Pool reuses relay connections between envelopes. Nil dials every time.
	Pool *deliver.Pool

	DeliveryURL string
	Events      Notifier
	Deliver     DeliverFunc
	Log         *slog.Logger
	// Signer adds a DKIM-Signature at delivery. Nil disables signing, which is
	// the shipped default and what a test constructing a RunnerConfig by literal
	// gets.
	Signer *Signer

	// Metrics may be nil; see Runner.obs. Production always sets it.
	Metrics *obs.Metrics
	// Gateway labels the audit rows this runner writes, so the log tables can
	// say which box relayed a message. Resolved once at bring-up.
	Gateway string
}

// Runner drains the spool.
type Runner struct {
	spool *Spool
	cfg   RunnerConfig
	log   *slog.Logger

	nudge chan struct{}
	sem   map[string]chan struct{}
	mu    sync.Mutex
	wg    sync.WaitGroup
}

// NewRunner builds a delivery loop over spool.
func NewRunner(spool *Spool, cfg RunnerConfig) *Runner {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 10
	}
	if cfg.PerGroup <= 0 {
		cfg.PerGroup = 5
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(int) time.Duration { return time.Minute }
	}
	if cfg.Deliver == nil {
		cfg.Deliver = deliver.Deliver
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Runner{
		spool: spool,
		cfg:   cfg,
		log:   cfg.Log,
		nudge: make(chan struct{}, 1),
		sem:   map[string]chan struct{}{},
	}
}

// obs returns the counter registry, or a shared discard when none was supplied.
//
// Deliberately not defaulted in NewRunner: a RunnerConfig built by literal in a
// test would still leave the field nil, and a nil dereference here happens on
// the delivery path. One accessor removes the whole class of failure.
func (r *Runner) obs() *obs.Metrics {
	if r.cfg.Metrics != nil {
		return r.cfg.Metrics
	}
	return obs.Discard
}

// sign returns the DKIM-Signature header field to put in front of this
// envelope, or "" when the message is not signed.
//
// A failure here NEVER stops the delivery. Refusing to relay because a key file
// lost its read permission would turn a configuration slip into a mail outage,
// and the message still reaches its recipient unsigned — it just may be
// distrusted at the far end, which is what dkim_sign_failed exists to make
// visible. The distinction the counter draws is deliberate: "no key for this
// domain" is not signing, and is silent; "there is a key and it did not work" is
// failing to sign, and is loud.
func (r *Runner) sign(env *Envelope, prepend string, log *slog.Logger) string {
	if r.cfg.Signer == nil {
		return ""
	}
	open := func() (io.ReadCloser, error) { return r.spool.OpenBody(env.Body) }

	sig, err := r.cfg.Signer.Sign(open, prepend)
	switch {
	case err == nil:
		r.obs().DKIMSigned.Add(1)
		return sig
	case errors.Is(err, ErrNoKey):
		log.Debug("not signing: no DKIM key for this message's From domain", "err", err)
		return ""
	default:
		r.obs().DKIMSignFailed.Add(1)
		log.Warn("DKIM signing failed; the message is going out UNSIGNED", "err", err)
		return ""
	}
}

// Nudge asks the runner to look for work immediately, instead of waiting for
// the next poll. Non-blocking: a pending nudge is enough.
func (r *Runner) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// Start runs the loop until ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	if n, err := r.spool.Recover(); err != nil {
		r.log.Error("queue recovery failed", "err", err)
	} else if n > 0 {
		// Work left in flight by a previous process. Some of it may already
		// have been delivered; redelivery is inherent to a spooling MTA.
		r.log.Warn("recovered in-flight envelopes from a previous run", "count", n)
	}

	slots := make(chan struct{}, r.cfg.Concurrency)

	timer := time.NewTimer(r.cfg.PollInterval)
	defer timer.Stop()

	for {
		next, hasNext := r.drain(ctx, slots)

		// Stop and drain before Reset: a timer that has already fired leaves a
		// value in the channel, and reusing it without clearing that would make
		// the next select return immediately.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(r.sleepFor(next, hasNext))

		select {
		case <-ctx.Done():
			r.wg.Wait()
			return
		case <-timer.C:
		case <-r.nudge:
		}
	}
}

// minSleep keeps the loop from spinning when work is due within the same
// instant — for example when every worker slot is busy and drain could not
// dispatch anything.
const minSleep = 50 * time.Millisecond

// sleepFor returns how long the scheduler may sleep.
//
// Queue filenames carry their due-second, so listing the directory already
// reveals exactly when the next deferred envelope is owed; sleeping until then
// is what makes a retry run on time instead of up to a poll interval late.
//
// PollInterval is the ceiling rather than a fixed period. Something can appear
// in q/ without going through Enqueue — an operator releasing a quarantined
// message by hand, say — and nothing would signal that, so the loop still
// rescans on its own eventually. Because due work no longer waits for that
// rescan, the interval can be raised well past the old 5s without delaying
// anything.
func (r *Runner) sleepFor(next time.Time, hasNext bool) time.Duration {
	d := r.cfg.PollInterval
	if d <= 0 {
		d = 5 * time.Second
	}
	if hasNext {
		if until := time.Until(next); until < d {
			d = until
		}
	}
	if d < minSleep {
		d = minSleep
	}
	return d
}

// drain claims and dispatches every envelope that is due, and reports when the
// earliest envelope that is not yet due comes due.
func (r *Runner) drain(ctx context.Context, slots chan struct{}) (next time.Time, hasNext bool) {
	names, next, hasNext, err := r.spool.ReadyAndNext(time.Now())
	if err != nil {
		r.log.Error("cannot list queue", "err", err)
		return time.Time{}, false
	}

	for _, name := range names {
		if ctx.Err() != nil {
			return
		}

		env, inflight, err := r.spool.Claim(name)
		if err != nil {
			// Another worker won the race, or the file was unreadable and has
			// been parked. Either way, move on.
			continue
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			// Put it back so it is not stranded in inflight/.
			_ = r.spool.Reschedule(inflight, env, time.Now())
			return
		}

		r.wg.Add(1)
		go func(env *Envelope, inflight string) {
			defer r.wg.Done()
			defer func() { <-slots }()
			r.attempt(ctx, env, inflight)
		}(env, inflight)
	}
	return next, hasNext
}

// targets is the ordered dial list for one attempt over a relay group.
//
// Identical to group.Attempts for every ordinary relay. A use_mx member is
// expanded here, in place, into one synthetic relay per mail exchanger — so the
// group's own priority ordering still decides which MEMBER is tried first, and
// the DNS preference decides the order within that member. Expanding at the call
// site rather than inside the group keeps relays.Table free of DNS.
//
// A member whose MX lookup fails is skipped with a warning rather than failing
// the attempt: a group can hold both an MX-resolved member and a plain one, and
// a DNS outage should fall through to the second rather than stop delivery.
//
// The joined resolution errors come back as well, because when EVERY member is
// use_mx and DNS is down this returns nothing and the caller's relay loop never
// runs — leaving it with no result to explain the deferral. The warning above
// names which relay failed and the error carries the reason to the envelope.
func (r *Runner) targets(ctx context.Context, group *relays.Group, log *slog.Logger) ([]relays.Relay, error) {
	members := group.Attempts(nil)

	var needsMX bool
	for _, m := range members {
		if m.UseMX {
			needsMX = true
			break
		}
	}
	if !needsMX {
		return members, nil
	}

	out := make([]relays.Relay, 0, len(members))
	var errs []error
	for _, m := range members {
		hosts, err := deliver.Expand(ctx, r.cfg.MX, m)
		if err != nil {
			log.Warn("cannot resolve mail exchangers; skipping this relay",
				"relay", m.Name, "domain", m.Exchange, "err", err)
			errs = append(errs, err)
			continue
		}
		out = append(out, hosts...)
	}
	return out, errors.Join(errs...)
}

// groupSem returns the concurrency limiter for a relay group.
func (r *Runner) groupSem(name string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sem[name]
	if !ok {
		s = make(chan struct{}, r.cfg.PerGroup)
		r.sem[name] = s
	}
	return s
}

// attempt makes one pass over an envelope's relay group.
func (r *Runner) attempt(ctx context.Context, env *Envelope, inflight string) {
	// However this attempt ends, the queue has changed: the envelope was
	// removed, buried, or put back with a new due time. Wake the scheduler so
	// it recomputes how long it may sleep — without this, an envelope deferred
	// by a few seconds would wait for the next rescan instead.
	defer r.Nudge()

	// Counted here rather than after the relay lookup below: the unknown-group
	// and cannot-open-body paths defer without ever reaching the relay loop, so
	// skipping them here would let deferred/attempts exceed 1 — which is the
	// one ratio a delivery dashboard is actually read for.
	r.obs().DeliverAttempts.Add(1)

	// Incremented for the same reason and in the same place. It used to happen
	// after the relay loop, so the two early returns below deferred on a stale
	// count and their backoff never grew — they retried every 60s forever.
	env.Attempts++

	log := r.log.With("uuid", env.UUID, "relay_group", env.RelayGrp)

	group, ok := r.cfg.Relays.Lookup(env.RelayGrp)
	if !ok {
		// The relay group vanished from the config while this was queued.
		// Deferring is right: an operator can put it back.
		log.Error("unknown relay group; deferring")
		r.deferEnvelope(env, inflight, "unknown relay group")
		return
	}

	sem := r.groupSem(env.RelayGrp)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		_ = r.spool.Reschedule(inflight, env, time.Now())
		return
	}
	defer func() { <-sem }()

	pending := env.PendingRcpts()
	addrs := make([]string, 0, len(pending))
	for _, p := range pending {
		addrs = append(addrs, p.Addr)
	}

	targets, resolveErr := r.targets(ctx, group, log)

	// Signed once per attempt, outside the relay loop: a DKIM signature covers
	// the message and says nothing about who it is being handed to, so
	// recomputing it per target would be two file reads and an RSA operation
	// for identical bytes.
	//
	// It goes in FRONT of the prepend block, so the block itself — every
	// add_header action and any Authentication-Results from an inbound check —
	// is inside what the signature covers. A signature over the spooled body
	// alone would cover a message that never goes on the wire.
	prepend := env.PrependBlock()
	signature := r.sign(env, prepend, log)

	var last *deliver.Result
	for _, relay := range targets {
		body, err := r.spool.OpenBody(env.Body)
		if err != nil {
			log.Error("cannot open spooled body", "err", err)
			r.deferEnvelope(env, inflight, "cannot open body")
			return
		}

		// Headers added by an `add_header` action are prepended here rather
		// than baked into the spooled body, which stays shared by every
		// envelope of the transaction.
		var msgBody io.Reader = body
		if head := signature + prepend; head != "" {
			msgBody = io.MultiReader(strings.NewReader(head), body)
		}

		res := r.cfg.Deliver(ctx, relay, deliver.Message{
			From:     env.MailFrom,
			Rcpts:    addrs,
			Body:     msgBody,
			SMTPUTF8: env.SMTPUTF8,
			Body8Bit: env.Body8Bit,
		}, deliver.Options{
			LocalName:      r.cfg.LocalName,
			ConnectTimeout: r.cfg.ConnectTimeout,
			DataTimeout:    r.cfg.DataTimeout,
			Pool:           r.cfg.Pool,
		})
		body.Close()
		last = res

		if res.Reused {
			r.obs().DeliverConnReused.Add(1)
		}

		// An opportunistic relay that could not upgrade sent this message in the
		// clear. That is the intended behaviour of the policy, but it used to
		// leave no trace anywhere except TLS=false on the audit row — so a relay
		// that quietly stopped offering STARTTLS looked exactly like one that
		// never did.
		if res.TLSDowngraded {
			r.obs().DeliverTLSDowngraded.Add(1)
			log.Warn("STARTTLS failed on an opportunistic relay; message sent in the clear",
				"relay", relay.Name, "host", relay.Exchange, "err", res.TLSDowngradeReason)
		}

		if res.Err != nil {
			// Connection-level failure: try the next relay in the group.
			// Counted per RELAY, not per attempt — a group of three that is
			// wholly unreachable adds three, which is what makes the counter
			// useful for spotting one bad relay among several.
			r.obs().DeliverConnFail.Add(1)
			log.Warn("relay attempt failed", "relay", relay.Name, "err", res.Err)
			continue
		}

		r.record(env, res)
		break
	}

	// Every way this attempt can end has to leave its own reason behind, because
	// deferEnvelope's `if reason != ""` guard means an empty one silently keeps
	// the last attempt's. Both mailq -json and the eventual DSN diagnostic read
	// that field, and when no relay was contacted it is the only evidence there
	// is — nothing reached a recipient, so no recipient carries a LastMsg
	// either.
	switch {
	case last != nil && last.Err != nil:
		env.LastErr = last.Err.Error()
	case last != nil:
		// The relay answered and deferred recipients — greylisting, a mailbox
		// over quota, a rate limit. Without this arm LastErr kept whatever the
		// PREVIOUS attempt left, so mailq reported a connection refusal for a
		// message that was in fact being greylisted; on a first attempt of this
		// shape it stayed blank, which is the same hole the arms below close.
		if reason := deferralReason(last); reason != "" {
			env.LastErr = reason
		}
	case resolveErr != nil:
		env.LastErr = resolveErr.Error()
	default:
		env.LastErr = "no relay in group " + env.RelayGrp + " could be tried"
	}

	if env.Done() {
		// Before Complete, not after: Complete deletes the envelope and, with
		// it, the body this notification quotes.
		r.bounceFailed(env)
		r.notifyRelayed(env)
		if err := r.spool.Complete(inflight, env); err != nil {
			log.Error("cannot complete envelope", "err", err)
		}
		return
	}

	r.deferEnvelope(env, inflight, env.LastErr)
}

// bounceFailed reports the recipients a relay permanently rejected during this
// attempt.
//
// Collected once, at the end of the attempt, so an envelope with several
// rejected recipients produces ONE report naming all of them rather than one
// each — which is what the per-recipient section of RFC 3464 is for.
//
// Enqueued before the envelope is removed or buried. A crash in between
// re-queues the pre-attempt envelope and bounces a second time; the other order
// loses the bounce outright. That is the spool's standing trade — a duplicate is
// recoverable, missing mail is not — and it is why DSNSent is not a guarantee of
// exactly-once. Its job is to stop a repeat on the NEXT attempt, which is the
// case that would otherwise recur every retry for four days.
func (r *Runner) bounceFailed(env *Envelope) {
	if r.cfg.Bouncer == nil || Suppress(env) {
		if r.cfg.Bouncer != nil {
			// Still worth counting the suppression, once, if anything failed.
			for _, rc := range env.Rcpts {
				if rc.Status == StatusPermfail && !rc.DSNSent {
					r.obs().DSNSuppressed.Add(1)
					break
				}
			}
		}
		return
	}

	var rcpts []dsn.Rcpt
	// Collected while filtering, so DSNSent is set on exactly the recipients the
	// report named. Marking every permfail recipient — which is what this did
	// before NOTIFY existed, when the report always named all of them — would
	// record "we notified the sender" about somebody deliberately left out, and
	// that claim outlives the envelope in dead/.
	var reported []int
	for i := range env.Rcpts {
		rc := &env.Rcpts[i]
		if rc.Status != StatusPermfail || rc.DSNSent {
			continue
		}
		if !rc.WantsFailureDSN() {
			r.obs().DSNNotifySuppressed.Add(1)
			continue
		}
		reported = append(reported, i)
		rcpts = append(rcpts, dsn.Rcpt{
			Addr:        rc.Addr,
			OrigAddr:    rc.OrigRcpt,
			OrigType:    rc.OrigRcptType,
			Status:      enhancedFor(rc.LastCode, false),
			Diagnostic:  diagnosticFor(rc.LastCode, rc.LastMsg),
			LastAttempt: time.Now(),
		})
	}
	if len(rcpts) == 0 {
		// Every failed recipient asked not to be told. Counted as a suppressed
		// report as well as per recipient above, so the two counters agree about
		// how many notifications this gateway decided not to send.
		if len(env.Rcpts) > 0 {
			r.obs().DSNSuppressed.Add(1)
		}
		return
	}

	r.bounce(env, Request{Env: env, Rcpts: rcpts, Kind: dsn.KindFailed})

	for _, i := range reported {
		env.Rcpts[i].DSNSent = true
	}
}

// notifyRelayed tells a sender that asked with NOTIFY=SUCCESS that its message
// reached the next hop.
//
// This exists because advertising DSN commits to it. RFC 3461 §5.2.7 requires a
// "relayed" notification when SUCCESS was requested and the parameter was not
// passed on to the next hop — and this gateway never passes it on, so the case
// is every message. Without this, a sender would ask, be answered 250, and hear
// nothing: a promise made on the wire and silently dropped, which is the defect
// class this milestone exists to close.
//
// "relayed", not "delivered": a relay accepted the recipient, and what happened
// after that is not something this gateway can know.
func (r *Runner) notifyRelayed(env *Envelope) {
	// Suppress() first, as everywhere else: a bounce is never answered, and a
	// null sender has nowhere to send a success report either.
	if r.cfg.Bouncer == nil || Suppress(env) {
		return
	}

	var rcpts []dsn.Rcpt
	var reported []int
	for i := range env.Rcpts {
		rc := &env.Rcpts[i]
		// DSNSent doubles as "a notification has already been sent about this
		// recipient", which is exactly what it means. A delivered recipient can
		// never also be reported as failed, so the two uses cannot collide.
		if rc.Status != StatusDelivered || rc.DSNSent || !rc.WantsSuccessDSN() {
			continue
		}
		reported = append(reported, i)
		rcpts = append(rcpts, dsn.Rcpt{
			Addr:        rc.Addr,
			OrigAddr:    rc.OrigRcpt,
			OrigType:    rc.OrigRcptType,
			Diagnostic:  diagnosticFor(rc.LastCode, rc.LastMsg),
			LastAttempt: time.Now(),
		})
	}
	if len(rcpts) == 0 {
		return
	}

	r.bounce(env, Request{Env: env, Rcpts: rcpts, Kind: dsn.KindRelayed})

	for _, i := range reported {
		env.Rcpts[i].DSNSent = true
	}
}

// bounce renders and queues one notification, quoting the original message.
func (r *Runner) bounce(env *Envelope, req Request) {
	b := r.cfg.Bouncer

	body, err := r.spool.OpenBody(env.Body)
	if err != nil {
		// Report what happened without quoting the message. A notification
		// naming the recipient and the reason is worth far more than none.
		r.log.Warn("cannot quote the original message in a notification",
			"uuid", env.UUID, "err", err)
	} else {
		defer body.Close()

		// add_header actions are applied at delivery rather than baked into the
		// spooled body, so quoting the file alone would show the sender headers
		// the relay never actually saw.
		var original io.Reader = body
		if block := env.PrependBlock(); block != "" {
			original = io.MultiReader(strings.NewReader(block), body)
		}

		// Asked before reading: HeaderBlock consumes the stream, so the choice
		// cannot be made from the reader afterwards.
		if b.QuotesFullMessage(env, req.Kind) {
			req.Original = original
		} else {
			req.Original = strings.NewReader(HeaderBlock(original))
		}
	}

	if _, err := b.Bounce(req); err != nil {
		r.log.Error("cannot generate a delivery status notification",
			"uuid", env.UUID, "err", err)
	}
}

// enhancedFor derives an RFC 3463 status from an SMTP reply code, when the relay
// did not supply one of its own.
func enhancedFor(code int, temporary bool) string {
	switch {
	case code >= 500:
		return "5.0.0"
	case code >= 400:
		return "4.0.0"
	case temporary:
		return "4.0.0"
	default:
		return "5.0.0"
	}
}

// diagnosticFor reassembles the relay's reply. The code is kept in front so the
// notification carries a real SMTP diagnostic rather than a bare sentence — see
// dsn.Build, which tags it by inspecting exactly that.
func diagnosticFor(code int, msg string) string {
	switch {
	case code > 0 && msg != "":
		return fmt.Sprintf("%d %s", code, msg)
	case code > 0:
		return fmt.Sprintf("%d", code)
	default:
		return msg
	}
}

// deferralReason summarises an attempt the relay answered but that left
// recipients pending, for Envelope.LastErr.
//
// Empty when nothing was deferred: the envelope is finished, and overwriting
// the reason on the way out would replace an explanation with a non-event.
func deferralReason(res *deliver.Result) string {
	var n int
	var msg string
	for _, rr := range res.Rcpts {
		if rr.Outcome != deliver.OutcomeDeferred {
			continue
		}
		n++
		if msg == "" && strings.TrimSpace(rr.Message) != "" {
			msg = strings.TrimSpace(rr.Message)
		}
	}
	if n == 0 {
		return ""
	}
	// The relay's own words when it gave any, because "451 4.7.1 greylisted,
	// try again in 60s" is the whole answer to "why is this still queued?".
	if msg != "" {
		return msg
	}
	return fmt.Sprintf("%d recipient(s) deferred by %s", n, res.Relay.Name)
}

// record applies a delivery result and emits one audit event per resolved
// recipient.
//
// One event per recipient is deliberate: logservice validates rcpt_list and
// rcpt_accepted as a SINGLE email address, so the comma-joined list that
// mailgw/plugins/npLogDelivery.js:37 sends is rejected with a 400 and lost.
func (r *Runner) record(env *Envelope, res *deliver.Result) {
	for _, rr := range res.Rcpts {
		switch rr.Outcome {
		case deliver.OutcomeDelivered:
			r.obs().DeliverOK.Add(1)
			env.SetStatus(rr.Addr, StatusDelivered, rr.Code, rr.Message)
		case deliver.OutcomeRejected:
			r.obs().DeliverBounced.Add(1)
			env.SetStatus(rr.Addr, StatusPermfail, rr.Code, rr.Message)
		default:
			// Deferred: leave pending so the next attempt retries it.
			//
			// Not counted here. DeliverDeferred is per envelope-attempt and
			// lives in deferEnvelope; this branch is per recipient, and it is
			// not even reached when every relay failed at the connection level
			// (record only runs on a result with no Err).
			continue
		}
		r.postDelivery(env, res, rr)
	}
}

func (r *Runner) postDelivery(env *Envelope, res *deliver.Result, rr deliver.RcptResult) {
	if r.cfg.Events == nil || r.cfg.DeliveryURL == "" {
		return
	}

	response := rr.Message
	if response == "" {
		response = res.Response
	}

	// The routing rule is recorded per recipient, so it is looked up by address
	// rather than taken from the envelope.
	var routeRule string
	for _, p := range env.Rcpts {
		if p.Addr == rr.Addr {
			routeRule = p.RouteRule
			break
		}
	}

	r.cfg.Events.Send(events.Envelope{
		Kind: events.KindDelivery,
		URL:  r.cfg.DeliveryURL,
		Body: events.Delivery{
			UUID:         env.UUID,
			DT:           env.QueuedAt,
			Sender:       env.MailFrom,
			RcptDomain:   events.DomainOf(rr.Addr),
			RcptList:     rr.Addr,
			RcptAccepted: rr.Addr,
			TLSForced:    res.TLSForced,
			TLS:          res.TLS,
			Auth:         res.Auth,
			Host:         res.Host,
			IP:           res.IP,
			Port:         res.Port,
			Response:     response,
			Delay:        time.Since(time.UnixMilli(env.QueuedAt)).Seconds(),
			Gateway:      r.cfg.Gateway,
			RouteRule:    routeRule,
		},
	})
}

// deferEnvelope reschedules an envelope after a failed attempt — or gives up on
// it, if it has now outlived max_lifetime.
//
// This is the single home for DeliverDeferred. Its three call sites each end
// the attempt immediately, so exactly one deferral is counted per attempt
// however many relays in the group were tried — and it covers both shapes of
// deferral, "every relay refused to talk to us" (record never ran) and "record
// ran and left recipients pending".
//
// Expiry lives here for the same reason it is the single home for the counter.
// It used to sit at the tail of attempt, AFTER the two early returns for an
// unknown relay group and an unopenable body — so an envelope whose relay group
// had been renamed out of the config was never checked against its lifetime at
// all. It retried forever: never expired, never bounced, never reached dead/.
// Every path that does not complete an envelope passes through here, so this is
// the one place the check cannot be skipped.
//
// Note the two context-cancellation paths call spool.Reschedule directly and
// must keep doing so: requeueing on shutdown is neither a deferral nor a reason
// to give up on mail.
func (r *Runner) deferEnvelope(env *Envelope, inflight, reason string) {
	r.obs().DeliverDeferred.Add(1)
	if reason != "" {
		env.LastErr = reason
	}

	if r.expire(env, inflight) {
		return
	}

	// Before the Reschedule that persists it: DelayWarned only survives because
	// the envelope is rewritten here, so setting it after the write would warn
	// again on every retry for the rest of the message's life.
	r.warnDelayed(env)

	delay := r.jitter(r.cfg.Backoff(env.Attempts))
	if err := r.spool.Reschedule(inflight, env, time.Now().Add(delay)); err != nil {
		r.log.Error("cannot reschedule envelope", "uuid", env.UUID, "err", err)
	}
}

// warnDelayed tells the sender that a message is late but not lost, once.
//
// A delay report is explicitly not a failure: it carries Action: delayed and a
// 4.x status, so a sender's software does not treat a message still being
// retried as dead. The gateway keeps trying either way.
func (r *Runner) warnDelayed(env *Envelope) {
	if r.cfg.Bouncer == nil || r.cfg.DelayWarnAfter <= 0 || env.DelayWarned || Suppress(env) {
		return
	}
	if time.Since(time.UnixMilli(env.QueuedAt)) <= r.cfg.DelayWarnAfter {
		return
	}

	var rcpts []dsn.Rcpt
	for _, rc := range env.Rcpts {
		if !rc.Pending() {
			continue
		}
		if !rc.WantsDelayDSN() {
			r.obs().DSNNotifySuppressed.Add(1)
			continue
		}
		rcpts = append(rcpts, dsn.Rcpt{
			Addr:        rc.Addr,
			OrigAddr:    rc.OrigRcpt,
			OrigType:    rc.OrigRcptType,
			Diagnostic:  diagnosticFor(rc.LastCode, firstNonEmpty(rc.LastMsg, env.LastErr)),
			LastAttempt: time.Now(),
		})
	}
	if len(rcpts) == 0 {
		// Deliberately without setting DelayWarned: a recipient that did want a
		// warning could still be added by a later split, and re-running a slice
		// scan on each attempt costs nothing. Nothing is sent either way.
		return
	}

	var until time.Time
	if r.cfg.MaxLifetime > 0 {
		until = time.UnixMilli(env.QueuedAt).Add(r.cfg.MaxLifetime)
	}

	// Set regardless of the outcome. A warning that could not be generated is
	// not worth retrying every attempt for the next four days.
	env.DelayWarned = true
	r.bounce(env, Request{Env: env, Rcpts: rcpts, Kind: dsn.KindDelayed, RetryUntil: until})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// expire buries an envelope that has outlived max_lifetime, reporting whether it
// did. Pending recipients are marked expired first, so dead/ records who never
// got the message rather than just that it was given up on.
func (r *Runner) expire(env *Envelope, inflight string) bool {
	if r.cfg.MaxLifetime <= 0 {
		return false
	}
	age := time.Since(time.UnixMilli(env.QueuedAt))
	if age <= r.cfg.MaxLifetime {
		return false
	}

	var expired []dsn.Rcpt
	for i := range env.Rcpts {
		rc := &env.Rcpts[i]
		if !rc.Pending() {
			continue
		}
		// The recipient did expire either way. Only the telling is optional,
		// and DSNSent stays false for one that asked not to be told: dead/ is
		// the only surviving record of this message, and "the sender declined"
		// and "we notified them" must not read the same there.
		rc.Status = StatusExpired
		if !rc.WantsFailureDSN() {
			r.obs().DSNNotifySuppressed.Add(1)
			continue
		}
		if !rc.DSNSent {
			expired = append(expired, dsn.Rcpt{
				Addr:     rc.Addr,
				OrigAddr: rc.OrigRcpt,
				OrigType: rc.OrigRcptType,
				// 5.4.7, not 4.4.7. Both are "message expired" in RFC 3463, but
				// the class has to agree with the Action: this report says
				// `failed` and no further attempt will be made, so a 4.x — which
				// tells the sender's software the condition is transient — would
				// contradict the very next line of the same block.
				Status: "5.4.7",
				Diagnostic: diagnosticFor(rc.LastCode,
					firstNonEmpty(rc.LastMsg, env.LastErr, "message expired in the outbound queue")),
				LastAttempt: time.Now(),
			})
			rc.DSNSent = true
		}
	}

	r.obs().EnvelopesExpired.Add(1)
	r.log.Error("envelope expired", "uuid", env.UUID, "age", age, "attempts", env.Attempts)

	// Before Bury, which collects the body this notification quotes — dead/ is
	// deliberately metadata-only, so once buried there is nothing left to quote.
	if len(expired) > 0 {
		if r.cfg.Bouncer != nil && Suppress(env) {
			r.obs().DSNSuppressed.Add(1)
		} else {
			r.bounce(env, Request{Env: env, Rcpts: expired, Kind: dsn.KindFailed})
		}
	}

	if err := r.spool.Bury(inflight, env); err != nil {
		r.log.Error("cannot bury expired envelope", "uuid", env.UUID, "err", err)
	}
	return true
}

// jitter spreads retries so a batch deferred together does not return in
// lockstep.
func (r *Runner) jitter(d time.Duration) time.Duration {
	if r.cfg.Jitter <= 0 {
		return d
	}
	spread := float64(d) * r.cfg.Jitter
	// rand.Float64() in [0,1) mapped to [-spread, +spread).
	return time.Duration(float64(d) + (rand.Float64()*2-1)*spread)
}
