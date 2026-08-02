// Package smtpsrv implements the inbound SMTP server.
package smtpsrv

import (
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

// denyResponse is written to a peer that fails the allowlist check.
//
// Haraka answers hook_connect with DENYDISCONNECT (mailgw/plugins/npFilter.js:69),
// which rejects before the greeting. go-smtp calls Backend.NewSession at EHLO —
// too late — so the check lives here, in front of the server, and this listener
// speaks the rejection itself.
const denyResponse = "550 5.7.1 Access denied\r\n"

// denyWriteTimeout bounds how long we will wait to push the rejection to a peer
// that is not reading, so a slow client cannot pin an accept loop goroutine.
const denyWriteTimeout = 5 * time.Second

// maxRefusalsInFlight bounds how many turned-away peers may be having their
// reply written at once, process-wide. Past it the reply is dropped and the
// socket closed: a peer that gets no answer retries, where an accept loop that
// stalls serves nobody.
const maxRefusalsInFlight = 128

// refusalLogInterval is how often a listener will say out loud that it is
// turning peers away. The counters are exact; the log line only has to name
// enough peers to act on.
const refusalLogInterval = 10 * time.Second

// refusals writes rejections off the accept loop; see refuser.
var refusals = newRefuser(maxRefusalsInFlight)

// refuser answers peers that are being turned away, on its own goroutines.
//
// Writing inline in Accept is what both listeners used to do, and on a TLS
// listener it is unbounded: the first Write runs the handshake, whose
// ClientHello read is not covered by a write deadline, so one silent peer stops
// that listener accepting anybody. Off the loop and behind a full SetDeadline,
// the worst a peer can cost is one of these slots for denyWriteTimeout.
type refuser struct{ slots chan struct{} }

func newRefuser(n int) *refuser { return &refuser{slots: make(chan struct{}, n)} }

// do writes reply to conn and closes it, then calls done — in that order, so a
// caller that synchronises on done sees a socket that is already gone.
func (r *refuser) do(conn net.Conn, reply string, log *slog.Logger, done func()) {
	select {
	case r.slots <- struct{}{}:
	default:
		_ = conn.Close()
		if done != nil {
			done()
		}
		return
	}

	go func() {
		defer func() { <-r.slots }()
		// SetDeadline, not SetWriteDeadline: on an implicit-TLS listener this
		// Write handshakes first, and that is a read.
		_ = conn.SetDeadline(time.Now().Add(denyWriteTimeout))
		if _, err := conn.Write([]byte(reply)); err != nil && log != nil {
			log.Debug("could not write refusal",
				"remote", conn.RemoteAddr().String(), "err", err)
		}
		_ = conn.Close()
		if done != nil {
			done()
		}
	}()
}

// throttledLogger emits at most one line per interval and reports how many it
// swallowed, so a flood is visible without becoming the flood.
type throttledLogger struct {
	mu       sync.Mutex
	last     time.Time
	skipped  int64
	interval time.Duration
}

// log calls emit with the number of lines suppressed since the last one, or
// counts this call and returns.
func (t *throttledLogger) log(emit func(suppressed int64)) {
	t.mu.Lock()
	interval := t.interval
	if interval <= 0 {
		interval = refusalLogInterval
	}
	now := time.Now()
	if !t.last.IsZero() && now.Sub(t.last) < interval {
		t.skipped++
		t.mu.Unlock()
		return
	}
	skipped := t.skipped
	t.skipped = 0
	t.last = now
	t.mu.Unlock()

	emit(skipped)
}

// AllowlistFunc returns the allowlist in force right now. It is a function
// rather than a value so that a SIGHUP reload is picked up by already-running
// listeners without restarting them.
type AllowlistFunc func() *config.Allowlist

// allowlistListener filters connections before the SMTP server ever sees them.
type allowlistListener struct {
	net.Listener
	allowed AllowlistFunc
	log     *slog.Logger
	// metrics may be nil; see obs(). This is the only place in the process
	// that sees a connection before the banner, so it owns both connection
	// counters.
	metrics *obs.Metrics
	// warns keeps a denial flood from becoming a log flood; conn_denied stays
	// exact either way.
	warns throttledLogger
	// onDeny is a test hook, invoked after a peer has been rejected and closed.
	onDeny func(addr string)
}

// obs returns the counter registry, or a shared discard when none was supplied.
func (l *allowlistListener) obs() *obs.Metrics {
	if l.metrics != nil {
		return l.metrics
	}
	return obs.Discard
}

// NewAllowlistListener wraps inner so that Accept only ever yields connections
// from permitted peers.
func NewAllowlistListener(inner net.Listener, allowed AllowlistFunc, log *slog.Logger, m *obs.Metrics) net.Listener {
	if log == nil {
		log = slog.Default()
	}
	return &allowlistListener{Listener: inner, allowed: allowed, log: log, metrics: m}
}

// Accept blocks until a permitted peer connects. Rejected peers are answered
// and closed inline, and the loop continues — a denial is not an accept error,
// because returning one would tear down the whole server.
func (l *allowlistListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		remote := conn.RemoteAddr().String()
		list := l.allowed()
		if list.Allowed(addrOf(conn.RemoteAddr())) {
			l.obs().ConnAccepted.Add(1)
			l.log.Debug("connection allowed", "remote", remote)
			return conn, nil
		}

		// Counted before the rejection is written, so a peer that hangs up
		// without reading its 550 is still counted as denied.
		l.obs().ConnDenied.Add(1)
		l.warns.log(func(suppressed int64) {
			l.log.Warn("connection denied by allowlist", "remote", remote,
				"allowlist", list.String(), "suppressed", suppressed)
		})
		l.deny(conn)
	}
}

// deny writes the rejection and closes the connection off the accept loop,
// giving up quickly if the peer will not read it.
func (l *allowlistListener) deny(conn net.Conn) {
	remote := conn.RemoteAddr().String()
	refusals.do(conn, denyResponse, l.log, func() {
		if l.onDeny != nil {
			l.onDeny(remote)
		}
	})
}

// addrOf extracts a netip.Addr from a net.Addr. An address it cannot parse
// yields the zero Addr, which Allowlist.Allowed rejects — the safe direction.
func addrOf(a net.Addr) netip.Addr {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.AddrPort().Addr()
	}

	s := a.String()
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}
