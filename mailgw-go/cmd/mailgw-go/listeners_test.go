package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ratelimit"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/smtpsrv"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/tlsx"
)

// The listener chain is assembled here and nowhere else, and until M16 nothing
// tested it: internal/smtpsrv's TLS tests build Guard(tls.NewListener(...))
// WITHOUT the connection cap, so the composition that actually ships — where
// the cap's accounting used to wrap the *tls.Conn and hide it from go-smtp —
// had no coverage at all.

// probeSession records what go-smtp made of the connection, which is the whole
// point: TLSConnectionState is how the server decides whether a session is
// encrypted, and a wrapper in the wrong place makes it answer no.
type probeSession struct{}

func (probeSession) Reset()                               {}
func (probeSession) Logout() error                        { return nil }
func (probeSession) Mail(string, *smtp.MailOptions) error { return nil }
func (probeSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (probeSession) Data(r io.Reader) error               { _, err := io.Copy(io.Discard, r); return err }

type probeBackend struct {
	sawTLS   atomic.Bool
	sessions atomic.Int64
}

func (b *probeBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	b.sessions.Add(1)
	if _, ok := c.TLSConnectionState(); ok {
		b.sawTLS.Store(true)
	}
	return probeSession{}, nil
}

// startChain brings up the production listener chain over a real go-smtp server.
func startChain(t *testing.T, implicitTLS bool, maxConns int) (addr string, b *probeBackend, m *obs.Metrics) {
	t.Helper()
	return startChainLimited(t, implicitTLS, maxConns, nil)
}

// startChainLimited is startChain with a rate limiter installed, for the M15
// cases. A nil limiter is what every earlier test gets, and Throttle then leaves
// the chain exactly as it was.
func startChainLimited(t *testing.T, implicitTLS bool, maxConns int, limiter *ratelimit.Limiter) (addr string, b *probeBackend, m *obs.Metrics) {
	t.Helper()

	certPath, keyPath, err := tlsx.EnsureSelfSigned(t.TempDir(), "chain.test")
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}
	tlsCfg, err := smtpsrv.NewTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("tls config: %v", err)
	}

	b = &probeBackend{}
	srv := smtp.NewServer(b)
	srv.TLSConfig = tlsCfg
	srv.Domain = "chain.test"
	// Short, because one case below waits for go-smtp's own deadline to reap a
	// silent peer. In production this is inactivity_timeout.
	srv.ReadTimeout = 2 * time.Second
	srv.WriteTimeout = 2 * time.Second

	cfg := &config.Config{}
	cfg.Server.Listen = []config.Listener{{Addr: "127.0.0.1:0", ImplicitTLS: implicitTLS}}
	cfg.Server.Max.Connections = maxConns

	allow, err := config.ParseAllowlist([]byte(`{"allowed":["127.0.0.0/8","::1/128"]}`), "test")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}

	m = obs.New()
	lns := &smtpListeners{}
	var limiterFn smtpsrv.LimiterFunc
	if limiter != nil {
		limiterFn = func() *ratelimit.Limiter { return limiter }
	}
	if err := lns.start(context.Background(), srv, cfg,
		func() *config.Allowlist { return allow }, limiterFn, m,
		slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		lns.closeAll()
		_ = srv.Close()
	})

	if len(lns.lns) != 1 {
		t.Fatalf("got %d listeners, want 1", len(lns.lns))
	}
	return lns.lns[0].Addr().String(), b, m
}

// dialTLS opens an implicit-TLS session and returns it with its greeting.
func dialTLS(t *testing.T, addr string) (*tls.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	return conn, r, line
}

// ehlo sends EHLO and collects the whole multiline reply.
func ehlo(t *testing.T, conn net.Conn, r *bufio.Reader) string {
	t.Helper()
	if _, err := conn.Write([]byte("EHLO probe.invalid\r\n")); err != nil {
		t.Fatalf("write EHLO: %v", err)
	}
	var b strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read EHLO reply: %v", err)
		}
		b.WriteString(line)
		if len(line) >= 4 && line[3] == ' ' {
			return b.String()
		}
	}
}

// The regression M16 exists for: with the cap in the chain, go-smtp must still
// see a *tls.Conn on an implicit-TLS listener.
func TestChain_ImplicitTLSStaysVisibleToTheServer(t *testing.T) {
	addr, b, _ := startChain(t, true, 16)

	conn, r, greeting := dialTLS(t, addr)
	if !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("greeting: %q", greeting)
	}
	reply := ehlo(t, conn, r)

	if !b.sawTLS.Load() {
		t.Error("the server saw a cleartext session on an implicit-TLS listener: " +
			"something in the chain is hiding the *tls.Conn")
	}
	// The consequence a client can see: STARTTLS offered inside TLS, which
	// go-smtp would then honour with a nested handshake.
	if strings.Contains(reply, "STARTTLS") {
		t.Errorf("an already-encrypted session was offered STARTTLS:\n%s", reply)
	}
}

// The cap still has to work through TLS, and its 421 has to be readable — which
// is why admission stays outside tls.NewListener.
func TestChain_CapRefusesInsideTheTLSSession(t *testing.T) {
	addr, _, m := startChain(t, true, 1)

	if _, _, greeting := dialTLS(t, addr); !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("first session: %q", greeting)
	}

	_, _, refused := dialTLS(t, addr)
	if !strings.HasPrefix(refused, "421 ") {
		t.Fatalf("over the cap: got %q, want 421 decrypted", refused)
	}
	if got := m.ConnThrottled.Load(); got != 1 {
		t.Errorf("conn_throttled = %d, want 1", got)
	}
}

// A slot has to come back when the TLS session ends — the release rides on
// tls.Conn.Close reaching the metered conn underneath it.
func TestChain_SlotIsReleasedWhenATLSSessionEnds(t *testing.T) {
	addr, _, _ := startChain(t, true, 1)

	first, _, greeting := dialTLS(t, addr)
	if !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("first session: %q", greeting)
	}
	if _, _, over := dialTLS(t, addr); !strings.HasPrefix(over, "421 ") {
		t.Fatalf("second session while the slot is held: %q", over)
	}
	_ = first.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, line := dialTLS(t, addr); strings.HasPrefix(line, "220 ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot was never released; the cap is now permanent")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A peer that completes TCP and then says nothing must not hold a slot for ever.
// Before M16 this was the leak: with the *tls.Conn hidden, go-smtp skipped the
// only pre-handshake read deadline it ever sets, so the session goroutine parked
// on the ClientHello and its slot never came back.
func TestChain_SilentPeerDoesNotHoldASlotForEver(t *testing.T) {
	addr, _, _ := startChain(t, true, 1)

	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer silent.Close()

	// It has the only slot. Wait for the server's own deadline to reap it, then
	// a real client must be greeted. ReadTimeout is 2s above, so this bounds the
	// leak rather than proving a particular timeout.
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			line, rerr := bufio.NewReader(conn).ReadString('\n')
			_ = conn.Close()
			if rerr == nil && strings.HasPrefix(line, "220 ") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("a silent peer held its slot until the test timed out")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// A refusal is written off the accept loop, so a peer that will not read its 421
// cannot stop the gateway accepting anyone else.
func TestChain_RefusalDoesNotStallTheAcceptLoop(t *testing.T) {
	addr, _, _ := startChain(t, false, 1)

	held, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer held.Close()
	if _, err := bufio.NewReader(held).ReadString('\n'); err != nil {
		t.Fatalf("read greeting: %v", err)
	}

	// Over the cap and never reading. Its refusal is written on its own
	// goroutine; the accept loop must move straight on.
	stuck, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer stuck.Close()

	done := make(chan string, 1)
	go func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			done <- "dial: " + err.Error()
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			done <- "read: " + err.Error()
			return
		}
		done <- line
	}()

	select {
	case line := <-done:
		if !strings.HasPrefix(line, "421 ") {
			t.Fatalf("third connection: got %q, want 421", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the accept loop stalled behind a peer that would not read its refusal")
	}
}
