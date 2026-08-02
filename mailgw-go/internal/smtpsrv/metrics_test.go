package smtpsrv

import (
	"net"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// These pin the unit boundaries the counters promise. A message, a recipient
// and an envelope are three different things, and every bug this file is likely
// to catch is one of them being counted as another.

func wantCounts(t *testing.T, m *obs.Metrics, want map[string]int64) {
	t.Helper()
	got := m.Snapshot()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
}

func TestMetrics_SuccessfulMessage(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)

	code, _ := c.send("me@ngm.dev", []string{"a@ngm.dev", "b@ngm.dev"}, "Subject: hi\r\n\r\nbody\r\n")
	if code != 250 {
		t.Fatalf("end of DATA = %d, want 250", code)
	}
	h.waitQueued(t)

	wantCounts(t, h.metrics, map[string]int64{
		"msg_accepted":   1,
		"rcpt_accepted":  2, // per recipient, not per message
		"env_queued":     1, // both recipients share one relay group
		"msg_rejected":   0,
		"rcpt_rejected":  0,
		"rcpt_discarded": 0,
	})
	if got := h.metrics.Snapshot()["bytes_in"]; got <= 0 {
		t.Errorf("bytes_in = %d, want the spooled size", got)
	}
}

func TestMetrics_PolicyRejectCountsOnce(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "refuse everything", Stage: "data",
			Match: ruleset.Pred{All: []ruleset.Pred{}},
			Then: []ruleset.Action{{Kind: ruleset.ActReject, Code: 550,
				Message: "Not today"}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))
	c := dialClient(t, h.addr)

	code, _ := c.send("me@ngm.dev", []string{"a@ngm.dev"}, "Subject: hi\r\n\r\nbody\r\n")
	if code != 550 {
		t.Fatalf("end of DATA = %d, want 550", code)
	}

	wantCounts(t, h.metrics, map[string]int64{
		"msg_rejected":   1,
		"msg_accepted":   0,
		"msg_tempfailed": 0,
		"env_queued":     0,
	})
	// The body still crossed the wire and was spooled to be scanned, so the
	// byte counter moves even though nothing was relayed.
	if got := h.metrics.Snapshot()["bytes_in"]; got <= 0 {
		t.Errorf("bytes_in = %d, want the bytes we received before refusing", got)
	}
}

// The unit-confusion guard: refusing one recipient of two does not refuse the
// message, and the message is still accepted for the other.
func TestMetrics_RecipientRejectIsNotAMessageReject(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "no root",
			Match: ruleset.Pred{Field: "rcpt.local", Op: ruleset.OpEq, Value: "root"},
			Then: []ruleset.Action{{Kind: ruleset.ActReject, Code: 550,
				Message: "No such user"}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))
	c := dialClient(t, h.addr)

	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	if code, _ := c.cmd("RCPT TO:<root@ngm.dev>"); code != 550 {
		t.Fatalf("RCPT for root = %d, want 550", code)
	}
	if code, _ := c.cmd("RCPT TO:<ok@ngm.dev>"); code != 250 {
		t.Fatalf("RCPT for ok = %d, want 250", code)
	}
	c.cmd("DATA")
	if code, _ := c.sendBody("Subject: hi\r\n\r\nbody\r\n"); code != 250 {
		t.Fatal("the message should still be accepted for the surviving recipient")
	}
	h.waitQueued(t)

	wantCounts(t, h.metrics, map[string]int64{
		"rcpt_rejected": 1,
		"rcpt_accepted": 1,
		"msg_rejected":  0, // a recipient refusal is not a message refusal
		"msg_accepted":  1,
		"env_queued":    1,
	})
}

// msg_accepted is documented as a superset of msg_discarded, and a console that
// treats them as disjoint buckets will be wrong. Pin it.
func TestMetrics_DiscardedMessageIsStillAccepted(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "blackhole", Stage: "data",
			Match: ruleset.Pred{All: []ruleset.Pred{}},
			Then:  []ruleset.Action{{Kind: ruleset.ActDiscard}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))
	c := dialClient(t, h.addr)

	code, msg := c.send("me@ngm.dev", []string{"a@ngm.dev"}, "Subject: hi\r\n\r\nbody\r\n")
	if code != 250 {
		t.Fatalf("end of DATA = %d (%s), want 250 — a discard is accepted, not refused", code, msg)
	}

	wantCounts(t, h.metrics, map[string]int64{
		"msg_accepted":   1, // still 250, so still accepted
		"msg_discarded":  1,
		"rcpt_discarded": 1,
		"env_queued":     0, // and nothing was queued
		"msg_rejected":   0,
	})
}

// A message-scoped quarantine files an envelope rather than queueing one, and
// the counter must agree with what LenAll finds on disk.
func TestMetrics_QuarantineFilesAnEnvelope(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "hold everything", Stage: "data",
			Match: ruleset.Pred{All: []ruleset.Pred{}},
			Then:  []ruleset.Action{{Kind: ruleset.ActQuarantine}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))
	c := dialClient(t, h.addr)

	if code, _ := c.send("me@ngm.dev", []string{"a@ngm.dev"}, "Subject: hi\r\n\r\nbody\r\n"); code != 250 {
		t.Fatalf("end of DATA = %d, want 250", code)
	}

	wantCounts(t, h.metrics, map[string]int64{
		"msg_accepted":    1,
		"msg_quarantined": 1,
		"env_quarantined": 1,
		"env_queued":      0,
	})

	counts, err := h.spool.LenAll()
	if err != nil {
		t.Fatalf("LenAll: %v", err)
	}
	if counts.Quarantine != 1 || counts.Ready != 0 {
		t.Errorf("spool = %+v, want one quarantined and nothing ready", counts)
	}
}

// The defect this milestone fixed: `discard` and `quarantine` are only
// compile-rejected BEFORE mail, so a rule carrying `stage: mail` reached
// applyTerminal with one — and fell through to "accept and relay", the exact
// opposite of what the rule asked for.
func TestMailStageDiscardIsHonoured(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "drop mail from spammer", Stage: "mail",
			Match: ruleset.Pred{Field: "mail.from", Op: ruleset.OpEq, Value: "spammer@bad.example"},
			Then:  []ruleset.Action{{Kind: ruleset.ActDiscard}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))
	c := dialClient(t, h.addr)

	code, _ := c.send("spammer@bad.example", []string{"a@ngm.dev"}, "Subject: hi\r\n\r\nbody\r\n")
	if code != 250 {
		t.Fatalf("end of DATA = %d, want 250 — a discard is accepted", code)
	}

	counts, err := h.spool.LenAll()
	if err != nil {
		t.Fatalf("LenAll: %v", err)
	}
	if counts.Ready != 0 {
		t.Errorf("a stage:mail discard queued %d envelopes; it must relay nothing", counts.Ready)
	}
	wantCounts(t, h.metrics, map[string]int64{
		"msg_accepted":  1,
		"msg_discarded": 1,
		"env_queued":    0,
	})
}

// The same hole on the quarantine side.
func TestMailStageQuarantineIsHonoured(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "hold mail from stranger", Stage: "mail",
			Match: ruleset.Pred{Field: "mail.from", Op: ruleset.OpEq, Value: "who@unknown.example"},
			Then:  []ruleset.Action{{Kind: ruleset.ActQuarantine}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))
	c := dialClient(t, h.addr)

	if code, _ := c.send("who@unknown.example", []string{"a@ngm.dev"}, "Subject: hi\r\n\r\nbody\r\n"); code != 250 {
		t.Fatal("a quarantine is accepted, not refused")
	}

	counts, err := h.spool.LenAll()
	if err != nil {
		t.Fatalf("LenAll: %v", err)
	}
	if counts.Ready != 0 || counts.Quarantine != 1 {
		t.Errorf("spool = %+v, want nothing ready and one quarantined", counts)
	}
}

// A message-scoped drop must not leak into the next transaction on the same
// connection — msgDrop is per-transaction state.
func TestMailStageDiscardDoesNotLeakToTheNextMessage(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "drop mail from spammer", Stage: "mail",
			Match: ruleset.Pred{Field: "mail.from", Op: ruleset.OpEq, Value: "spammer@bad.example"},
			Then:  []ruleset.Action{{Kind: ruleset.ActDiscard}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))
	c := dialClient(t, h.addr)

	c.greet()
	c.cmd("EHLO test.example.com")

	c.cmd("MAIL FROM:<spammer@bad.example>")
	c.cmd("RCPT TO:<a@ngm.dev>")
	c.cmd("DATA")
	c.sendBody("Subject: dropped\r\n\r\nbody\r\n")

	c.cmd("MAIL FROM:<honest@ngm.dev>")
	c.cmd("RCPT TO:<a@ngm.dev>")
	c.cmd("DATA")
	if code, _ := c.sendBody("Subject: kept\r\n\r\nbody\r\n"); code != 250 {
		t.Fatalf("second message = %d, want 250", code)
	}
	h.waitQueued(t)

	wantCounts(t, h.metrics, map[string]int64{
		"msg_accepted":  2,
		"msg_discarded": 1, // only the first
		"env_queued":    1, // only the second
	})
}

// The allowlist listener is the only place in the process that sees a peer
// before the banner, so it owns both connection counters.
func TestMetrics_DenialCountsDeniedNotAccepted(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	m := obs.New()
	list := allowlistFrom(t, `{"allowed":["10.0.0.1"]}`) // not loopback
	denied := make(chan string, 4)
	l := &allowlistListener{
		Listener: inner,
		allowed:  func() *config.Allowlist { return list },
		log:      quietLogger(),
		metrics:  m,
		onDeny:   func(a string) { denied <- a },
	}
	// Accept blocks until a PERMITTED peer arrives, which never happens here,
	// so it runs in the background and onDeny is what the test synchronises on.
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	conn, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case <-denied:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the denial")
	}

	wantCounts(t, m, map[string]int64{
		"conn_denied":   1,
		"conn_accepted": 0,
	})
}

func TestMetrics_AcceptCountsAcceptedNotDenied(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	m := obs.New()
	list := allowlistFrom(t, `{"allowed":["127.0.0.1","::1"]}`)
	accepted := make(chan struct{}, 4)
	l := &allowlistListener{
		Listener: inner,
		allowed:  func() *config.Allowlist { return list },
		log:      quietLogger(),
		metrics:  m,
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			accepted <- struct{}{}
		}
	}()

	conn, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the connection to be accepted")
	}

	wantCounts(t, m, map[string]int64{
		"conn_accepted": 1,
		"conn_denied":   0,
	})
}
