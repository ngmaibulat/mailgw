package smtpsrv

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// stubDNS is the same shape internal/msgauth's own tests use: one map-backed
// resolver serving SPF, DKIM and DMARC, so nothing here touches the network.
type stubDNS struct{ txt map[string][]string }

func (s stubDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := s.txt[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (s stubDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (s stubDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (s stubDNS) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
}

// authDNS publishes the records these tests check against. 127.0.0.1 is what a
// loopback client connects from, so an SPF record naming it is a pass and one
// that does not is a fail.
func authDNS() stubDNS {
	return stubDNS{txt: map[string][]string{
		"ngm.dev":             {"v=spf1 ip4:127.0.0.1 ip6:::1 -all"},
		"evil.example":        {"v=spf1 -all"},
		"_dmarc.ngm.dev":      {"v=DMARC1; p=reject"},
		"_dmarc.evil.example": {"v=DMARC1; p=quarantine"},
	}}
}

// msgAuthHarness starts a server with message authentication on.
func msgAuthHarness(t *testing.T, rules string, tweak func(*config.MsgAuthConfig)) *harness {
	t.Helper()
	return startServerTuned(t, compileRulesYAML(t, rules), func(cfg *config.Config, b *Backend) {
		m := config.MsgAuthConfig{
			SPF: config.MsgAuthCheck{Enabled: true}, MaxDKIMSignatures: 10,
			DNSTimeout: config.Duration(time.Second),
		}
		if tweak != nil {
			tweak(&m)
		}
		cfg.Server.MsgAuth = m
		b.MsgAuth = &MsgAuth{
			Resolver:          authDNS(),
			AuthservID:        m.AuthservIDFor(cfg.Server.Hostname),
			SPF:               m.SPF.Enabled,
			DKIM:              m.DKIM.Enabled,
			DMARC:             m.DMARC.Enabled,
			MaxDKIMSignatures: m.MaxDKIMSignatures,
		}
	})
}

// deliver drives one message through and returns the envelopes it produced.
func deliver(t *testing.T, h *harness, from, rcpt, body string) []*queue.Envelope {
	t.Helper()
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	c.cmd("MAIL FROM:<%s>", from)
	c.cmd("RCPT TO:<%s>", rcpt)
	c.cmd("DATA")
	c.sendBody(body)

	select {
	case es := <-h.queued:
		return es
	case <-time.After(5 * time.Second):
		t.Fatal("message was never queued")
		return nil
	}
}

func spooled(t *testing.T, h *harness, e *queue.Envelope) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.spool.Root(), "data", e.Body))
	if err != nil {
		t.Fatalf("read spooled body: %v", err)
	}
	return string(raw)
}

// prepended renders what Envelope.PrependBlock will put ahead of the body at
// delivery — which is where the message-authentication headers live.
func prepended(e *queue.Envelope) string { return e.PrependBlock() }

const relayAll = `
version: 1
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

// TestMsgAuth_AddsHeadersAtDelivery pins where the results end up.
//
// They are NOT in the spooled body: receivedHeader() is written before DATA is
// read, so the DKIM and DMARC results do not exist yet. They ride on txnHeaders
// — the same list an add_header rule feeds — which puts them ABOVE this
// gateway's own Received: at delivery, the ordering RFC 7601 wants.
func TestMsgAuth_AddsHeadersAtDelivery(t *testing.T) {
	h := msgAuthHarness(t, relayAll, nil)
	es := deliver(t, h, "alice@ngm.dev", "bob@ngm.dev", "Subject: hi\r\n\r\nbody")

	block := prepended(es[0])
	if !strings.Contains(block, "Authentication-Results: devbook.local; spf=pass") {
		t.Errorf("no Authentication-Results in the prepend block:\n%s", block)
	}
	if !strings.Contains(block, "Received-SPF: pass") {
		t.Errorf("no Received-SPF in the prepend block:\n%s", block)
	}

	// Above our own Received:, which is the first line of the spooled body.
	body := spooled(t, h, es[0])
	if strings.Contains(body, "Authentication-Results:") {
		t.Error("the results were baked into the spooled body; they belong on the envelope")
	}
	if !strings.HasPrefix(body, "Received: from") {
		t.Errorf("the spooled body should still begin with our Received header:\n%s", body)
	}
}

func TestMsgAuth_SPFFailIsRecorded(t *testing.T) {
	h := msgAuthHarness(t, relayAll, nil)
	es := deliver(t, h, "mallory@evil.example", "bob@ngm.dev", "Subject: hi\r\n\r\nbody")

	block := prepended(es[0])
	if !strings.Contains(block, "spf=fail") {
		t.Errorf("expected spf=fail:\n%s", block)
	}
	if got := h.metrics.SPFFailed.Load(); got != 1 {
		t.Errorf("spf_failed = %d, want 1", got)
	}
	if got := h.metrics.SPFChecked.Load(); got != 1 {
		t.Errorf("spf_checked = %d, want 1", got)
	}
}

// TestMsgAuth_SPFIsAMailStageFact: the whole reason SPF is registered at
// StageMail is that a rule reading it must be answerable on the sender's own
// MAIL line, before a megabyte of DATA arrives.
func TestMsgAuth_SPFIsAMailStageFact(t *testing.T) {
	const rules = `
version: 1
policy:
  - name: refuse spf failures
    match: {field: spf.result, op: eq, value: fail}
    then:
      - {action: reject, code: 550, enhanced: "5.7.23", message: "SPF fail"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	h := msgAuthHarness(t, rules, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")

	code, msg := c.cmd("MAIL FROM:<mallory@evil.example>")
	if code != 550 {
		t.Fatalf("MAIL FROM: got %d %q, want 550 on the MAIL line itself", code, msg)
	}
	if !strings.Contains(msg, "SPF fail") {
		t.Errorf("refusal does not carry the rule's message: %q", msg)
	}

	// A sender whose SPF passes is unaffected.
	if code, _ := c.cmd("MAIL FROM:<alice@ngm.dev>"); code != 250 {
		t.Errorf("a passing sender was refused: %d", code)
	}
}

func TestMsgAuth_DMARCIsADataStageFact(t *testing.T) {
	const rules = `
version: 1
policy:
  - name: refuse dmarc failures under p=reject
    match:
      all:
        - {field: dmarc.result, op: eq, value: fail}
        - {field: dmarc.policy, op: eq, value: reject}
    then:
      - {action: reject, code: 550, enhanced: "5.7.1", message: "DMARC fail"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	h := msgAuthHarness(t, rules, func(m *config.MsgAuthConfig) {
		m.DKIM.Enabled = true
		m.DMARC.Enabled = true
	})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	// SPF passes for ngm.dev from loopback, but the From header claims a
	// different domain — so nothing authenticated is aligned and DMARC fails
	// under ngm.dev's p=reject.
	if code, _ := c.cmd("MAIL FROM:<bounce@evil.example>"); code != 250 {
		t.Fatal("MAIL should be accepted: dmarc is a data-stage fact")
	}
	c.cmd("RCPT TO:<bob@ngm.dev>")
	c.cmd("DATA")
	code, msg := c.sendBody("From: Alice <alice@ngm.dev>\r\nSubject: forged\r\n\r\nbody")
	if code != 550 || !strings.Contains(msg, "DMARC fail") {
		t.Fatalf("end of DATA: got %d %q, want 550 DMARC fail", code, msg)
	}
}

// TestMsgAuth_StripsForgedAuthResults is RFC 7601 §5: a sender must not be able
// to assert results under this gateway's own name.
func TestMsgAuth_StripsForgedAuthResults(t *testing.T) {
	h := msgAuthHarness(t, relayAll, nil)
	es := deliver(t, h, "alice@ngm.dev", "bob@ngm.dev",
		"Authentication-Results: devbook.local; spf=pass smtp.mailfrom=bank.example\r\n"+
			"Authentication-Results: upstream.partner.com; dkim=pass header.d=partner.com\r\n"+
			"Subject: forged\r\n\r\nbody")

	body := spooled(t, h, es[0])
	if strings.Contains(body, "bank.example") {
		t.Errorf("a forged field bearing our authserv-id was spooled:\n%s", body)
	}
	// A third party's results are somebody else's to make and are left alone.
	if !strings.Contains(body, "upstream.partner.com") {
		t.Errorf("a third party's Authentication-Results was removed:\n%s", body)
	}
}

// TestMsgAuth_OffIsByteForByteUnchanged is the regression floor for the whole
// milestone: with nothing configured and no rule asking, the DATA path must not
// differ from what it was before msgauth existed.
func TestMsgAuth_OffIsByteForByteUnchanged(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, relayAll))
	const raw = "Authentication-Results: devbook.local; spf=pass smtp.mailfrom=bank.example\r\n" +
		"Subject: untouched\r\n\r\nbody\r\n"
	es := deliver(t, h, "alice@ngm.dev", "bob@ngm.dev", raw)

	body := spooled(t, h, es[0])
	// Even a field forging our own name survives, because a gateway that adds
	// no Authentication-Results has no name to protect and no business
	// rewriting mail.
	if !strings.Contains(body, "bank.example") {
		t.Errorf("the DATA stream was filtered with msgauth off:\n%s", body)
	}
	if block := prepended(es[0]); strings.Contains(block, "Authentication-Results") {
		t.Errorf("a header was added with msgauth off:\n%s", block)
	}
	if got := h.metrics.SPFChecked.Load(); got != 0 {
		t.Errorf("spf_checked = %d with msgauth off, want 0", got)
	}
}

// TestMsgAuth_RulesTurnChecksOnByThemselves: an operator who writes a rule
// reading spf.* should not also have to remember msgauth.spf.enabled. The
// configuration flags exist for the other case — wanting the header without a
// rule to go with it.
func TestMsgAuth_RulesTurnChecksOnByThemselves(t *testing.T) {
	const rules = `
version: 1
routes:
  - name: spf-aware
    match: {field: spf.result, op: eq, value: pass}
    then:
      - {action: relay, relay: Outbound}
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	// Every configuration switch is OFF; only the rule asks.
	h := msgAuthHarness(t, rules, func(m *config.MsgAuthConfig) {
		m.SPF.Enabled = false
	})
	es := deliver(t, h, "alice@ngm.dev", "bob@ngm.dev", "Subject: hi\r\n\r\nbody")

	if got := h.metrics.SPFChecked.Load(); got != 1 {
		t.Fatalf("spf_checked = %d; a rule reading spf.* must turn the check on", got)
	}
	if !strings.Contains(prepended(es[0]), "spf=pass") {
		t.Error("the result was computed but not recorded in the header")
	}
}

// TestMsgAuth_TagSummarisesTheVerdict: tag.msgauth exists so an operator can
// branch on the outcome without repeating three predicates, and it is set before
// the data-stage pass that reads it — the TagAttachScan precedent.
//
// Note `stage: data`, exactly as TestAttach_TagIsReadableByRules needs it. The
// tag.* prefix is registered at StageConnect so that reading a tag never forces
// a rule to defer, which means a rule matching ONLY a tag would otherwise be
// inferred to connect and bound at RCPT, before the verdict exists. The verdict
// does not exist before the message does, so the stage has to be stated.
func TestMsgAuth_TagSummarisesTheVerdict(t *testing.T) {
	const rules = `
version: 1
routes:
  - name: authenticated mail
    stage: data
    match: {field: tag.msgauth, op: eq, value: spf_pass}
    then:
      - {action: add_header, name: X-Verdict, value: trusted}
      - {action: relay, relay: Outbound}
  - name: catch-all
    priority: 100
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	h := msgAuthHarness(t, rules, nil)
	es := deliver(t, h, "alice@ngm.dev", "bob@ngm.dev", "Subject: hi\r\n\r\nbody")
	if !strings.Contains(prepended(es[0]), "X-Verdict: trusted") {
		t.Errorf("tag.msgauth was not readable by the data-stage pass:\n%s", prepended(es[0]))
	}
}

// TestMsgAuth_ResetsBetweenTransactions: SPF is evaluated per MAIL, so a second
// message on the same connection must not inherit the first one's answer.
func TestMsgAuth_ResetsBetweenTransactions(t *testing.T) {
	h := msgAuthHarness(t, relayAll, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")

	send := func(from string) *queue.Envelope {
		t.Helper()
		c.cmd("MAIL FROM:<%s>", from)
		c.cmd("RCPT TO:<bob@ngm.dev>")
		c.cmd("DATA")
		c.sendBody("Subject: hi\r\n\r\nbody")
		select {
		case es := <-h.queued:
			return es[0]
		case <-time.After(5 * time.Second):
			t.Fatal("message was never queued")
			return nil
		}
	}

	if got := prepended(send("alice@ngm.dev")); !strings.Contains(got, "spf=pass") {
		t.Fatalf("first message: %s", got)
	}
	second := prepended(send("mallory@evil.example"))
	if !strings.Contains(second, "spf=fail") {
		t.Errorf("the second transaction inherited the first one's SPF result:\n%s", second)
	}
	if strings.Contains(second, "spf=pass") {
		t.Errorf("two SPF results on one message:\n%s", second)
	}
}

// TestMsgAuth_NoneReadsAsAbsentToARule pins the distinction the fields carry:
// "the check did not run" is not "the check ran and found nothing". A gateway
// with msgauth off must not match a rule looking for spf.result.
func TestMsgAuth_NoneReadsAsAbsentToARule(t *testing.T) {
	env := &ruleset.Env{Stage: ruleset.StageMail, Mail: &ruleset.MailEnv{From: "a@ngm.dev"}}
	if v := env.Lookup("spf.result"); v.OK {
		t.Errorf("spf.result is present (%+v) before any check ran", v)
	}
}

// TestMsgAuth_AuthservIDDefaultsToTheHostname: the DSNConfig.PostmasterFor
// precedent — a default that depends on another configurable value is resolved
// where that value is known.
func TestMsgAuth_AuthservIDDefaultsToTheHostname(t *testing.T) {
	h := msgAuthHarness(t, relayAll, func(m *config.MsgAuthConfig) { m.AuthservID = "" })
	es := deliver(t, h, "alice@ngm.dev", "bob@ngm.dev", "Subject: hi\r\n\r\nbody")
	if !strings.Contains(prepended(es[0]), "Authentication-Results: devbook.local;") {
		t.Errorf("authserv_id did not default to the hostname:\n%s", prepended(es[0]))
	}
}

// Compile-time proof that the stub satisfies the interface the production
// resolver does — so a change to msgauth.Resolver breaks here rather than in a
// test that silently stops exercising DNS.
var _ msgauth.Resolver = stubDNS{}
