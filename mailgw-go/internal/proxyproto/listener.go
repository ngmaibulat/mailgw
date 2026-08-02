package proxyproto

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

const (
	// headerTimeout bounds the header read. It applies only to a peer already
	// inside proxy_trusted — an untrusted one is closed before any read — and a
	// balancer writes its header immediately on connect, so this is orders of
	// magnitude more than a healthy path needs.
	headerTimeout = 5 * time.Second

	// maxParallel is how many header reads may be outstanding at once. Past it
	// the internal accept loop blocks and the kernel's accept queue absorbs the
	// rest, which is the correct backpressure.
	maxParallel = 128

	// hdrBufSize covers v1's 107-byte maximum with room for whatever the peer
	// pipelined behind it.
	hdrBufSize = 256
)

// listener resolves the PROXY header of every accepted connection before
// handing it on.
//
// The header has to be resolved BEFORE the allowlist sees the connection:
// smtpsrv's Guard reads RemoteAddr() synchronously inside its own accept loop,
// so a header parsed lazily on first Read would be parsed after the allow/deny
// decision — leaving the allowlist looking at the balancer, which is the bug
// this package exists to fix. Parsing it inline in Accept would fix that but let
// one silent peer stall every other connection for the whole deadline.
//
// So: an internal goroutine accepts, a bounded pool of workers resolves headers
// concurrently, and Accept drains already-resolved connections.
type listener struct {
	net.Listener
	trusted []netip.Prefix
	log     *slog.Logger
	metrics *obs.Metrics

	// ready is UNBUFFERED on purpose: a resolved connection can then never sit
	// in it unowned, so shutdown cannot leak one. A worker is always parked on
	// the send and takes the done arm instead.
	ready chan net.Conn
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
	// fail is sticky, so every Accept after the inner listener dies returns the
	// same error rather than blocking forever.
	fail atomic.Pointer[error]

	// Test hooks, in the shape internal/smtpsrv/listener_test.go already uses.
	timeout time.Duration
	onDrop  func(remote string, err error)
}

// NewListener wraps inner so Accept yields only connections whose PROXY header
// has already been read and whose RemoteAddr is the forwarded client.
//
// trusted is a fixed list rather than a reload hook like smtpsrv.AllowlistFunc:
// proxy_trusted lives inside server.listen[], and `listen` is on
// restartRequired, so a change to it restarts this process.
func NewListener(inner net.Listener, trusted []netip.Prefix, log *slog.Logger, m *obs.Metrics) net.Listener {
	if log == nil {
		log = slog.Default()
	}
	l := &listener{
		Listener: inner,
		trusted:  trusted,
		log:      log,
		metrics:  m,
		ready:    make(chan net.Conn),
		slots:    make(chan struct{}, maxParallel),
		done:     make(chan struct{}),
	}
	go l.loop()
	return l
}

func (l *listener) obs() *obs.Metrics {
	if l.metrics != nil {
		return l.metrics
	}
	return obs.Discard
}

func (l *listener) headerTimeout() time.Duration {
	if l.timeout > 0 {
		return l.timeout
	}
	return headerTimeout
}

func (l *listener) loop() {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			l.fail.Store(&err)
			l.stop()
			return
		}
		select {
		case l.slots <- struct{}{}:
		case <-l.done:
			_ = raw.Close()
			return
		}
		go l.resolve(raw)
	}
}

func (l *listener) resolve(raw net.Conn) {
	defer func() { <-l.slots }()

	c, err := l.wrap(raw)
	if err != nil {
		l.drop(raw, err)
		return
	}
	select {
	case l.ready <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *listener) Accept() (net.Conn, error) {
	if p := l.fail.Load(); p != nil {
		return nil, *p
	}
	select {
	case c := <-l.ready:
		return c, nil
	case <-l.done:
		if p := l.fail.Load(); p != nil {
			return nil, *p
		}
		return nil, net.ErrClosed
	}
}

// Close stops the resolver and closes the underlying listener. Delegating
// downward is what keeps smtpListeners.closeAll reaching the socket through the
// whole wrapper chain.
func (l *listener) Close() error {
	l.stop()
	return l.Listener.Close()
}

func (l *listener) stop() { l.once.Do(func() { close(l.done) }) }

// wrap resolves one connection.
func (l *listener) wrap(raw net.Conn) (net.Conn, error) {
	// Trust FIRST, before any read. An untrusted peer must cost one accept and
	// one close — never a parser slot, never a deadline — because a PROXY header
	// is trivially forged and this list is the only thing between a forged one
	// and an open relay.
	if !trusts(l.trusted, addrOf(raw.RemoteAddr())) {
		return nil, ErrUntrusted
	}

	if err := raw.SetReadDeadline(time.Now().Add(l.headerTimeout())); err != nil {
		return nil, err
	}

	br := bufio.NewReaderSize(raw, hdrBufSize)
	h, err := Parse(br)
	if err != nil {
		return nil, err
	}

	// Clear our deadline before handing the connection on. go-smtp arms its own
	// per-command deadline from ReadTimeout, but only when that is non-zero; a
	// stale deadline left here would kill the session five seconds in.
	if err := raw.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}

	c := &conn{Conn: raw, remote: raw.RemoteAddr(), local: raw.LocalAddr()}
	// Whatever the reader pulled in past the header. A balancer that writes the
	// header and the TLS ClientHello in one segment leaves the ClientHello here,
	// and the tls.Conn above must find it.
	if n := br.Buffered(); n > 0 {
		rest := make([]byte, n)
		if _, err := io.ReadFull(br, rest); err != nil {
			return nil, err
		}
		c.rest = rest
	}

	// A v2 LOCAL command, a v1 UNKNOWN, or an address family that is not TCP
	// all mean "no proxied client", and the real peer address stands.
	if h.Proxied() {
		c.remote = net.TCPAddrFromAddrPort(h.Src)
		if h.Dst.IsValid() {
			c.local = net.TCPAddrFromAddrPort(h.Dst)
		}
	}
	return c, nil
}

func (l *listener) drop(raw net.Conn, err error) {
	remote := raw.RemoteAddr().String()
	// Closed silently: the PROXY protocol has no error response, and answering
	// an untrusted peer at all only tells it what is listening.
	_ = raw.Close()
	l.obs().ProxyDropped.Add(1)
	l.log.Warn("dropped connection without a usable PROXY header",
		"remote", remote, "err", err)
	if l.onDrop != nil {
		l.onDrop(remote, err)
	}
}

// conn reports the forwarded address and serves whatever the header reader
// over-read before the socket.
type conn struct {
	net.Conn
	remote net.Addr
	local  net.Addr
	rest   []byte
}

// RemoteAddr returns a *net.TCPAddr, and that concrete type matters:
// smtpsrv/session.go type-asserts it with no fallback, so any other
// implementation would silently leave every conn.* rule, log line and audit row
// with a zero address.
func (c *conn) RemoteAddr() net.Addr { return c.remote }

// LocalAddr reports the address the client connected to on the proxy — the VIP,
// which is what a conn.local_port rule author means. Reporting the gateway's own
// private address behind a VIP would be the surprise.
func (c *conn) LocalAddr() net.Addr { return c.local }

func (c *conn) Read(p []byte) (int, error) {
	if len(c.rest) > 0 {
		n := copy(p, c.rest)
		c.rest = c.rest[n:]
		if len(c.rest) == 0 {
			// Release the header buffer for the life of the session rather than
			// retaining it per connection.
			c.rest = nil
		}
		return n, nil
	}
	return c.Conn.Read(p)
}

// trusts reports whether a is inside any trusted prefix.
func trusts(prefixes []netip.Prefix, a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(a) })
}

// addrOf extracts the IP from a net.Addr.
//
// A near-copy of smtpsrv.addrOf, which is unexported there. Duplicated
// deliberately: exporting it from smtpsrv and importing that package here would
// point the lower layer at the upper one.
func addrOf(a net.Addr) netip.Addr {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.AddrPort().Addr().Unmap()
	}
	s := a.String()
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}
