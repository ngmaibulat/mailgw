package proxyproto

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func prefixes(t *testing.T, entries ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", e, err)
		}
		out = append(out, p)
	}
	return out
}

type rig struct {
	addr    string
	accepts chan net.Conn
	drops   chan error
	close   func()
}

// start brings up a proxyproto listener over a real socket, constructing the
// unexported type directly so the tests can use the onDrop hook and a short
// deadline — the same shape internal/smtpsrv/listener_test.go uses for onDeny.
func start(t *testing.T, trusted []netip.Prefix, timeout time.Duration) *rig {
	t.Helper()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	r := &rig{
		addr:    inner.Addr().String(),
		accepts: make(chan net.Conn, 8),
		drops:   make(chan error, 8),
	}

	l := &listener{
		Listener: inner,
		trusted:  trusted,
		log:      quiet(),
		ready:    make(chan net.Conn),
		slots:    make(chan struct{}, maxParallel),
		done:     make(chan struct{}),
		timeout:  timeout,
	}
	l.onDrop = func(_ string, err error) { r.drops <- err }
	go l.loop()

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			r.accepts <- c
		}
	}()

	r.close = func() { _ = l.Close() }
	t.Cleanup(r.close)
	return r
}

func (r *rig) accepted(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-r.accepts:
		return c
	case err := <-r.drops:
		t.Fatalf("connection was dropped: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no connection was accepted")
	}
	return nil
}

func (r *rig) dropped(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.drops:
		return err
	case c := <-r.accepts:
		t.Fatalf("connection was accepted; RemoteAddr=%v", c.RemoteAddr())
	case <-time.After(3 * time.Second):
		t.Fatal("connection was neither accepted nor dropped")
	}
	return nil
}

func dial(t *testing.T, addr string, write string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if write != "" {
		if _, err := c.Write([]byte(write)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return c
}

func TestListener_V1Accepted(t *testing.T) {
	r := start(t, prefixes(t, "127.0.0.0/8"), 0)
	dial(t, r.addr, "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\r\n")

	c := r.accepted(t)

	// The concrete type matters: smtpsrv/session.go type-asserts *net.TCPAddr
	// with no fallback, so anything else silently zeroes the address everywhere.
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("RemoteAddr is %T, want *net.TCPAddr", c.RemoteAddr())
	}
	if got := ta.String(); got != "203.0.113.7:5000" {
		t.Errorf("RemoteAddr = %s, want the forwarded client", got)
	}
	if got := c.LocalAddr().String(); got != "198.51.100.1:25" {
		t.Errorf("LocalAddr = %s, want the address the client connected to", got)
	}
}

func TestListener_V2Accepted(t *testing.T) {
	r := start(t, prefixes(t, "127.0.0.0/8"), 0)
	hdr := v2(0x21, 0x11, v2TCP4([4]byte{203, 0, 113, 7}, [4]byte{198, 51, 100, 1}, 5000, 25))
	dial(t, r.addr, string(hdr))

	c := r.accepted(t)
	if got := c.RemoteAddr().String(); got != "203.0.113.7:5000" {
		t.Errorf("RemoteAddr = %s, want the forwarded client", got)
	}
}

// LOCAL is what a balancer's own health check sends: there is no client, so the
// real peer address must stand.
func TestListener_V2LocalKeepsTheRealPeer(t *testing.T) {
	r := start(t, prefixes(t, "127.0.0.0/8"), 0)
	client := dial(t, r.addr, string(v2(0x20, 0x00, nil)))

	c := r.accepted(t)
	if got, want := c.RemoteAddr().String(), client.LocalAddr().String(); got != want {
		t.Errorf("RemoteAddr = %s, want the dialer's own address %s", got, want)
	}
}

// A PROXY header is trivially forged, so a peer outside proxy_trusted is dropped
// even when its header is perfect — and dropped WITHOUT a read, which is what
// keeps a hostile peer from occupying a parser slot for the whole deadline.
func TestListener_UntrustedPeerIsDroppedWithoutReading(t *testing.T) {
	const timeout = 2 * time.Second
	r := start(t, prefixes(t, "10.0.0.0/8"), timeout)

	started := time.Now()
	dial(t, r.addr, "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\r\n")

	if err := r.dropped(t); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("err = %v, want ErrUntrusted", err)
	}
	if elapsed := time.Since(started); elapsed >= timeout {
		t.Errorf("drop took %v, which is at least the header deadline — the trust "+
			"check must happen before any read", elapsed)
	}
}

func TestListener_MissingHeaderIsDropped(t *testing.T) {
	r := start(t, prefixes(t, "127.0.0.0/8"), 0)
	dial(t, r.addr, "EHLO mail.example.com\r\n")

	if err := r.dropped(t); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("err = %v, want ErrNoHeader", err)
	}
}

func TestListener_MalformedHeaderIsDropped(t *testing.T) {
	cases := map[string]string{
		"v1 bad port": "PROXY TCP4 203.0.113.7 198.51.100.1 smtp 25\r\n",
		"v2 bad ver":  string(v2(0x31, 0x11, v2TCP4([4]byte{1, 2, 3, 4}, [4]byte{5, 6, 7, 8}, 1, 2))),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			r := start(t, prefixes(t, "127.0.0.0/8"), 0)
			dial(t, r.addr, raw)
			if err := r.dropped(t); !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

// Bytes written in the same segment as the header must survive: a balancer that
// writes the header and the TLS ClientHello together leaves the ClientHello in
// the reader's buffer, and the tls.Conn above has to find it.
func TestListener_OverReadIsPreserved(t *testing.T) {
	r := start(t, prefixes(t, "127.0.0.0/8"), 0)

	payload := make([]byte, 300) // larger than hdrBufSize, so it spans both paths
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	dial(t, r.addr, "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\r\n"+string(payload))

	c := r.accepted(t)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("bytes after the header were lost or reordered")
	}
}

// The reason the header is resolved off the accept loop: one peer that connects
// and says nothing must not stall everyone behind it.
func TestListener_ASilentPeerDoesNotStallOthers(t *testing.T) {
	r := start(t, prefixes(t, "127.0.0.0/8"), 3*time.Second)

	// Connects and never speaks. Its slot stays occupied until the deadline.
	dial(t, r.addr, "")

	// A well-behaved connection right behind it must be served immediately.
	started := time.Now()
	dial(t, r.addr, "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\r\n")

	c := r.accepted(t)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("a good connection waited %v behind a silent one", elapsed)
	}
	if got := c.RemoteAddr().String(); got != "203.0.113.7:5000" {
		t.Errorf("RemoteAddr = %s", got)
	}
}

func TestListener_SilentPeerIsEventuallyDropped(t *testing.T) {
	r := start(t, prefixes(t, "127.0.0.0/8"), 200*time.Millisecond)
	dial(t, r.addr, "")

	if err := r.dropped(t); err == nil {
		t.Fatal("a silent peer must be dropped at the deadline")
	}
}

func TestListener_CloseUnblocksAccept(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l := NewListener(inner, prefixes(t, "127.0.0.0/8"), quiet(), nil)

	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	_ = l.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Accept should report an error after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}

	// Sticky: every later Accept reports too, rather than blocking forever.
	if _, err := l.Accept(); err == nil {
		t.Error("a second Accept after Close should also report an error")
	}
}
