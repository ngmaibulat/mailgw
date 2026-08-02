package deliver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// M9.2: `data_timeout` and `connect_timeout` were threaded from server.yaml all
// the way into deliver.Options and then never applied to the connection. A
// relay that completes the TCP handshake and then stops answering held the
// worker goroutine and its per-group slot forever.
//
// These tests use a listener that accepts and then says nothing at all, which
// is the shape a real wedged relay has: the dial succeeds, so `connect_timeout`
// on net.Dialer never fires.

// stalledRelay accepts connections and never writes. It optionally speaks a
// greeting and EHLO response first, so the stall can be placed at a chosen
// point in the conversation.
func stalledRelay(t *testing.T, script []string) relays.Relay {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// Hold the connection open, unread and (after the script)
				// unanswered, until the test tears the listener down.
				defer c.Close()
				for _, line := range script {
					if _, err := c.Write([]byte(line + "\r\n")); err != nil {
						return
					}
				}
				buf := make([]byte, 1024)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	host, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	p, _ := net.LookupPort("tcp", port)
	return relays.Relay{
		Name:     "stalled",
		Exchange: host,
		Port:     relays.Port(p),
		// Plaintext, so the stall is hit on the SMTP conversation rather than
		// on a TLS handshake that would never complete either way.
		TLS: relays.TLSNone,
	}
}

// A relay that accepts the connection and never sends a greeting.
func TestDeliver_StalledBeforeGreetingHitsConnectTimeout(t *testing.T) {
	relay := stalledRelay(t, nil)

	start := time.Now()
	res := Deliver(context.Background(), relay, msg("a@x.com", "b@y.com"), Options{
		LocalName:      "test.invalid",
		ConnectTimeout: 300 * time.Millisecond,
		DataTimeout:    300 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if res.Err == nil {
		t.Fatal("expected a connection-level error, got none")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Deliver took %v — the deadline was not applied", elapsed)
	}
	t.Logf("returned in %v with %v", elapsed, res.Err)
}

// A relay that greets and answers EHLO, then stalls at MAIL FROM. This is past
// the point net.Dialer's timeout covers, so only an explicit deadline bounds it.
func TestDeliver_StalledAfterEHLOHitsDeadline(t *testing.T) {
	relay := stalledRelay(t, []string{
		"220 stalled.invalid ESMTP",
		"250-stalled.invalid",
		"250 SIZE 0",
	})

	start := time.Now()
	res := Deliver(context.Background(), relay, msg("a@x.com", "b@y.com"), Options{
		LocalName:      "test.invalid",
		ConnectTimeout: 300 * time.Millisecond,
		DataTimeout:    300 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if res.Err == nil && len(res.Rcpts) == 0 {
		t.Fatal("expected either a connection-level error or per-recipient outcomes")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Deliver took %v — the deadline was not applied past EHLO", elapsed)
	}
	t.Logf("returned in %v with err=%v rcpts=%d", elapsed, res.Err, len(res.Rcpts))
}

// Cancelling the context must interrupt an attempt that is already past the
// dial — the runner relies on this to stop work at shutdown.
func TestDeliver_ContextCancellationInterruptsStalledAttempt(t *testing.T) {
	relay := stalledRelay(t, []string{"220 stalled.invalid ESMTP"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := Deliver(ctx, relay, msg("a@x.com", "b@y.com"), Options{
		LocalName: "test.invalid",
		// Deliberately long, so only the cancellation can end this.
		ConnectTimeout: 30 * time.Second,
		DataTimeout:    30 * time.Second,
	})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Deliver took %v — cancellation did not interrupt the attempt", elapsed)
	}
	if res.Err == nil && len(res.Rcpts) == 0 {
		t.Fatal("expected an error after cancellation")
	}
	t.Logf("returned in %v with %v", elapsed, res.Err)
}

// The happy path must not trip the deadlines: a normal delivery well inside the
// budget still succeeds, and the DATA phase gets its own (longer) window.
func TestDeliver_DeadlinesDoNotBreakNormalDelivery(t *testing.T) {
	b := &fakeBackend{}
	relay := startRelay(t, b, "ok")

	res := Deliver(context.Background(), relay, msg("a@x.com", "b@y.com"), Options{
		LocalName:      "test.invalid",
		ConnectTimeout: 5 * time.Second,
		DataTimeout:    5 * time.Second,
	})

	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := res.Accepted(); len(got) != 1 || got[0] != "b@y.com" {
		t.Fatalf("accepted: got %v, want [b@y.com]", got)
	}
	if len(b.messages()) != 1 {
		t.Fatalf("relay received %d messages, want 1", len(b.messages()))
	}
	if !strings.Contains(res.Response, "2") {
		t.Errorf("unexpected end-of-DATA response %q", res.Response)
	}
}
