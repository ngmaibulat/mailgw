package smtpsrv

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func allowlistFrom(t *testing.T, body string) *config.Allowlist {
	t.Helper()
	a, err := config.ParseAllowlist([]byte(body), "allowlist profile")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	return a
}

// serve wraps a loopback listener and hands accepted connections to accepted.
func serve(t *testing.T, list *config.Allowlist) (addr string, accepted chan string, denied chan string) {
	t.Helper()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	accepted = make(chan string, 8)
	denied = make(chan string, 8)

	var mu sync.Mutex
	current := list
	l := &allowlistListener{
		Listener: inner,
		allowed: func() *config.Allowlist {
			mu.Lock()
			defer mu.Unlock()
			return current
		},
		log:    quietLogger(),
		onDeny: func(a string) { denied <- a },
	}

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c.RemoteAddr().String()
			// Behave like a server so the client sees something.
			_, _ = c.Write([]byte("220 test ESMTP Service Ready\r\n"))
			_ = c.Close()
		}
	}()

	t.Cleanup(func() { _ = l.Close() })
	return inner.Addr().String(), accepted, denied
}

func TestListener_AllowsPermittedPeer(t *testing.T) {
	addr, accepted, denied := serve(t, allowlistFrom(t, `{"allowed":["127.0.0.1"]}`))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.HasPrefix(line, "220 ") {
		t.Errorf("got %q, want a 220 greeting", line)
	}

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("connection was never accepted")
	}
	select {
	case a := <-denied:
		t.Fatalf("connection was denied: %s", a)
	default:
	}
}

// The peer must be rejected before any greeting, matching Haraka's
// DENYDISCONNECT at npFilter.js:69.
func TestListener_DeniesUnlistedPeerBeforeGreeting(t *testing.T) {
	addr, accepted, denied := serve(t, allowlistFrom(t, `{"allowed":["10.99.99.99"]}`))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(body)
	if !strings.HasPrefix(got, "550 ") {
		t.Errorf("got %q, want a 550 rejection", got)
	}
	if strings.Contains(got, "220") {
		t.Errorf("a denied peer must never see a greeting, got %q", got)
	}

	select {
	case <-denied:
	case <-time.After(2 * time.Second):
		t.Fatal("connection was never denied")
	}
	select {
	case a := <-accepted:
		t.Fatalf("denied connection reached the server: %s", a)
	default:
	}
}

// A denial must not kill the listener; the next permitted peer still connects.
func TestListener_SurvivesDenialAndKeepsServing(t *testing.T) {
	addr, _, denied := serve(t, allowlistFrom(t, `{"allowed":["10.99.99.99"]}`))

	for i := 0; i < 3; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_, _ = io.ReadAll(conn)
		_ = conn.Close()

		select {
		case <-denied:
		case <-time.After(2 * time.Second):
			t.Fatalf("dial %d was never denied", i)
		}
	}
}

// A reload that widens the allowlist must take effect on the next connection
// without restarting the listener.
func TestListener_PicksUpReloadedAllowlist(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer inner.Close()

	var mu sync.Mutex
	current := allowlistFrom(t, `{"allowed":["10.99.99.99"]}`)

	denied := make(chan string, 8)
	accepted := make(chan string, 8)
	l := &allowlistListener{
		Listener: inner,
		allowed: func() *config.Allowlist {
			mu.Lock()
			defer mu.Unlock()
			return current
		},
		log:    quietLogger(),
		onDeny: func(a string) { denied <- a },
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c.RemoteAddr().String()
			_ = c.Close()
		}
	}()

	// Denied under the original list.
	c1, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _ = io.ReadAll(c1)
	_ = c1.Close()
	select {
	case <-denied:
	case <-time.After(2 * time.Second):
		t.Fatal("first connection should have been denied")
	}

	// Widen the allowlist, as a SIGHUP reload would.
	mu.Lock()
	current = allowlistFrom(t, `{"allowed":["127.0.0.1","::1"]}`)
	mu.Unlock()

	c2, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c2.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("second connection should have been accepted after reload")
	}
}

// A nil allowlist (a load that failed) must deny, not panic and not allow.
func TestListener_NilAllowlistDenies(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer inner.Close()

	denied := make(chan string, 4)
	l := &allowlistListener{
		Listener: inner,
		allowed:  func() *config.Allowlist { return nil },
		log:      quietLogger(),
		onDeny:   func(a string) { denied <- a },
	}
	go func() {
		for {
			if _, err := l.Accept(); err != nil {
				return
			}
		}
	}()

	conn, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case <-denied:
	case <-time.After(2 * time.Second):
		t.Fatal("a nil allowlist must deny")
	}
}

func TestAddrOf(t *testing.T) {
	tcp := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 25}
	if got := addrOf(tcp); got.Unmap().String() != "127.0.0.1" {
		t.Errorf("TCPAddr: got %q", got.String())
	}

	v6 := &net.TCPAddr{IP: net.ParseIP("::1"), Port: 25}
	if got := addrOf(v6); got.String() != "::1" {
		t.Errorf("IPv6 TCPAddr: got %q", got.String())
	}

	// Anything unparseable must yield the zero Addr, which Allowed() rejects.
	if got := addrOf(fakeAddr("not-an-address")); got.IsValid() {
		t.Errorf("unparseable address should be invalid, got %q", got.String())
	}
}

type fakeAddr string

func (f fakeAddr) Network() string { return "fake" }
func (f fakeAddr) String() string  { return string(f) }
