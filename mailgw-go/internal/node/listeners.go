package node

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/proxyproto"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/smtpsrv"
)

var errNoTLSConfig = errors.New("implicit_tls listener without tls.cert and tls.key")

// smtpListeners owns the inbound sockets.
//
// Extracted from the boot path so that M5 can start them from the config-pull
// loop, after the first bundle applies, instead of at process start — a managed
// gateway has no configuration to serve mail with until then. sync.Once is what
// makes those two call sites safe; today there is exactly one caller and it
// never contends.
type smtpListeners struct {
	once sync.Once
	lns  []net.Listener
	err  error
}

// start opens and serves every configured listener. It is safe to call more
// than once: only the first call does anything.
func (l *smtpListeners) start(
	ctx context.Context,
	srv *smtp.Server,
	cfg *config.Config,
	allowlist func() *config.Allowlist,
	limiter smtpsrv.LimiterFunc,
	metrics *obs.Metrics,
	log *slog.Logger,
) error {
	l.once.Do(func() {
		// One semaphore for every listener, because max.connections is a
		// ceiling on this PROCESS's file descriptors and spool temp files, not
		// a property of any one socket. It sits under `max:`, which is
		// server-wide, unlike the per-listener proxy_protocol keys.
		sem := make(chan struct{}, cfg.Server.Max.Connections)

		for _, lst := range cfg.Server.Listen {
			ln, err := net.Listen("tcp", lst.Addr)
			if err != nil {
				log.Error("cannot listen", "addr", lst.Addr, "err", err)
				l.err = err
				// Close whatever already came up, so a partial failure does
				// not leave half the gateway serving.
				l.closeAll()
				return
			}

			// The PROXY header precedes the TLS handshake on the wire, so this
			// goes on the RAW listener — inside tls.NewListener, and inside
			// Guard, which must see the address the header carries rather than
			// the balancer's. tls.Conn.RemoteAddr delegates straight down to the
			// conn it wraps, so the forwarded address reaches Guard and the
			// session with no further change: the same delegation the comment
			// below already relies on for the opposite reason.
			//
			//     tcp -> proxyproto -> Meter -> tls -> Guard -> Throttle -> Limit -> srv.Serve
			if lst.ProxyProtocol {
				trusted, perr := config.ParsePrefixes(lst.ProxyTrusted)
				if perr != nil {
					// Unreachable through validation, which parses the same list
					// — but a listener that honoured a forged header because a
					// check moved is not a failure to find in the field.
					log.Error("invalid proxy_trusted", "addr", lst.Addr, "err", perr)
					l.err = perr
					l.closeAll()
					_ = ln.Close()
					return
				}
				ln = proxyproto.NewListener(ln, trusted, log, metrics)
			}

			// The cap's accounting half, UNDERNEATH the TLS listener. Above it,
			// the wrapper would hide the *tls.Conn from go-smtp, which finds TLS
			// by a bare type assertion: an implicit-TLS session would report no
			// TLS to the rules, the Received header and the audit row, be
			// offered STARTTLS inside TLS, and lose the only pre-handshake read
			// deadline there is — so a silent peer would hold its slot for
			// ever. Admission still happens outside Guard, below.
			ln = smtpsrv.Meter(ln)

			if lst.ImplicitTLS {
				if srv.TLSConfig == nil {
					// Unreachable through validation, which requires a keypair
					// for implicit_tls — but serving cleartext on 465 because a
					// check moved is not a failure to discover in the field.
					log.Error("implicit_tls listener has no TLS configuration", "addr", lst.Addr)
					l.err = errNoTLSConfig
					l.closeAll()
					_ = ln.Close()
					return
				}
				ln = tls.NewListener(ln, srv.TLSConfig)
			}

			// Guard goes OUTSIDE the TLS listener, not inside. tls.NewListener's
			// Accept does not handshake, so the allowlist's "550 Access denied"
			// is written through the TLS conn and the peer can read it; wrapping
			// the other way round would put a cleartext 550 on a socket that is
			// expecting a handshake. RemoteAddr still delegates to the underlying
			// TCP conn, so the allowlist sees the real address either way.
			guarded := smtpsrv.Guard(ln, allowlist, log, metrics)

			// The per-IP connection RATE limiter goes INSIDE Guard, which is the
			// opposite side from the cap below — and the resource is what decides
			// the side. This one bounds nothing shared: every key has its own
			// bucket, so an unlisted peer cannot spend anyone else's allowance
			// and there is no starvation to design against. What it does cost is
			// a map entry per distinct address, and outside Guard that map is
			// keyed by the internet rather than by the allowlist — which would
			// make the limiter the memory-exhaustion vector it exists to prevent.
			// Inside, every key is a peer that was already allowed to connect.
			// It wraps nothing, so no *tls.Conn is hidden from go-smtp.
			throttled := smtpsrv.Throttle(guarded, limiter, log, metrics)

			// The cap's admission half goes OUTSIDE Guard: inside, a peer the
			// allowlist is about to refuse would hold a slot until Guard closed
			// it, so a flood from unlisted addresses could fill the semaphore
			// and throttle legitimate senders — the cap would become the attack.
			// Outside, a slot is only ever spent on a peer that already passed.
			// It stays inside nothing else, so its 421 is written through the
			// same TLS conn Guard's 550 is. It arms the Meter installed above
			// and hands the connection on unwrapped.
			//
			// Last, so a peer already over its rate is refused without ever
			// spending a semaphore slot on the way.
			capped := smtpsrv.Limit(throttled, sem, log, metrics)
			l.lns = append(l.lns, capped)

			go func(addr string, implicit bool, ln net.Listener) {
				log.Info("listening", "addr", addr, "implicit_tls", implicit)
				if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
					log.Error("serve failed", "addr", addr, "err", err)
				}
			}(lst.Addr, lst.ImplicitTLS, capped)
		}
	})
	return l.err
}

// closeAll stops accepting new connections. In-flight sessions are drained by
// the server's own Shutdown.
// addrs is what the listeners actually bound, which differs from what the
// configuration asked for whenever it asked for port 0.
//
// Read without a lock because start() runs under a sync.Once and appends to lns
// before anything can call this: the admin listener is answering by then, but a
// gateway that has not applied a configuration has no listeners at all and gets
// an empty slice, which is the truthful answer.
func (l *smtpListeners) addrs() []string {
	out := make([]string, 0, len(l.lns))
	for _, ln := range l.lns {
		out = append(out, ln.Addr().String())
	}
	return out
}

func (l *smtpListeners) closeAll() {
	for _, ln := range l.lns {
		_ = ln.Close()
	}
	l.lns = nil
}
