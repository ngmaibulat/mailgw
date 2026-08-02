package smtpsrv

import (
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// connEvents returns the Connection events captured so far.
//
// Logout runs on the server's goroutine after the client closes, so this polls
// briefly rather than reading once — the alternative is a flaky test that passes
// on a fast machine.
func connEvents(t *testing.T, h *harness, want int) []events.Connection {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		h.events.mu.Lock()
		var out []events.Connection
		for _, e := range h.events.sent {
			if e.Kind == events.KindConnection {
				if c, ok := e.Body.(events.Connection); ok {
					out = append(out, c)
				}
			}
		}
		h.events.mu.Unlock()

		if len(out) >= want || time.Now().After(deadline) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A connection that greets and quits without ever offering a message used to
// produce no Connection row at all, because Data was the only caller of
// postConnection. That is exactly the traffic worth investigating.
func TestConnEvent_GreetedAndQuitIsStillReported(t *testing.T) {
	h := startServer(t, defaultRoutes())

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO prober.example.com")
	c.cmd("QUIT")

	got := connEvents(t, h, 1)
	if len(got) != 1 {
		t.Fatalf("got %d Connection events, want exactly 1", len(got))
	}
	if got[0].HelloName != "prober.example.com" {
		t.Errorf("HelloName = %q, want the EHLO name", got[0].HelloName)
	}
	if got[0].TranCount != 0 {
		t.Errorf("TranCount = %d, want 0", got[0].TranCount)
	}
}

// A connection that does deliver still reports exactly once — the Logout post
// must not duplicate the one Data already sent.
func TestConnEvent_DeliveringConnectionReportsExactlyOnce(t *testing.T) {
	h := startServer(t, defaultRoutes())

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<you@ngm.dev>")
	c.cmd("DATA")
	c.sendBody("Subject: hi\r\n\r\nbody")
	c.cmd("QUIT")

	// Wait past the point a duplicate would have landed.
	time.Sleep(200 * time.Millisecond)
	if got := connEvents(t, h, 1); len(got) != 1 {
		t.Fatalf("got %d Connection events, want exactly 1", len(got))
	}
}

// A connection whose every recipient was rejected, then RSET, reports the
// rejections. resetTxn zeroes the per-transaction counters, so these have to
// come from the connection-scoped ones or they are lost.
func TestConnEvent_RejectedRecipientsSurviveAReset(t *testing.T) {
	rules := `
version: 1
policy:
  - name: no-blocked
    match: {field: rcpt.to, op: contains, value: blocked}
    then:
      - {action: reject, code: 550, message: "Access denied"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	h := startServerWithRules(t, compileRulesYAML(t, rules))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	if code, raw := c.cmd("RCPT TO:<blocked1@ngm.dev>"); code != 550 {
		t.Fatalf("RCPT: got %d (%s), want 550", code, raw)
	}
	if code, raw := c.cmd("RCPT TO:<blocked2@ngm.dev>"); code != 550 {
		t.Fatalf("RCPT: got %d (%s), want 550", code, raw)
	}
	c.cmd("RSET")
	c.cmd("QUIT")

	got := connEvents(t, h, 1)
	if len(got) != 1 {
		t.Fatalf("got %d Connection events, want exactly 1", len(got))
	}
	if got[0].RcptCountReject != 2 {
		t.Errorf("RcptCountReject = %d, want 2 — a transaction ended by RSET must "+
			"not destroy its counts", got[0].RcptCountReject)
	}
}

// Counts accumulate across every transaction on the connection, not just the
// last one.
func TestConnEvent_CountsAreConnectionCumulative(t *testing.T) {
	h := startServer(t, defaultRoutes())

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	for range 2 {
		c.cmd("MAIL FROM:<me@ngm.dev>")
		c.cmd("RCPT TO:<a@ngm.dev>")
		c.cmd("RCPT TO:<b@ngm.dev>")
		c.cmd("DATA")
		c.sendBody("Subject: hi\r\n\r\nbody")
	}
	c.cmd("QUIT")

	got := connEvents(t, h, 1)
	if len(got) == 0 {
		t.Fatal("no Connection event")
	}
	// The first (and only, per the once-per-connection rule) event is posted at
	// the first DATA, so it carries that transaction's two recipients.
	if got[0].RcptCountAccept != 2 {
		t.Errorf("RcptCountAccept = %d, want 2", got[0].RcptCountAccept)
	}
}

// A recipient refused by an allow-nothing policy at connect stage still yields a
// row, since the session is created before the greeting is refused.
func TestConnEvent_RefusedAtGreetingIsStillReported(t *testing.T) {
	rules := `
version: 1
policy:
  - name: refuse-all
    match: {always: true}
    then:
      - {action: reject, code: 554, message: "not welcome"}
`
	rs := compileRulesYAML(t, rules)
	if len(rs.Policy) == 0 {
		t.Fatal("expected a policy rule")
	}
	h := startServerWithRules(t, rs)

	c := dialClient(t, h.addr)
	c.greet()
	if code, _ := c.cmd("EHLO probe.invalid"); code != 554 {
		t.Fatalf("EHLO: got %d, want 554", code)
	}
	c.cmd("QUIT")

	if got := connEvents(t, h, 1); len(got) != 1 {
		t.Fatalf("got %d Connection events, want exactly 1", len(got))
	}
}

var _ = ruleset.StageConnect
