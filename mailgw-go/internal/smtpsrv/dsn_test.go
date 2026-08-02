package smtpsrv

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
)

// waitSpooled returns the envelopes as they were actually built, where
// waitQueued flattens them to the addresses and headers most tests care about.
// These assertions are about the fields themselves.
func (h *harness) waitSpooled(t *testing.T) []*queue.Envelope {
	t.Helper()
	select {
	case es := <-h.queued:
		return es
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for envelopes")
		return nil
	}
}

// dsnHarness starts a server with notifications enabled, which is the shipped
// default and what gates the DSN capability.
func dsnHarness(t *testing.T, tweak func(*config.Config)) *harness {
	t.Helper()
	return startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, _ *Backend) {
		cfg.Server.DSN.Enabled = true
		if tweak != nil {
			tweak(cfg)
		}
	})
}

// Matched on a line boundary rather than by substring: "DSN" appears inside
// plenty of other things, and a test that passes on a hostname containing it is
// not testing anything.
var dsnCap = regexp.MustCompile(`(?m)^250[- ]DSN\r?$`)

func TestDSN_AdvertisedWhenNotificationsAreEnabled(t *testing.T) {
	h := dsnHarness(t, nil)
	if caps := ehloCaps(t, dialClient(t, h.addr)); !dsnCap.MatchString(caps) {
		t.Errorf("EHLO does not advertise DSN:\n%s", caps)
	}
}

// With dsn.enabled off this gateway generates no notifications at all, so
// accepting NOTIFY= would be a promise made on the wire and never kept.
func TestDSN_NotAdvertisedWhenNotificationsAreDisabled(t *testing.T) {
	h := dsnHarness(t, func(cfg *config.Config) { cfg.Server.DSN.Enabled = false })

	c := dialClient(t, h.addr)
	c.greet()
	_, caps := c.cmd("EHLO probe.invalid")
	if dsnCap.MatchString(caps) {
		t.Errorf("EHLO advertises DSN with notifications disabled:\n%s", caps)
	}

	c.cmd("MAIL FROM:<a@example.com>")
	if code, raw := c.cmd("RCPT TO:<b@ngm.dev> NOTIFY=FAILURE"); code != 504 {
		t.Errorf("NOTIFY with DSN disabled = %d %q, want 504", code, raw)
	}
}

func TestDSN_ParametersAreAccepted(t *testing.T) {
	h := dsnHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	if code, raw := c.cmd("MAIL FROM:<a@example.com> RET=HDRS ENVID=batch42"); code != 250 {
		t.Errorf("MAIL with RET and ENVID = %d %q, want 250", code, raw)
	}
	if code, raw := c.cmd("RCPT TO:<b@ngm.dev> NOTIFY=FAILURE,DELAY ORCPT=rfc822;orig@ngm.dev"); code != 250 {
		t.Errorf("RCPT with NOTIFY and ORCPT = %d %q, want 250", code, raw)
	}
}

// go-smtp's validation, not this gateway's — but it is on this gateway's wire
// now, and a regression there would silently accept nonsense.
func TestDSN_MalformedParametersAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, mail, rcpt string }{
		{"NEVER with another keyword", "MAIL FROM:<a@example.com>",
			"RCPT TO:<b@ngm.dev> NOTIFY=NEVER,DELAY"},
		{"unknown NOTIFY keyword", "MAIL FROM:<a@example.com>",
			"RCPT TO:<b@ngm.dev> NOTIFY=MAYBE"},
		{"ORCPT with no address type", "MAIL FROM:<a@example.com>",
			"RCPT TO:<b@ngm.dev> ORCPT=orig@ngm.dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := dsnHarness(t, nil)
			c := dialClient(t, h.addr)
			c.greet()
			c.cmd("EHLO probe.invalid")
			c.cmd("%s", tc.mail)

			if code, raw := c.cmd("%s", tc.rcpt); code < 500 {
				t.Errorf("%s = %d %q, want a 5xx refusal", tc.rcpt, code, raw)
			}
		})
	}
}

func TestDSN_RETIsRefusedWhenMalformed(t *testing.T) {
	h := dsnHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	if code, raw := c.cmd("MAIL FROM:<a@example.com> RET=MAYBE"); code < 500 {
		t.Errorf("RET=MAYBE = %d %q, want a 5xx refusal", code, raw)
	}
}

// --- what reaches the spool ---

func TestDSN_ParametersReachTheQueuedEnvelope(t *testing.T) {
	h := dsnHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<a@example.com> RET=FULL ENVID=batch42")
	c.cmd("RCPT TO:<b@ngm.dev> NOTIFY=SUCCESS,FAILURE ORCPT=rfc822;orig@ngm.dev")
	c.cmd("DATA")
	c.sendBody("Subject: hi\r\n\r\nbody\r\n")

	envs := h.waitSpooled(t)
	if len(envs) != 1 {
		t.Fatalf("queued %d envelopes, want 1", len(envs))
	}
	env := envs[0]

	if env.DSNRet != "FULL" {
		t.Errorf("DSNRet = %q, want FULL", env.DSNRet)
	}
	if env.DSNEnvID != "batch42" {
		t.Errorf("DSNEnvID = %q, want batch42", env.DSNEnvID)
	}
	rc := env.Rcpts[0]
	if rc.OrigRcpt != "orig@ngm.dev" {
		t.Errorf("OrigRcpt = %q, want orig@ngm.dev", rc.OrigRcpt)
	}
	// go-smtp upper-cases the address type it parsed; the report lower-cases it
	// again on the way out.
	if !strings.EqualFold(rc.OrigRcptType, "rfc822") {
		t.Errorf("OrigRcptType = %q, want rfc822", rc.OrigRcptType)
	}
	if !rc.WantsSuccessDSN() || !rc.WantsFailureDSN() {
		t.Errorf("Notify = %v, want SUCCESS and FAILURE honoured", rc.Notify)
	}
	// FAILURE was named and DELAY was not, so the sender declined delay
	// warnings by omission.
	if rc.WantsDelayDSN() {
		t.Errorf("Notify = %v, want the delay warning suppressed", rc.Notify)
	}
}

// A message that says nothing about DSN must queue an envelope identical to the
// one it queued before any of this existed — which is what keeps a spool
// written by an older build interchangeable with one written by this one.
func TestDSN_SilentSenderLeavesNoTrace(t *testing.T) {
	h := dsnHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<a@example.com>")
	c.cmd("RCPT TO:<b@ngm.dev>")
	c.cmd("DATA")
	c.sendBody("Subject: hi\r\n\r\nbody\r\n")

	env := h.waitSpooled(t)[0]
	if env.DSNRet != "" || env.DSNEnvID != "" {
		t.Errorf("DSNRet=%q DSNEnvID=%q, want both empty", env.DSNRet, env.DSNEnvID)
	}
	rc := env.Rcpts[0]
	if rc.OrigRcpt != "" || rc.OrigRcptType != "" || rc.Notify != nil {
		t.Errorf("recipient carries DSN parameters nobody sent: %+v", rc)
	}
	// The RFC 3461 defaults, which are what this gateway did before it parsed
	// the parameter at all.
	if !rc.WantsFailureDSN() || !rc.WantsDelayDSN() || rc.WantsSuccessDSN() {
		t.Error("a silent sender did not get the RFC 3461 defaults")
	}
}

// RET and ENVID are MAIL parameters and describe one transaction only. A
// message that inherited the previous one's RET=FULL would have its bounce
// quote a body its sender never asked to have returned, and an inherited ENVID
// would identify the wrong message entirely.
//
// Two things currently prevent that — resetTxn clears the pair, and go-smtp
// hands Mail a non-nil *MailOptions every time so they are rewritten anyway —
// so removing either one alone leaves this green. It pins the behaviour, not
// the mechanism, which is the honest thing for it to claim.
func TestDSN_ParametersDoNotLeakBetweenTransactions(t *testing.T) {
	h := dsnHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	send := func(mail string) *queue.Envelope {
		t.Helper()
		c.cmd("%s", mail)
		c.cmd("RCPT TO:<b@ngm.dev>")
		c.cmd("DATA")
		c.sendBody("Subject: hi\r\n\r\nbody\r\n")
		return h.waitSpooled(t)[0]
	}

	if env := send("MAIL FROM:<a@example.com> RET=FULL ENVID=first"); env.DSNRet != "FULL" {
		t.Fatalf("first message DSNRet = %q, want FULL", env.DSNRet)
	}

	second := send("MAIL FROM:<a@example.com>")
	if second.DSNRet != "" {
		t.Errorf("second message inherited RET=%q from the first", second.DSNRet)
	}
	if second.DSNEnvID != "" {
		t.Errorf("second message inherited ENVID=%q from the first", second.DSNEnvID)
	}

	// And RSET clears them too, which is the other way a transaction ends.
	c.cmd("MAIL FROM:<a@example.com> RET=FULL ENVID=third")
	c.cmd("RSET")
	if env := send("MAIL FROM:<a@example.com>"); env.DSNRet != "" || env.DSNEnvID != "" {
		t.Errorf("RSET left DSNRet=%q DSNEnvID=%q behind", env.DSNRet, env.DSNEnvID)
	}
}

// NOTIFY is per recipient, so two recipients in one transaction must not share
// one another's answer.
func TestDSN_NotifyIsPerRecipient(t *testing.T) {
	h := dsnHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<a@example.com>")
	c.cmd("RCPT TO:<quiet@ngm.dev> NOTIFY=NEVER")
	c.cmd("RCPT TO:<loud@ngm.dev> NOTIFY=FAILURE,DELAY")
	c.cmd("DATA")
	c.sendBody("Subject: hi\r\n\r\nbody\r\n")

	env := h.waitSpooled(t)[0]
	if len(env.Rcpts) != 2 {
		t.Fatalf("envelope holds %d recipients, want 2", len(env.Rcpts))
	}

	byAddr := map[string]queue.Recipient{}
	for _, rc := range env.Rcpts {
		byAddr[rc.Addr] = rc
	}
	if byAddr["quiet@ngm.dev"].WantsFailureDSN() {
		t.Error("the NEVER recipient would still be sent a failure report")
	}
	if !byAddr["loud@ngm.dev"].WantsFailureDSN() {
		t.Error("the FAILURE recipient would not be sent one")
	}
	// One envelope, not two: these fields travel on the recipient and must not
	// become part of the bucketing key, which would multiply relay
	// transactions for no delivery-visible difference.
	if len(env.Rcpts) != 2 {
		t.Error("differing NOTIFY parameters split the envelope")
	}
}
