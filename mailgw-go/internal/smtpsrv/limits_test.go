package smtpsrv

import (
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
)

// limited starts a harness with the message limits tweaked. The default harness
// leaves received_headers and header_lines at zero — unlimited — which is what
// every pre-existing test in this package runs on.
func limited(t *testing.T, tweak func(*config.Limits)) *harness {
	t.Helper()
	return startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, _ *Backend) {
		tweak(&cfg.Server.Max)
	})
}

// offer runs one transaction and returns the reply to end-of-DATA.
func offer(t *testing.T, h *harness, body string) (int, string) {
	t.Helper()
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<you@ngm.dev>")
	if code, raw := c.cmd("DATA"); code != 354 {
		t.Fatalf("DATA: got %d (%s), want 354", code, raw)
	}
	return c.sendBody(body)
}

// spoolIsEmpty asserts a refused message left nothing behind. Every refusal
// below happens after the body is already on disk, so the cleanup is part of the
// contract rather than incidental.
func spoolIsEmpty(t *testing.T, h *harness) {
	t.Helper()
	entries, err := h.spool.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("refused message left %d spool entries behind", len(entries))
	}
}

// A message over max.bytes must be refused permanently. It used to collapse into
// the same 451 a disk failure produces, so the sender retried it for the whole
// queue lifetime and could never succeed.
func TestLimits_OversizeMessageIsRefusedPermanently(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.Bytes = 2048 })

	// Many short lines, not one long one: the harness also sets line_length 512,
	// and a single 8 KiB line trips THAT limit first, which is a different error
	// and a different reply.
	code, raw := offer(t, h, "Subject: big\r\n\r\n"+strings.Repeat("a line of filler\r\n", 500))

	if code != 552 {
		t.Fatalf("end of DATA: got %d (%s), want 552 — a 4xx tells the sender to retry "+
			"a message that can never succeed", code, raw)
	}
	if !strings.Contains(raw, "max.bytes") {
		t.Errorf("reply %q should name the limit that refused it", raw)
	}
	if got := h.metrics.MsgRejected.Load(); got != 1 {
		t.Errorf("MsgRejected = %d, want 1 — the refusal must go through refuse()", got)
	}
	spoolIsEmpty(t, h)
}

// A message under the limit is unaffected by it.
func TestLimits_UnderMaxBytesIsAccepted(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.Bytes = 1 << 20 })

	if code, raw := offer(t, h, "Subject: small\r\n\r\nhello"); code != 250 {
		t.Fatalf("end of DATA: got %d (%s), want 250", code, raw)
	}
}

// max.received_headers is the mail-loop guard RFC 5321 §6.3 asks for. It was
// configured, defaulted, and read by nothing.
func TestLimits_TooManyReceivedHeadersIsRefused(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.ReceivedHeaders = 3 })

	body := strings.Repeat("Received: from a.example.com by b.example.com; today\r\n", 5) +
		"Subject: looping\r\n\r\nround and round"

	code, raw := offer(t, h, body)

	if code != 554 {
		t.Fatalf("end of DATA: got %d (%s), want 554", code, raw)
	}
	if got := h.metrics.MsgLoopRejected.Load(); got != 1 {
		t.Errorf("MsgLoopRejected = %d, want 1", got)
	}
	if got := h.metrics.MsgRejected.Load(); got != 1 {
		t.Errorf("MsgRejected = %d, want 1 — a loop rejection is a subset of it", got)
	}
	spoolIsEmpty(t, h)
}

// The count includes the trace header this hop prepends, which is what a hop
// limit means — so a message arriving with exactly the limit already used is
// refused rather than relayed with one more.
func TestLimits_ReceivedCountIncludesOurOwnHeader(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.ReceivedHeaders = 2 })

	body := strings.Repeat("Received: from a.example.com by b.example.com; today\r\n", 2) +
		"Subject: two plus ours\r\n\r\nbody"

	if code, raw := offer(t, h, body); code != 554 {
		t.Fatalf("end of DATA: got %d (%s), want 554 — two received headers plus "+
			"our own is three, over a limit of two", code, raw)
	}
}

// One under the limit still gets through, so the guard is off-by-one safe.
func TestLimits_ReceivedHeadersUnderTheLimitIsAccepted(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.ReceivedHeaders = 3 })

	body := "Received: from a.example.com by b.example.com; today\r\n" +
		"Subject: one hop\r\n\r\nbody"

	if code, raw := offer(t, h, body); code != 250 {
		t.Fatalf("end of DATA: got %d (%s), want 250", code, raw)
	}
}

// max.header_lines was the other dead key. It is enforced in the body scan,
// which is the only thing that sees the header block stream past.
func TestLimits_TooManyHeaderLinesIsRefused(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.HeaderLines = 10 })

	var sb strings.Builder
	for i := range 50 {
		sb.WriteString("X-Filler-")
		sb.WriteString(strings.Repeat("a", 3))
		sb.WriteString(": ")
		sb.WriteString(strings.Repeat("b", i%7+1))
		sb.WriteString("\r\n")
	}
	sb.WriteString("Subject: wide\r\n\r\nbody")

	code, raw := offer(t, h, sb.String())

	if code != 552 {
		t.Fatalf("end of DATA: got %d (%s), want 552", code, raw)
	}
	if !strings.Contains(raw, "max.header_lines") {
		t.Errorf("reply %q should name the limit that refused it", raw)
	}
	spoolIsEmpty(t, h)
}

// A body with many lines but a short header block must NOT trip the header
// limit — the count is re-taken against the truncated block once the terminator
// is found, so body lines are not charged to the header.
func TestLimits_HeaderLineLimitDoesNotCountBodyLines(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.HeaderLines = 10 })

	body := "Subject: narrow header\r\n\r\n" + strings.Repeat("a line of body\r\n", 500)

	if code, raw := offer(t, h, body); code != 250 {
		t.Fatalf("end of DATA: got %d (%s), want 250 — body lines are not header lines",
			code, raw)
	}
}

// go-smtp answers an over-long COMMAND line itself (server.go:192), but during
// DATA its line-limit reader sits underneath the reader handed to Data, so
// smtp.ErrTooLongLine reaches us — and used to become a 451, telling the sender
// to retry a message whose format can never become acceptable.
//
// M10 predicted this could not happen and asked for it to be pinned by test
// rather than handled. The test is what found otherwise; this is now the pin on
// the fix instead.
func TestLimits_OverLongLineIsRefusedPermanently(t *testing.T) {
	h := limited(t, func(m *config.Limits) { m.LineLength = 512 })

	code, raw := offer(t, h, "Subject: long\r\n\r\n"+strings.Repeat("x", 4096))

	if code != 500 {
		t.Fatalf("end of DATA: got %d (%s), want 500 — an over-long line is a "+
			"permanent format error, not something to retry", code, raw)
	}
	if !strings.Contains(raw, "max.line_length") {
		t.Errorf("reply %q should name the limit that refused it", raw)
	}
	spoolIsEmpty(t, h)
}
