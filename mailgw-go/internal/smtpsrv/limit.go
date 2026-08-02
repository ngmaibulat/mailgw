package smtpsrv

import (
	"log/slog"
	"net"
	"sync"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

// throttleResponse is written to a peer arriving over max.connections.
//
// 421 and not 550: the peer did nothing wrong and the condition is temporary,
// so the code has to be the one that says "come back later". A 5xx here would
// make a sending MTA give up on mail that would have been accepted a second
// later.
const throttleResponse = "421 4.7.0 Too many connections, try again later\r\n"

// The connection cap is TWO listeners, and the split is the whole design.
//
//	tcp -> proxyproto -> Meter -> tls.NewListener -> Guard -> Throttle -> Limit -> srv.Serve
//
// Limit — the admission decision — goes OUTSIDE Guard. Inside it, a peer the
// allowlist is about to refuse would hold a slot for the moment before Guard
// closed it, so a flood from unlisted addresses could fill the semaphore and get
// legitimate senders throttled: the cap would become the attack. Outside, a slot
// is only ever spent on a peer that already passed. It also stays outside
// tls.NewListener so its 421, like Guard's 550, is written inside the TLS
// session and can be read.
//
// M15's Throttle sits between the two, on the OTHER side of Guard, and the
// contrast is the point: this cap bounds one process-wide semaphore, so an
// unlisted peer must never hold a slot; that limiter bounds a MAP, so an
// unlisted peer must never occupy a key. The resource decides the side. See
// throttle.go.
//
// Meter — the accounting — goes UNDERNEATH tls.NewListener, because the slot has
// to be released when the connection closes and the only way to see that is to
// wrap Close. go-smtp identifies a TLS session by a direct type assertion with
// no unwrap interface (conn.go:189, server.go:164), so a wrapper ABOVE
// tls.NewListener hides the *tls.Conn: an implicit-TLS session would then report
// no TLS to policy rules, the Received header and the audit row, be offered
// STARTTLS inside TLS, and — worst — lose the pre-handshake read deadline that
// server.go:164 is the only place to arm, so a peer that connects and says
// nothing would hold its slot forever. Wrapping below TLS keeps the assertion
// intact, and tls.Conn.Close closes what it wraps, so the release still happens.
//
// The semaphore is supplied by the caller rather than sized here, because
// max.connections is a process-wide ceiling on file descriptors — every listener
// shares one.

// Meter wraps accepted connections so a slot can be released when one closes.
//
// It takes no semaphore: a metered connection is unarmed until Limit admits it,
// so the two halves can sit on opposite sides of tls.NewListener.
func Meter(inner net.Listener) net.Listener {
	return &meterListener{Listener: inner}
}

type meterListener struct{ net.Listener }

func (l *meterListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &slotConn{Conn: conn}, nil
}

// slotConn returns its slot when the session ends.
//
// sync.Once because Close is called more than once on a normal path: go-smtp
// closes the connection when the session ends, and Server.Close closes every
// live one again at shutdown. Releasing twice would let the cap drift upward
// until it stopped bounding anything.
type slotConn struct {
	net.Conn
	once sync.Once
	mu   sync.Mutex
	// release is nil until Limit admits this connection. A metered connection
	// that is never armed — refused by the cap, or denied by the allowlist —
	// therefore releases nothing when it closes.
	release func()
}

func (c *slotConn) arm(release func()) {
	c.mu.Lock()
	c.release = release
	c.mu.Unlock()
}

func (c *slotConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.mu.Lock()
		release := c.release
		c.mu.Unlock()
		if release != nil {
			release()
		}
	})
	return err
}

// slotOf finds the metered connection underneath conn.
//
// One hop of unwrapping is all the production chain needs (tls.Conn over
// slotConn), but the loop is bounded rather than fixed so an added layer that
// forwards NetConn does not silently disable the cap.
func slotOf(conn net.Conn) *slotConn {
	for range 4 {
		if sc, ok := conn.(*slotConn); ok {
			return sc
		}
		under, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			return nil
		}
		if conn = under.NetConn(); conn == nil {
			return nil
		}
	}
	return nil
}

// limitListener caps concurrent connections handed on to the SMTP server.
type limitListener struct {
	net.Listener
	sem chan struct{}
	log *slog.Logger
	// metrics may be nil; see obs().
	metrics *obs.Metrics
	// warns keeps a refusal flood from becoming a log flood. conn_throttled is
	// the exact signal; the line is there to say which peers.
	warns throttledLogger
	// unmetered fires at most once, when Accept is handed a connection Meter
	// never saw. It means the listener chain has been rewired.
	unmetered sync.Once
	// onRefuse is a test hook, invoked after a peer has been throttled and
	// closed. Mirrors allowlistListener.onDeny and proxyproto's onDrop.
	onRefuse func(addr string)
}

func (l *limitListener) obs() *obs.Metrics {
	if l.metrics != nil {
		return l.metrics
	}
	return obs.Discard
}

// Limit admits at most len(sem) connections at once. Pair it with Meter, which
// must be installed below any TLS listener; see the comment above.
//
// A nil or zero-capacity semaphore returns inner unchanged: configuration
// validates max.connections as positive, and a wrapper that refused everything
// would be far worse than one that is absent.
func Limit(inner net.Listener, sem chan struct{}, log *slog.Logger, m *obs.Metrics) net.Listener {
	if sem == nil || cap(sem) == 0 {
		return inner
	}
	if log == nil {
		log = slog.Default()
	}
	return &limitListener{Listener: inner, sem: sem, log: log, metrics: m}
}

// Accept blocks until a connection is available and the cap allows it through.
//
// The select is NON-blocking on purpose. golang.org/x/net/netutil's
// LimitListener waits for a slot, which leaves the peer holding an open socket
// with no reply — indistinguishable from a hung gateway. Answering 421 and
// closing tells the sender exactly what happened and costs one round trip.
//
// Like Guard, a refusal is not an accept error: returning one would tear down
// the whole server.
func (l *limitListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		select {
		case l.sem <- struct{}{}:
			if sc := slotOf(conn); sc != nil {
				sc.arm(l.release)
				// Returned UNWRAPPED, so nothing hides a *tls.Conn from
				// go-smtp.
				return conn, nil
			}
			// No Meter underneath. Wrapping here is what this listener used to
			// do always; it keeps the cap honest at the cost of the TLS
			// assertion, which is the right way round for a chain that should
			// not be reachable.
			l.unmetered.Do(func() {
				l.log.Error("connection cap is running without Meter; " +
					"inbound TLS state will be invisible to the server")
			})
			return &limitConn{Conn: conn, release: l.release}, nil
		default:
			remote := conn.RemoteAddr().String()
			// Counted before the refusal is written, so a peer that hangs up
			// without reading its 421 is still counted.
			l.obs().ConnThrottled.Add(1)
			l.warns.log(func(suppressed int64) {
				l.log.Warn("connection refused: max.connections reached",
					"remote", remote, "limit", cap(l.sem), "suppressed", suppressed)
			})
			// Off the accept loop: on a TLS listener this write runs the
			// handshake, and one silent peer must not stop the gateway
			// accepting anyone else.
			refusals.do(conn, throttleResponse, l.log, func() {
				if l.onRefuse != nil {
					l.onRefuse(remote)
				}
			})
		}
	}
}

// release returns a slot. Guarded against an empty channel so a double release
// can never block the accept loop, however a future caller misuses it.
func (l *limitListener) release() {
	select {
	case <-l.sem:
	default:
	}
}

// limitConn is the fallback for a chain with no Meter in it; see Accept.
type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
