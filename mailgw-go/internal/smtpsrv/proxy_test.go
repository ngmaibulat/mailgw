package smtpsrv

import (
	"crypto/tls"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/proxyproto"
)

// proxyStack builds the production listener chain:
//
//	tcp -> proxyproto -> tls -> Guard -> (accept)
//
// This is the composition test for M10.5. Everything else about the PROXY
// protocol is covered in internal/proxyproto; what can only be checked here is
// that the forwarded address survives two wrappers and reaches the allowlist,
// which is the entire point of the feature.
func proxyStack(t *testing.T, allowlist string, trusted []string, withTLS bool) (addr string, accepted chan net.Conn, denied chan string) {
	t.Helper()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = inner.Addr().String()

	prefixes, err := config.ParsePrefixes(trusted)
	if err != nil {
		t.Fatalf("ParsePrefixes: %v", err)
	}

	var ln net.Listener = proxyproto.NewListener(inner, prefixes, quietLogger(), nil)

	if withTLS {
		cert, key := keypair(t)
		tlsCfg, err := NewTLSConfig(cert, key)
		if err != nil {
			t.Fatalf("NewTLSConfig: %v", err)
		}
		ln = tls.NewListener(ln, tlsCfg)
	}

	accepted = make(chan net.Conn, 4)
	denied = make(chan string, 4)

	list := allowlistFrom(t, allowlist)
	guarded := &allowlistListener{
		Listener: ln,
		allowed:  func() *config.Allowlist { return list },
		log:      quietLogger(),
		onDeny:   func(a string) { denied <- a },
	}

	go func() {
		for {
			c, err := guarded.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() { _ = guarded.Close() })

	return addr, accepted, denied
}

const proxyHeader = "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\r\n"

// The milestone, in one assertion: an allowlist that names ONLY 203.0.113.7
// accepts a connection dialled from loopback, because the PROXY header says the
// client is 203.0.113.7. Without the wrapper the allowlist would see 127.0.0.1
// — and behind a real balancer it would see the balancer, making the relay open
// to everything behind it.
func TestProxy_AllowlistSeesTheForwardedAddress(t *testing.T) {
	addr, accepted, denied := proxyStack(t, `{"allowed":["203.0.113.7"]}`, []string{"127.0.0.0/8"}, false)

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(proxyHeader)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case c := <-accepted:
		if got := c.RemoteAddr().String(); got != "203.0.113.7:5000" {
			t.Errorf("RemoteAddr = %s, want the forwarded client", got)
		}
	case a := <-denied:
		t.Fatalf("the allowlist denied %s — it is still seeing the peer address", a)
	case <-time.After(3 * time.Second):
		t.Fatal("connection was neither accepted nor denied")
	}
}

// The negative half: the allowlist is applied to the forwarded address, so a
// header naming an address that is not listed is refused.
func TestProxy_ForwardedAddressIsStillSubjectToTheAllowlist(t *testing.T) {
	addr, accepted, denied := proxyStack(t, `{"allowed":["203.0.113.7"]}`, []string{"127.0.0.0/8"}, false)

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// A different forwarded client, from the same trusted balancer.
	if _, err := conn.Write([]byte("PROXY TCP4 198.51.100.9 198.51.100.1 5000 25\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case a := <-denied:
		if a[:len("198.51.100.9")] != "198.51.100.9" {
			t.Errorf("denied %s, want the forwarded address", a)
		}
	case c := <-accepted:
		t.Fatalf("accepted a client the allowlist does not name: %v", c.RemoteAddr())
	case <-time.After(3 * time.Second):
		t.Fatal("connection was neither accepted nor denied")
	}

	// The 550 is written through the stack and is readable by the peer.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if n == 0 || string(buf[:3]) != "550" {
		t.Errorf("denial reply = %q, want a readable 550", buf[:n])
	}
}

// The forwarded address must survive the TLS wrapper too. tls.Conn.RemoteAddr
// delegates to the conn it wraps, which is what lets Guard sit outside the TLS
// listener and still see the real client.
func TestProxy_ForwardedAddressSurvivesTheTLSWrapper(t *testing.T) {
	addr, accepted, denied := proxyStack(t, `{"allowed":["203.0.113.7"]}`, []string{"127.0.0.0/8"}, true)

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(proxyHeader)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The handshake is driven from the accepted side, so complete it here.
	go func() {
		tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "devbook.local"})
		_ = tc.Handshake()
	}()

	select {
	case c := <-accepted:
		if got := c.RemoteAddr().String(); got != "203.0.113.7:5000" {
			t.Errorf("RemoteAddr through tls.Conn = %s, want the forwarded client", got)
		}
	case a := <-denied:
		t.Fatalf("the allowlist denied %s through the TLS wrapper", a)
	case <-time.After(5 * time.Second):
		t.Fatal("connection was neither accepted nor denied")
	}
}

// Without the wrapper in the chain nothing changes: the peer address is what the
// allowlist judges, exactly as it always has.
func TestProxy_WithoutTheWrapperTheAllowlistSeesThePeer(t *testing.T) {
	addr, accepted, denied := serve(t, allowlistFrom(t, `{"allowed":["203.0.113.7"]}`))

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(proxyHeader)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-denied:
	case <-accepted:
		t.Fatal("a PROXY header must not be honoured on a listener that did not ask for it")
	case <-time.After(3 * time.Second):
		t.Fatal("connection was neither accepted nor denied")
	}
}

var (
	_ = io.Discard
	_ = netip.Addr{}
)
