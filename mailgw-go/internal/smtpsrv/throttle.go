package smtpsrv

import (
	"log/slog"
	"net"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ratelimit"
)

// rateResponse is written to a peer connecting faster than ratelimit.connect_per_ip.
//
// 421, on exactly the reasoning throttleResponse gives for the concurrency cap:
// the condition is temporary and the peer will be welcome again shortly, so the
// code has to be the one that says "come back later". Nothing in M15 may answer
// 5xx — turning a limit an operator set too low into permanent rejection is the
// failure mode the whole milestone is designed against.
const rateResponse = "421 4.7.0 Too many connections from your address, try again later\r\n"

// LimiterFunc returns the limiter in force right now.
//
// A function for the same reason AllowlistFunc is one: rate limits are read live
// so an operator can retune them during an incident without restarting a mail
// server, and a listener that captured the limiter at bring-up would pin them.
type LimiterFunc func() *ratelimit.Limiter

// throttleListener refuses a peer that is connecting too often.
//
//	tcp -> proxyproto -> Meter -> tls -> Guard -> Throttle -> Limit -> srv.Serve
//
// # Why this sits INSIDE Guard, when the concurrency cap sits outside it
//
// That looks inconsistent and is not. The two bound different resources, and the
// resource decides the side.
//
// The cap (Limit) bounds a PROCESS-WIDE semaphore. A peer the allowlist is about
// to refuse would hold a slot for the moment before Guard closed it, so a flood
// from unlisted addresses could fill the semaphore and throttle legitimate
// senders — the cap would become the attack. It has to be outside.
//
// This limiter bounds nothing shared. Every key gets its own bucket, so an
// unlisted peer cannot spend anybody else's allowance and there is no
// starvation to design against. What it does cost is a MAP ENTRY per distinct
// address — and outside Guard that map is keyed by the internet rather than by
// the allowlist, which makes the limiter itself the memory-exhaustion vector it
// was added to prevent. So it goes inside, where every key is a peer that was
// already allowed to connect.
//
// It sits before Limit rather than after so that a peer over its rate never
// spends a semaphore slot on its way to being refused.
type throttleListener struct {
	net.Listener
	limiter LimiterFunc
	log     *slog.Logger
	// metrics may be nil; see obs().
	metrics *obs.Metrics
	// warns keeps a refusal flood from becoming a log flood. The counter stays
	// exact either way.
	warns throttledLogger
	// onRefuse is a test hook, invoked after a peer has been refused and closed.
	// Mirrors allowlistListener.onDeny and limitListener.onRefuse.
	onRefuse func(addr string)
}

func (l *throttleListener) obs() *obs.Metrics {
	if l.metrics != nil {
		return l.metrics
	}
	return obs.Discard
}

// Throttle wraps inner so that Accept refuses peers over their connection rate.
//
// A nil limiter func returns inner unchanged, so a gateway with no rate limits
// configured has no extra listener in its chain at all — not merely one that
// always says yes.
func Throttle(inner net.Listener, limiter LimiterFunc, log *slog.Logger, m *obs.Metrics) net.Listener {
	if limiter == nil {
		return inner
	}
	if log == nil {
		log = slog.Default()
	}
	return &throttleListener{Listener: inner, limiter: limiter, log: log, metrics: m}
}

// Accept blocks until a peer within its rate connects.
//
// Like Guard and Limit, a refusal is not an accept error: returning one would
// tear down the whole server. The connection is returned UNWRAPPED, so nothing
// here can hide a *tls.Conn from go-smtp — the defect M16 found in the
// connection cap, and the reason this listener wraps nothing at all.
func (l *throttleListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		// netip rather than the string form: two spellings of one IPv6 address
		// would otherwise get two buckets, and ::ffff: normalisation would give
		// a dual-stack peer a second allowance.
		ip := addrOf(conn.RemoteAddr())
		if !ip.IsValid() {
			// Guard already refuses an address it cannot parse, so this is
			// unreachable in the production chain. Allow rather than refuse: an
			// unkeyable connection is not evidence of abuse.
			return conn, nil
		}

		if l.limiter().Allow(ratelimit.ConnPerIP, ip.Unmap().String()) {
			return conn, nil
		}

		remote := conn.RemoteAddr().String()
		// Counted before the refusal is written, so a peer that hangs up without
		// reading its 421 is still counted.
		l.obs().RateConn.Add(1)
		l.warns.log(func(suppressed int64) {
			l.log.Warn("connection refused: ratelimit.connect_per_ip",
				"remote", remote, "suppressed", suppressed)
		})
		// Off the accept loop, for the reason refuser exists: on a TLS listener
		// this write runs the handshake, and one silent peer must not stop the
		// gateway accepting anybody else.
		refusals.do(conn, rateResponse, l.log, func() {
			if l.onRefuse != nil {
				l.onRefuse(remote)
			}
		})
	}
}
