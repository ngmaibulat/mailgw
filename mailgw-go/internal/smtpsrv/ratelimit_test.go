package smtpsrv

import (
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ratelimit"
)

// rateLimited starts a harness with rate limits in force.
// (limits_test.go already owns the name `limited`, for max.* rather than rates.)
//
// The limiter takes the real clock: these tests never need time to pass, only
// events to accumulate, so the window arithmetic is internal/ratelimit's problem
// and this file only checks the SMTP behaviour around it.
func rateLimited(t *testing.T, rules ratelimit.Rules) (*harness, *ratelimit.Limiter) {
	t.Helper()
	l := ratelimit.New(rules, nil)
	h := startServerTuned(t, compileRulesYAML(t, relayAll), func(_ *config.Config, b *Backend) {
		b.Limiter = func() *ratelimit.Limiter { return l }
	})
	return h, l
}

// TestRateLimit_SenderIsRefused450: a transaction-level refusal is 4xx, always.
// The whole milestone is designed against a limit set too low turning into
// permanently rejected mail.
func TestRateLimit_SenderIsRefused450(t *testing.T) {
	h, _ := rateLimited(t, ratelimit.Rules{
		MsgPerSender: ratelimit.Rule{Rate: 2, Per: time.Hour},
	})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")

	for i := range 2 {
		if code, msg := c.cmd("MAIL FROM:<busy@ngm.dev>"); code != 250 {
			t.Fatalf("message %d: got %d %q, want 250", i+1, code, msg)
		}
		c.cmd("RSET")
	}

	code, msg := c.cmd("MAIL FROM:<busy@ngm.dev>")
	if code != 450 {
		t.Fatalf("over the limit: got %d %q, want 450", code, msg)
	}
	if !strings.Contains(msg, "Rate limit") {
		t.Errorf("refusal does not say why: %q", msg)
	}
	if got := h.metrics.RateSender.Load(); got != 1 {
		t.Errorf("rate_sender = %d, want 1", got)
	}

	// A different sender is unaffected: every key has its own bucket, which is
	// what lets this limiter live where it does.
	if code, _ := c.cmd("MAIL FROM:<quiet@ngm.dev>"); code != 250 {
		t.Errorf("an unrelated sender was refused: %d", code)
	}
}

// TestRateLimit_SenderIsCaseInsensitive: an address differing only in case is
// the same mailbox, and two buckets would double its allowance.
func TestRateLimit_SenderIsCaseInsensitive(t *testing.T) {
	h, _ := rateLimited(t, ratelimit.Rules{
		MsgPerSender: ratelimit.Rule{Rate: 1, Per: time.Hour},
	})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")

	c.cmd("MAIL FROM:<busy@ngm.dev>")
	c.cmd("RSET")
	if code, _ := c.cmd("MAIL FROM:<BUSY@NGM.DEV>"); code != 450 {
		t.Errorf("a differently-cased sender got a second allowance: %d", code)
	}
}

// TestRateLimit_NullSenderIsNeverLimited. Every bounce in the world shares the
// null sender, so one bucket for all of them would refuse exactly the
// notifications this gateway most needs to deliver.
func TestRateLimit_NullSenderIsNeverLimited(t *testing.T) {
	h, _ := rateLimited(t, ratelimit.Rules{
		MsgPerSender: ratelimit.Rule{Rate: 1, Per: time.Hour},
	})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")

	for i := range 5 {
		if code, msg := c.cmd("MAIL FROM:<>"); code != 250 {
			t.Fatalf("bounce %d: got %d %q, want 250", i+1, code, msg)
		}
		c.cmd("RSET")
	}
	if got := h.metrics.RateSender.Load(); got != 0 {
		t.Errorf("rate_sender = %d; the null sender must not be limited", got)
	}
}

// TestRateLimit_RcptDomainRefusesOneRecipient: checked per recipient and refused
// per recipient, so the rest of the transaction continues. That is why it is
// enforced at RCPT rather than at DATA.
func TestRateLimit_RcptDomainRefusesOneRecipient(t *testing.T) {
	h, _ := rateLimited(t, ratelimit.Rules{
		RcptPerDomain: ratelimit.Rule{Rate: 2, Per: time.Hour},
	})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")

	for _, rcpt := range []string{"a@busy.example", "b@busy.example"} {
		if code, msg := c.cmd("RCPT TO:<%s>", rcpt); code != 250 {
			t.Fatalf("%s: got %d %q, want 250", rcpt, code, msg)
		}
	}
	if code, msg := c.cmd("RCPT TO:<c@busy.example>"); code != 450 {
		t.Fatalf("over the limit: got %d %q, want 450", code, msg)
	}
	// A recipient at a different domain still goes through, in the SAME
	// transaction.
	if code, msg := c.cmd("RCPT TO:<d@quiet.example>"); code != 250 {
		t.Fatalf("an unrelated domain was refused: %d %q", code, msg)
	}

	if got := h.metrics.RateRcptDomain.Load(); got != 1 {
		t.Errorf("rate_rcpt_domain = %d, want 1", got)
	}
	// A refused recipient was never accepted, so it must not be counted as one.
	if got := h.metrics.RcptAccepted.Load(); got != 3 {
		t.Errorf("rcpt_accepted = %d, want 3 (the refused one is not accepted)", got)
	}

	// The message still goes, with the three recipients that were accepted.
	c.cmd("DATA")
	if code, _ := c.sendBody("Subject: partial\r\n\r\nbody"); code != 250 {
		t.Errorf("the transaction did not survive one refused recipient: %d", code)
	}
	select {
	case es := <-h.queued:
		total := 0
		for _, e := range es {
			total += len(e.Rcpts)
		}
		if total != 3 {
			t.Errorf("queued %d recipients, want 3", total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message was never queued")
	}
}

// TestRateLimit_OffChangesNothing is the regression floor: with no limits
// configured the session must behave exactly as it did before M15.
func TestRateLimit_OffChangesNothing(t *testing.T) {
	h, _ := rateLimited(t, ratelimit.Rules{})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	for range 20 {
		if code, _ := c.cmd("MAIL FROM:<busy@ngm.dev>"); code != 250 {
			t.Fatal("a sender was refused with no limits configured")
		}
		if code, _ := c.cmd("RCPT TO:<a@busy.example>"); code != 250 {
			t.Fatal("a recipient was refused with no limits configured")
		}
		c.cmd("RSET")
	}
	for name, got := range map[string]int64{
		"rate_sender":      h.metrics.RateSender.Load(),
		"rate_user":        h.metrics.RateUser.Load(),
		"rate_rcpt_domain": h.metrics.RateRcptDomain.Load(),
		"rate_auth":        h.metrics.RateAuth.Load(),
	} {
		if got != 0 {
			t.Errorf("%s = %d with no limits configured", name, got)
		}
	}
}

// TestRateLimit_NilBackendLimiterIsInert: every test in this package that builds
// a Backend by literal gets a nil Limiter, and it must behave as "not limited"
// rather than panicking on the mail path.
func TestRateLimit_NilBackendLimiterIsInert(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, relayAll))
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	if code, _ := c.cmd("MAIL FROM:<me@ngm.dev>"); code != 250 {
		t.Fatal("MAIL was refused with no limiter at all")
	}
	if code, _ := c.cmd("RCPT TO:<you@ngm.dev>"); code != 250 {
		t.Fatal("RCPT was refused with no limiter at all")
	}
}

// ── failed AUTH ──────────────────────────────────────────────────────────────

// authLimited is auth_test.go's plainHarness with a failed-AUTH limit on top —
// the same credential set and the same in-the-clear AUTH, so these tests differ
// from M13's in exactly one thing.
func authLimited(t *testing.T, rate int) (*harness, *ratelimit.Limiter) {
	t.Helper()
	l := ratelimit.New(ratelimit.Rules{
		AuthFailPerIP: ratelimit.Rule{Rate: rate, Per: time.Hour},
	}, nil)
	creds := authCreds(t)
	h := startServerTuned(t, compileRulesYAML(t, relayAll), func(cfg *config.Config, b *Backend) {
		cfg.Server.TLS.AllowInsecureAuth = true
		cfg.Auth = *creds
		b.Auth = func() *config.Auth { return creds }
		b.Limiter = func() *ratelimit.Limiter { return l }
	})
	return h, l
}

// TestRateLimit_FailedAuthIsRefused454 — and the code matters. 421 would be the
// honest answer for "go away", but go-smtp's handleAuth never closes the
// connection, so announcing a disconnection that does not happen would be a lie
// on the wire. 454 is RFC 4954's temporary authentication failure and is true.
func TestRateLimit_FailedAuthIsRefused454(t *testing.T) {
	h, _ := authLimited(t, 3)

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")

	for i := range 3 {
		code, _ := authPlain(c, "app@ngm.dev", "wrong")
		if code != 535 {
			t.Fatalf("attempt %d: got %d, want 535", i+1, code)
		}
	}

	code, msg := authPlain(c, "app@ngm.dev", "wrong")
	if code != 454 {
		t.Fatalf("over the limit: got %d %q, want 454", code, msg)
	}
	if !strings.Contains(msg, "Too many authentication failures") {
		t.Errorf("refusal does not say why: %q", msg)
	}

	if got := h.metrics.RateAuth.Load(); got != 1 {
		t.Errorf("rate_auth = %d, want 1", got)
	}
	// The refusal is NOT an authentication failure: no password was compared,
	// so auth_failed stays at the three real attempts. The two counters are
	// siblings, not a subset — together they are what stuffing looks like.
	if got := h.metrics.AuthFailed.Load(); got != 3 {
		t.Errorf("auth_failed = %d, want 3", got)
	}
}

// TestRateLimit_SuccessfulAuthDoesNotSpendTheBudget is the reason Blocked and
// Spend are separate operations. The key is called auth_failures_per_ip; a
// client that gets its password right every time must never be throttled by it,
// however tight it is set.
func TestRateLimit_SuccessfulAuthDoesNotSpendTheBudget(t *testing.T) {
	h, _ := authLimited(t, 1)

	for i := range 5 {
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO sender.example.com")
		code, msg := authPlain(c, "app@ngm.dev", testPassword)
		if code != 235 {
			t.Fatalf("correct login %d: got %d %q, want 235", i+1, code, msg)
		}
		c.cmd("QUIT")
	}
	if got := h.metrics.RateAuth.Load(); got != 0 {
		t.Errorf("rate_auth = %d; successful logins spent the failure budget", got)
	}
}

// TestRateLimit_FailedAuthLimitSurvivesReconnect: the budget is keyed on the
// peer's address, not on the connection, so hanging up and dialling again does
// not reset it. That is the whole point against a stuffing run.
func TestRateLimit_FailedAuthLimitSurvivesReconnect(t *testing.T) {
	h, _ := authLimited(t, 2)

	for i := range 2 {
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO sender.example.com")
		if code, _ := authPlain(c, "app@ngm.dev", "wrong"); code != 535 {
			t.Fatalf("attempt %d was not a plain failure", i+1)
		}
		c.cmd("QUIT")
	}

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	if code, _ := authPlain(c, "app@ngm.dev", "wrong"); code != 454 {
		t.Errorf("a new connection reset the failed-AUTH budget: %d", code)
	}
}
