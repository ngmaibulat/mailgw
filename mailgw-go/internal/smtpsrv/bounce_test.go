package smtpsrv

import (
	"io"
	"slices"
	"strings"
	"testing"
)

// A recipient can be refused after the whole message has been accepted, because
// a data-stage rule cannot run until the body has arrived and the RCPT reply is
// long spent. Those recipients used to be dropped with a WARN and nobody was
// told; now the permanent ones produce a notification back to the sender.
//
// The distinction between permanent and temporary is the point of these tests.

// Two recipients: one rejected 5xx by a data-stage rule, one routed normally.
// The message is still queued for the survivor, and the sender hears about the
// other.
const rulesRejectOneRecipientAtData = `
version: 1
policy:
  - name: block-finance-secrets
    match:
      all:
        - {field: rcpt.domain, op: eq, value: finance.example}
        - {field: header.subject, op: contains, value: secret}
    then:
      - {action: reject, code: 550, enhanced: "5.7.1", message: "blocked by policy"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

func TestBounce_PermanentDataStageRefusalNotifiesTheSender(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesRejectOneRecipientAtData))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	if code, raw := c.cmd("RCPT TO:<blocked@finance.example>"); code != 250 {
		t.Fatalf("RCPT: got %d %q, want 250 — the rule under test needs DATA", code, raw)
	}
	if code, raw := c.cmd("RCPT TO:<fine@partner.com>"); code != 250 {
		t.Fatalf("RCPT: got %d %q, want 250", code, raw)
	}
	c.cmd("DATA")

	code, raw := c.sendBody("Subject: secret plans\r\n\r\nbody\r\n")
	// The message was accepted: one recipient is deliverable, and the refusal of
	// the other has no reply left to travel in. That is exactly why it bounces.
	if code != 250 {
		t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
	}

	if n := h.bounces.len(); n != 1 {
		t.Fatalf("got %d notifications, want 1", n)
	}
	got := h.bounces.addrs()
	if !slices.Equal(got, []string{"blocked@finance.example"}) {
		t.Errorf("notification names %v, want only the refused recipient", got)
	}

	// The deliverable recipient still got their mail.
	select {
	case envs := <-h.queued:
		var addrs []string
		for _, e := range envs {
			addrs = append(addrs, e.RcptAddrs()...)
		}
		if !slices.Equal(addrs, []string{"fine@partner.com"}) {
			t.Errorf("queued for %v, want only the surviving recipient", addrs)
		}
	default:
		t.Error("nothing was queued; the surviving recipient lost their message")
	}
}

// The notification must say which rule refused the recipient. "rejected by rule
// block-finance-secrets" is something an operator can act on; "rejected" is not.
func TestBounce_NotificationNamesTheRuleThatRefused(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesRejectOneRecipientAtData))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<blocked@finance.example>")
	c.cmd("RCPT TO:<fine@partner.com>")
	c.cmd("DATA")
	c.sendBody("Subject: secret plans\r\n\r\nbody\r\n")

	if h.bounces.len() != 1 {
		t.Fatalf("got %d notifications, want 1", h.bounces.len())
	}
	rc := h.bounces.req[0].Rcpts[0]
	if !strings.Contains(rc.Diagnostic, "block-finance-secrets") {
		t.Errorf("diagnostic %q does not name the rule that refused the recipient", rc.Diagnostic)
	}
	if rc.Status != "5.7.1" {
		t.Errorf("status = %q, want the rule's enhanced code 5.7.1", rc.Status)
	}
}

// A 4xx refusal must NOT bounce. The default action for a recipient no rule
// routed is a 451 (preserving Haraka's DENYSOFT at npRoute.js:65), so bouncing
// temporary refusals would turn a gap in the routing configuration into
// permanent rejections for mail that would deliver fine once it was noticed.
const rulesTempfailOneRecipientAtData = `
version: 1
policy:
  - name: defer-finance-secrets
    match:
      all:
        - {field: rcpt.domain, op: eq, value: finance.example}
        - {field: header.subject, op: contains, value: secret}
    then:
      - {action: tempfail, code: 451, message: "try again later"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

func TestBounce_TemporaryDataStageRefusalDoesNotNotify(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesTempfailOneRecipientAtData))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<deferred@finance.example>")
	c.cmd("RCPT TO:<fine@partner.com>")
	c.cmd("DATA")

	if code, raw := c.sendBody("Subject: secret plans\r\n\r\nbody\r\n"); code != 250 {
		t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
	}

	if n := h.bounces.len(); n != 0 {
		t.Errorf("got %d notifications, want 0 — a temporary refusal was turned into a permanent bounce: %v",
			n, h.bounces.addrs())
	}
}

// When no recipient survives, the refusal still goes back on the SMTP reply —
// nothing was accepted, so there is nothing to bounce and the sender is told
// synchronously, which is strictly better.
func TestBounce_NotSentWhenTheWholeMessageIsRefused(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesRejectOneRecipientAtData))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<blocked@finance.example>")
	c.cmd("DATA")

	code, raw := c.sendBody("Subject: secret plans\r\n\r\nbody\r\n")
	if code != 550 {
		t.Fatalf("end of DATA: got %d %q, want 550", code, raw)
	}
	if n := h.bounces.len(); n != 0 {
		t.Errorf("got %d notifications, want 0 — the client was already told on the reply", n)
	}
}

// The notification quotes the original headers, taken from the scan that already
// streamed past on the way to disk.
func TestBounce_NotificationQuotesTheOriginalHeaders(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesRejectOneRecipientAtData))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<blocked@finance.example>")
	c.cmd("RCPT TO:<fine@partner.com>")
	c.cmd("DATA")
	c.sendBody("Subject: secret plans\r\nX-Marker: findme\r\n\r\nthe body\r\n")

	if h.bounces.len() != 1 {
		t.Fatalf("got %d notifications, want 1", h.bounces.len())
	}
	req := h.bounces.req[0]
	if req.Original == nil {
		t.Fatal("the notification quotes nothing")
	}
	quoted, err := io.ReadAll(req.Original)
	if err != nil {
		t.Fatalf("read quoted original: %v", err)
	}
	if !strings.Contains(string(quoted), "X-Marker: findme") {
		t.Errorf("quoted original is missing the message's headers:\n%s", quoted)
	}
	// Headers only by default: the body is not returned to an address that did
	// not ask for it.
	if strings.Contains(string(quoted), "the body") {
		t.Errorf("quoted original includes the body, but the default is headers only:\n%s", quoted)
	}
}

// The synthetic envelope a session-side notification hangs off must carry the
// sender and a uuid under this transaction, or the Bouncer has nothing to
// address it to and no identity to nest it under.
func TestBounce_SyntheticEnvelopeCarriesTheTransactionIdentity(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesRejectOneRecipientAtData))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	c.cmd("RCPT TO:<blocked@finance.example>")
	c.cmd("RCPT TO:<fine@partner.com>")
	c.cmd("DATA")
	_, raw := c.sendBody("Subject: secret plans\r\n\r\nbody\r\n")

	txn := uuidScraper.FindStringSubmatch(raw)
	if len(txn) < 2 {
		t.Fatalf("no transaction id in the reply %q", raw)
	}

	env := h.bounces.req[0].Env
	if env.MailFrom != "sender@example.com" {
		t.Errorf("notification envelope sender = %q, want the original sender", env.MailFrom)
	}
	if !strings.HasPrefix(env.UUID, txn[1]) {
		t.Errorf("notification uuid %q does not extend the transaction %q", env.UUID, txn[1])
	}
}
