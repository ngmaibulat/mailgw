// Package smtpsrv implements the inbound SMTP server.
package smtpsrv

import (
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
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

// AllowlistFunc returns the allowlist in force right now. It is a function
// rather than a value so that a SIGHUP reload is picked up by already-running
// listeners without restarting them.
type AllowlistFunc func() *config.Allowlist

// allowlistListener filters connections before the SMTP server ever sees them.
type allowlistListener struct {
	net.Listener
	allowed AllowlistFunc
	log     *slog.Logger
	// onDeny is a test hook, invoked after a peer has been rejected and closed.
	onDeny func(addr string)
}

// NewAllowlistListener wraps inner so that Accept only ever yields connections
// from permitted peers.
func NewAllowlistListener(inner net.Listener, allowed AllowlistFunc, log *slog.Logger) net.Listener {
	if log == nil {
		log = slog.Default()
	}
	return &allowlistListener{Listener: inner, allowed: allowed, log: log}
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
			l.log.Debug("connection allowed", "remote", remote)
			return conn, nil
		}

		l.log.Warn("connection denied by allowlist", "remote", remote, "allowlist", list.String())
		l.deny(conn)
		if l.onDeny != nil {
			l.onDeny(remote)
		}
	}
}

// deny writes the rejection and closes the connection, giving up quickly if the
// peer will not read it.
func (l *allowlistListener) deny(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(denyWriteTimeout))
	if _, err := conn.Write([]byte(denyResponse)); err != nil {
		l.log.Debug("could not write denial", "remote", conn.RemoteAddr().String(), "err", err)
	}
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
