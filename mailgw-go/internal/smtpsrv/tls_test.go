package smtpsrv

import (
	"bufio"
	"crypto/tls"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/tlsx"
)

// keypair generates a throwaway certificate the way a managed node does on first
// boot, so these tests exercise the same code path a shipped gateway uses.
func keypair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	certPath, keyPath, err := tlsx.EnsureSelfSigned(filepath.Join(t.TempDir(), "tls"), "devbook.local")
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}
	return certPath, keyPath
}

// tlsHarness starts a server with a keypair, tweaking the TLS settings first.
func tlsHarness(t *testing.T, tweak func(*config.Server)) *harness {
	t.Helper()
	cert, key := keypair(t)
	return startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, _ *Backend) {
		cfg.Server.TLS = config.TLSConfig{Cert: cert, Key: key, STARTTLS: true}
		if tweak != nil {
			tweak(&cfg.Server)
		}
	})
}

func ehloCaps(t *testing.T, c *client) string {
	t.Helper()
	c.greet()
	_, raw := c.cmd("EHLO probe.invalid")
	return raw
}

func TestTLS_STARTTLSIsAdvertisedOnlyWithAKeypair(t *testing.T) {
	t.Run("with one", func(t *testing.T) {
		h := tlsHarness(t, nil)
		if caps := ehloCaps(t, dialClient(t, h.addr)); !strings.Contains(caps, "STARTTLS") {
			t.Errorf("EHLO does not advertise STARTTLS:\n%s", caps)
		}
	})

	t.Run("without one", func(t *testing.T) {
		// The default harness has no keypair — which is also what every
		// pre-existing test in this package runs on, and what the Bun SMTP
		// suite's capability assertion expects.
		h := startServerWithRules(t, compileRulesYAML(t, relayEverything))
		if caps := ehloCaps(t, dialClient(t, h.addr)); strings.Contains(caps, "STARTTLS") {
			t.Errorf("EHLO advertises STARTTLS with no certificate:\n%s", caps)
		}
	})

	// starttls is an opt-out, for a node whose plain port must stay plain.
	t.Run("opted out", func(t *testing.T) {
		h := tlsHarness(t, func(s *config.Server) { s.TLS.STARTTLS = false })
		if caps := ehloCaps(t, dialClient(t, h.addr)); strings.Contains(caps, "STARTTLS") {
			t.Errorf("starttls: false still advertised the upgrade:\n%s", caps)
		}
	})
}

func TestTLS_STARTTLSDeliversAndIsRecordedInTheAuditRow(t *testing.T) {
	h := tlsHarness(t, nil)

	raw := dialTLSSession(t, h.addr)
	defer raw.Close()

	c := &client{t: t, conn: raw, r: bufio.NewReader(raw)}
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	if code, reply := c.cmd("RCPT TO:<rcpt@example.com>"); code != 250 {
		t.Fatalf("RCPT: %d %q", code, reply)
	}
	c.cmd("DATA")
	if code, reply := c.sendBody("Subject: over tls\r\n\r\nbody"); code != 250 {
		t.Fatalf("end of DATA: %d %q", code, reply)
	}

	var seen bool
	for _, e := range h.events.sent {
		conn, ok := e.Body.(events.Connection)
		if !ok || e.Kind != events.KindConnection {
			continue
		}
		seen = true
		if !conn.UsingTLS {
			t.Error("using_tls is false on a session that negotiated STARTTLS")
		}
	}
	if !seen {
		t.Fatal("no connection event was posted")
	}
}

// go-smtp's handleStartTLS calls setSession(nil) (conn.go:947-953), so
// Backend.NewSession — and greetPolicy with it — runs a SECOND time on an
// upgraded connection.
//
// This rule cannot match before the handshake and must match after it, which
// proves the second evaluation happens; and the refusal must be counted once,
// which is what a doubled greetPolicy would get wrong.
func TestTLS_PolicyIsReevaluatedOnceAfterTheUpgrade(t *testing.T) {
	const rules = `
version: 1
policy:
  - name: refuse-encrypted-sessions
    match: {field: conn.tls, op: eq, value: true}
    then:
      - {action: reject, code: 554, message: "not welcome"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	cert, key := keypair(t)
	h := startServerTuned(t, compileRulesYAML(t, rules), func(cfg *config.Config, _ *Backend) {
		cfg.Server.TLS = config.TLSConfig{Cert: cert, Key: key, STARTTLS: true}
	})

	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.greet()
	if code, raw := c.cmd("EHLO probe.invalid"); code != 250 {
		t.Fatalf("EHLO before STARTTLS: got %d %q, want 250 — the rule must not match yet", code, raw)
	}
	if got := h.metrics.MsgRejected.Load(); got != 0 {
		t.Fatalf("msg_rejected = %d before the upgrade, want 0", got)
	}
	if code, raw := c.cmd("STARTTLS"); code != 220 {
		t.Fatalf("STARTTLS: got %d %q", code, raw)
	}

	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "devbook.local"})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = tc.SetDeadline(time.Now().Add(20 * time.Second))

	tlsClient := &client{t: t, conn: tc, r: bufio.NewReader(tc)}
	if code, raw := tlsClient.cmd("EHLO probe.invalid"); code != 554 {
		t.Fatalf("EHLO after STARTTLS: got %d %q, want 554 — the session was not re-evaluated", code, raw)
	}
	if got := h.metrics.MsgRejected.Load(); got != 1 {
		t.Errorf("msg_rejected = %d, want 1 — one refusal, counted once", got)
	}
}

// startTLSClient upgrades a fresh connection and returns a client speaking over
// it, plus the raw conn so the caller can close it.
func startTLSClient(t *testing.T, h *harness) (*client, func()) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.greet()
	c.cmd("EHLO sender.example.com")
	if code, raw := c.cmd("STARTTLS"); code != 220 {
		t.Fatalf("STARTTLS: got %d %q", code, raw)
	}

	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "devbook.local"})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = tc.SetDeadline(time.Now().Add(20 * time.Second))

	return &client{t: t, conn: tc, r: bufio.NewReader(tc)}, func() { _ = conn.Close() }
}

// A STARTTLS connection must produce exactly ONE Connection row.
//
// go-smtp logs the pre-upgrade session out before creating the post-upgrade one
// (conn.go:948), and every session mints its own connUUID — so reporting
// unconditionally from Logout writes two rows, under two different uuids, for a
// single TCP connection. This is the regression test for that.
func TestTLS_UpgradedConnectionReportsExactlyOneConnectionEvent(t *testing.T) {
	cert, key := keypair(t)
	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, _ *Backend) {
		cfg.Server.TLS = config.TLSConfig{Cert: cert, Key: key, STARTTLS: true}
	})

	t.Run("without a message", func(t *testing.T) {
		tc, closeConn := startTLSClient(t, h)
		tc.cmd("EHLO sender.example.com")
		tc.cmd("QUIT")
		closeConn()

		time.Sleep(200 * time.Millisecond)
		if got := connEvents(t, h, 1); len(got) != 1 {
			t.Fatalf("got %d Connection events for one TCP connection, want exactly 1", len(got))
		}
	})
}

// The same, for a connection that upgrades and then actually delivers: Data
// posts, and neither Logout may add a second row.
func TestTLS_UpgradedConnectionThatDeliversReportsOnce(t *testing.T) {
	cert, key := keypair(t)
	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, _ *Backend) {
		cfg.Server.TLS = config.TLSConfig{Cert: cert, Key: key, STARTTLS: true}
	})

	tc, closeConn := startTLSClient(t, h)
	tc.cmd("EHLO sender.example.com")
	tc.cmd("MAIL FROM:<me@ngm.dev>")
	tc.cmd("RCPT TO:<you@ngm.dev>")
	tc.cmd("DATA")
	if code, raw := tc.sendBody("Subject: encrypted\r\n\r\nbody"); code != 250 {
		t.Fatalf("end of DATA: got %d (%s), want 250", code, raw)
	}
	tc.cmd("QUIT")
	closeConn()

	time.Sleep(200 * time.Millisecond)
	got := connEvents(t, h, 1)
	if len(got) != 1 {
		t.Fatalf("got %d Connection events for one TCP connection, want exactly 1", len(got))
	}
	if !got[0].UsingTLS {
		t.Error("using_tls should be true on the reported session")
	}
}

func TestTLS_ImplicitTLSListenerSpeaksTLSFromTheFirstByte(t *testing.T) {
	cert, key := keypair(t)
	tlsCfg, err := NewTLSConfig(cert, key)
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}

	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, _ *Backend) {
		cfg.Server.TLS = config.TLSConfig{Cert: cert, Key: key, STARTTLS: true}
	})

	// The listener wrapping cmd/mailgw-go/listeners.go performs for an
	// implicit_tls entry: Guard OUTSIDE, tls.NewListener inside.
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	allow, err := config.ParseAllowlist([]byte(`{"allowed":["127.0.0.0/8","::1/128"]}`), "test")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	guarded := Guard(tls.NewListener(raw, tlsCfg), func() *config.Allowlist { return allow }, nil, h.metrics)
	t.Cleanup(func() { _ = guarded.Close() })
	go func() { _ = h.srv.Serve(guarded) }()

	conn, err := tls.Dial("tcp", guarded.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	if code, reply := c.greet(); code != 220 {
		t.Fatalf("greeting over implicit TLS: %d %q", code, reply)
	}
	// The upgrade has already happened, so it must not be offered again.
	if _, reply := c.cmd("EHLO probe.invalid"); strings.Contains(reply, "STARTTLS") {
		t.Errorf("an already-encrypted session was offered STARTTLS:\n%s", reply)
	}
}

// A nil allowlist denies everything (it is the fail-closed zero value), so this
// exercises the deny path on an encrypted socket: the 550 has to be readable,
// which is why Guard wraps the TLS listener rather than the other way round.
func TestTLS_AllowlistDenialOnAnImplicitTLSListenerIsReadable(t *testing.T) {
	cert, key := keypair(t)
	tlsCfg, err := NewTLSConfig(cert, key)
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}

	deny := &config.Allowlist{}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	guarded := Guard(tls.NewListener(raw, tlsCfg), func() *config.Allowlist { return deny }, nil, nil)
	t.Cleanup(func() { _ = guarded.Close() })
	go func() {
		for {
			conn, err := guarded.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	conn, err := tls.Dial("tcp", guarded.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the denial: %v", err)
	}
	if !strings.HasPrefix(line, "550") {
		t.Errorf("denial = %q, want a readable 550 inside the TLS session", line)
	}
}

// dialTLSSession opens a plain connection and upgrades it with STARTTLS.
func dialTLSSession(t *testing.T, addr string) *tls.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.greet()
	c.cmd("EHLO probe.invalid")
	if code, raw := c.cmd("STARTTLS"); code != 220 {
		t.Fatalf("STARTTLS: got %d %q, want 220", code, raw)
	}

	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "devbook.local"})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = tc.SetDeadline(time.Now().Add(20 * time.Second))
	return tc
}
