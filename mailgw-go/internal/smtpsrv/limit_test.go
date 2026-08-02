package smtpsrv

import (
	"bufio"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

// serveCapped builds the production chain —
//
//	tcp -> Meter -> Guard -> Limit -> (accept)
//
// and holds every accepted connection open, because the whole subject here is
// what happens while sessions are still running. Accepted connections arrive on
// the channel; the caller closes them to release slots.
func serveCapped(t *testing.T, limit int, m *obs.Metrics) (addr string, accepted chan net.Conn, refused chan string) {
	t.Helper()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	accepted = make(chan net.Conn, 16)
	refused = make(chan string, 16)

	list := allowlistFrom(t, `{"allowed":["127.0.0.1","::1"]}`)
	guarded := &allowlistListener{
		Listener: Meter(inner),
		allowed:  func() *config.Allowlist { return list },
		log:      quietLogger(),
		metrics:  m,
	}
	l := &limitListener{
		Listener: guarded,
		sem:      make(chan struct{}, limit),
		log:      quietLogger(),
		metrics:  m,
		onRefuse: func(a string) { refused <- a },
	}

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("220 test ESMTP Service Ready\r\n"))
			accepted <- c
		}
	}()

	t.Cleanup(func() { _ = l.Close() })
	return inner.Addr().String(), accepted, refused
}

// dialAndGreet opens a connection and reads the first line, which is how a test
// learns the session was actually handed to the server rather than refused.
func dialAndGreet(t *testing.T, addr string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	return conn, line
}

func waitFor(t *testing.T, ch chan string, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestLimit_RefusesPastTheCapAndKeepsServing(t *testing.T) {
	const maxConns = 2
	m := obs.New()
	addr, accepted, refused := serveCapped(t, maxConns, m)

	// Fill every slot. Each of these must get a real greeting.
	for i := range maxConns {
		if _, line := dialAndGreet(t, addr); line[:4] != "220 " {
			t.Fatalf("connection %d: got %q, want a 220 greeting", i, line)
		}
	}
	for range maxConns {
		select {
		case <-accepted:
		case <-time.After(2 * time.Second):
			t.Fatal("a connection under the cap was never handed to the server")
		}
	}

	// One more. It must be answered 421 rather than left hanging, and the
	// socket must be closed rather than held open with no session behind it.
	over, line := dialAndGreet(t, addr)
	if line[:4] != "421 " {
		t.Fatalf("over the cap: got %q, want 421", line)
	}
	waitFor(t, refused, "the refusal hook")

	_ = over.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := over.Read(make([]byte, 1)); err == nil {
		t.Errorf("a refused connection is still open (read %d bytes)", n)
	}

	if got := m.ConnThrottled.Load(); got != 1 {
		t.Errorf("conn_throttled = %d, want 1", got)
	}
	// The cap sits outside the allowlist, so a throttled peer was accepted by
	// Guard first — all three connections count.
	if got := m.ConnAccepted.Load(); got != maxConns+1 {
		t.Errorf("conn_accepted = %d, want %d", got, maxConns+1)
	}
}

// The half a naive semaphore gets wrong: taking a slot is easy, giving it back
// when the session ends is where a cap silently becomes permanent.
func TestLimit_ReleasesTheSlotWhenASessionEnds(t *testing.T) {
	m := obs.New()
	addr, accepted, refused := serveCapped(t, 1, m)

	if _, line := dialAndGreet(t, addr); line[:4] != "220 " {
		t.Fatalf("first connection was not accepted: %q", line)
	}
	var first net.Conn
	select {
	case first = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("the first connection never reached the server")
	}

	if _, line := dialAndGreet(t, addr); line[:4] != "421 " {
		t.Fatalf("second connection: got %q, want 421 while the slot is held", line)
	}
	waitFor(t, refused, "the refusal hook")

	// End the first session. Its slot must come back.
	_ = first.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		_, line := dialAndGreet(t, addr)
		if line[:4] == "220 " {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was never released; the cap is now permanent")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close is called more than once on a normal path — go-smtp closes the session's
// connection and Server.Close closes every live one again at shutdown. Releasing
// twice would let the cap drift upward until it bounded nothing.
func TestLimit_DoubleCloseReleasesOneSlotOnly(t *testing.T) {
	l := &limitListener{sem: make(chan struct{}, 2), log: quietLogger()}
	l.sem <- struct{}{}
	l.sem <- struct{}{}

	client, server := net.Pipe()
	defer client.Close()

	c := &slotConn{Conn: server}
	c.arm(l.release)
	_ = c.Close()
	_ = c.Close()

	if got := len(l.sem); got != 1 {
		t.Errorf("%d slots in use after a double close, want 1", got)
	}
}

// A metered connection that never reached Limit — refused by the cap, or denied
// by the allowlist — holds no slot, so closing it must release nothing.
func TestLimit_AnUnarmedMeteredConnReleasesNothing(t *testing.T) {
	l := &limitListener{sem: make(chan struct{}, 2), log: quietLogger()}
	l.sem <- struct{}{}

	client, server := net.Pipe()
	defer client.Close()

	c := &slotConn{Conn: server}
	_ = c.Close()

	if got := len(l.sem); got != 1 {
		t.Errorf("%d slots in use, want the 1 that was already taken", got)
	}
}

// The reason Meter exists: go-smtp finds TLS with a bare type assertion, so
// whatever Limit hands on must still BE the *tls.Conn. Wrapping it here is what
// M11 did, and it cost the session its TLS identity and its handshake deadline.
func TestLimit_HandsOnTheTLSConnUnwrapped(t *testing.T) {
	cert, key := keypair(t)
	tlsCfg, err := NewTLSConfig(cert, key)
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// The shipped order, minus Guard, which does not wrap.
	capped := Limit(tls.NewListener(Meter(raw), tlsCfg), make(chan struct{}, 4), quietLogger(), nil)
	t.Cleanup(func() { _ = capped.Close() })

	got := make(chan net.Conn, 1)
	go func() {
		c, err := capped.Accept()
		if err != nil {
			return
		}
		got <- c
	}()

	// Dialled from its own goroutine: tls.Dial blocks on the handshake, and
	// nothing here drives the server side of one — the subject is what Accept
	// returns, not what it negotiates.
	dialed := make(chan net.Conn, 1)
	go func() {
		c, err := tls.Dial("tcp", capped.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err == nil {
			dialed <- c
		}
	}()
	t.Cleanup(func() {
		select {
		case c := <-dialed:
			_ = c.Close()
		default:
		}
	})

	select {
	case c := <-got:
		if _, ok := c.(*tls.Conn); !ok {
			t.Fatalf("Accept returned %T; go-smtp asserts *tls.Conn and would "+
				"treat this session as cleartext", c)
		}
		if slotOf(c) == nil {
			t.Error("the metered connection is unreachable, so the slot can never be released")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no connection was accepted")
	}
}

// A zero-capacity semaphore would refuse every connection, which is far worse
// than an absent cap. Configuration already rejects max.connections <= 0; this
// pins the belt-and-braces.
func TestLimit_ZeroCapacityReturnsTheListenerUnwrapped(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer inner.Close()

	if got := Limit(inner, make(chan struct{}, 0), quietLogger(), nil); got != inner {
		t.Error("a zero-capacity cap must not wrap the listener")
	}
	if got := Limit(inner, nil, quietLogger(), nil); got != inner {
		t.Error("a nil semaphore must not wrap the listener")
	}
}
