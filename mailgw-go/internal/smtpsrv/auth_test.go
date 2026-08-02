package smtpsrv

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// The password every test in this file authenticates with, and its hash.
//
// Cost 4 rather than the console's 10: these tests run bcrypt dozens of times
// and the cost is the point of the algorithm. Nothing here depends on the cost,
// because validateAuth accepts any hash bcrypt itself can parse.
const testPassword = "correct horse battery staple"

func testHash(t *testing.T, pass string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	return string(h)
}

// authCreds is a one-user credential set.
func authCreds(t *testing.T) *config.Auth {
	t.Helper()
	return &config.Auth{Users: []config.AuthUser{
		{User: "app@ngm.dev", Hash: testHash(t, testPassword)},
	}}
}

// plainHarness starts a server with credentials and AUTH allowed in the clear,
// which is the only way to exercise the mechanism without a TLS handshake in
// every test. The TLS-gating tests below assert that this is not the default.
func plainHarness(t *testing.T, rs *ruleset.Ruleset) *harness {
	t.Helper()
	if rs == nil {
		rs = compileRulesYAML(t, relayEverything)
	}
	creds := authCreds(t)
	return startServerTuned(t, rs, func(cfg *config.Config, b *Backend) {
		cfg.Server.TLS.AllowInsecureAuth = true
		cfg.Auth = *creds
		b.Auth = func() *config.Auth { return creds }
	})
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// authPlain runs a one-shot AUTH PLAIN with the initial response inline, which
// is what every real submission client sends.
func authPlain(c *client, user, pass string) (int, string) {
	return c.cmd("AUTH PLAIN %s", b64("\x00"+user+"\x00"+pass))
}

// --- what is advertised ---

func TestAuth_NotAdvertisedWithoutCredentials(t *testing.T) {
	// The default harness has no credential set at all, which is every
	// deployment that existed before this milestone.
	h := startServerWithRules(t, compileRulesYAML(t, relayEverything))
	caps := ehloCaps(t, dialClient(t, h.addr))
	if strings.Contains(caps, "AUTH") {
		t.Errorf("EHLO advertises AUTH with no credentials configured:\n%s", caps)
	}
}

func TestAuth_NotAdvertisedOnAnUnencryptedSessionByDefault(t *testing.T) {
	creds := authCreds(t)
	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, b *Backend) {
		// Credentials, but no TLS and no allow_insecure_auth: go-smtp's
		// authAllowed() is false, so the capability must not appear.
		cfg.Auth = *creds
		b.Auth = func() *config.Auth { return creds }
	})

	caps := ehloCaps(t, dialClient(t, h.addr))
	if strings.Contains(caps, "AUTH") {
		t.Errorf("EHLO advertises AUTH in the clear without allow_insecure_auth:\n%s", caps)
	}
}

func TestAuth_RefusedInTheClearWithoutAllowInsecureAuth(t *testing.T) {
	creds := authCreds(t)
	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, b *Backend) {
		cfg.Auth = *creds
		b.Auth = func() *config.Auth { return creds }
	})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	// Not merely unadvertised: the command itself is refused, so a client that
	// tries anyway cannot put a password on a cleartext socket.
	code, raw := authPlain(c, "app@ngm.dev", testPassword)
	if code != 523 {
		t.Errorf("AUTH in the clear = %d %q, want 523", code, raw)
	}
	if h.metrics.AuthOK.Load() != 0 {
		t.Errorf("auth_ok = %d, want 0 — the credential was never judged",
			h.metrics.AuthOK.Load())
	}
}

func TestAuth_AdvertisedWithCredentialsAndAllowInsecureAuth(t *testing.T) {
	h := plainHarness(t, nil)
	caps := ehloCaps(t, dialClient(t, h.addr))
	for _, want := range []string{"AUTH", "PLAIN", "LOGIN"} {
		if !strings.Contains(caps, want) {
			t.Errorf("EHLO does not advertise %s:\n%s", want, caps)
		}
	}
}

func TestAuth_AdvertisedOverSTARTTLSWithoutAllowInsecureAuth(t *testing.T) {
	// The shipped shape: a keypair, STARTTLS, and no allow_insecure_auth. AUTH
	// must be absent before the upgrade and present after it.
	cert, key := keypair(t)
	creds := authCreds(t)
	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, b *Backend) {
		cfg.Server.TLS = config.TLSConfig{Cert: cert, Key: key, STARTTLS: true}
		cfg.Auth = *creds
		b.Auth = func() *config.Auth { return creds }
	})

	if caps := ehloCaps(t, dialClient(t, h.addr)); strings.Contains(caps, "AUTH") {
		t.Errorf("EHLO advertises AUTH before the upgrade:\n%s", caps)
	}

	c, closeConn := startTLSClient(t, h)
	defer closeConn()
	_, caps := c.cmd("EHLO sender.example.com")
	if !strings.Contains(caps, "AUTH") {
		t.Errorf("EHLO does not advertise AUTH after STARTTLS:\n%s", caps)
	}
}

// --- the exchange itself ---

func TestAuth_PlainSucceeds(t *testing.T) {
	h := plainHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	if code, raw := authPlain(c, "app@ngm.dev", testPassword); code != 235 {
		t.Fatalf("AUTH PLAIN = %d %q, want 235", code, raw)
	}
	if got := h.metrics.AuthOK.Load(); got != 1 {
		t.Errorf("auth_ok = %d, want 1", got)
	}
	if got := h.metrics.AuthFailed.Load(); got != 0 {
		t.Errorf("auth_failed = %d, want 0", got)
	}
}

// A wrong password is permanent, so it must answer 535 5.7.8 rather than
// go-smtp's default 454 4.7.0 for an authenticator error — which would tell a
// client to retry the same wrong password.
func TestAuth_BadCredentialsAnswer535(t *testing.T) {
	for _, tc := range []struct{ name, user, pass string }{
		{"wrong password", "app@ngm.dev", "hunter2"},
		{"unknown user", "nobody@ngm.dev", testPassword},
		{"empty password", "app@ngm.dev", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := plainHarness(t, nil)
			c := dialClient(t, h.addr)
			c.greet()
			c.cmd("EHLO probe.invalid")

			code, raw := authPlain(c, tc.user, tc.pass)
			if code != 535 {
				t.Errorf("AUTH = %d %q, want 535", code, raw)
			}
			if !strings.Contains(raw, "5.7.8") {
				t.Errorf("AUTH reply %q does not carry the 5.7.8 enhanced code", raw)
			}
			if got := h.metrics.AuthFailed.Load(); got != 1 {
				t.Errorf("auth_failed = %d, want 1", got)
			}
			if got := h.metrics.AuthOK.Load(); got != 0 {
				t.Errorf("auth_ok = %d, want 0", got)
			}
		})
	}
}

// The authorization identity is the one PLAIN field this gateway has no meaning
// for. Refused rather than ignored, so a client cannot believe it assumed an
// identity the audit trail then attributes to the login.
func TestAuth_PlainRefusesADifferentAuthorizationIdentity(t *testing.T) {
	h := plainHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	code, raw := c.cmd("AUTH PLAIN %s", b64("someone@else\x00app@ngm.dev\x00"+testPassword))
	if code != 535 {
		t.Errorf("AUTH with a foreign authzid = %d %q, want 535", code, raw)
	}
}

func TestAuth_PlainAcceptsItsOwnUsernameAsTheAuthorizationIdentity(t *testing.T) {
	h := plainHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	code, raw := c.cmd("AUTH PLAIN %s", b64("app@ngm.dev\x00app@ngm.dev\x00"+testPassword))
	if code != 235 {
		t.Errorf("AUTH with its own authzid = %d %q, want 235", code, raw)
	}
}

// LOGIN is the mechanism go-sasl has no server for, so this exercises code that
// exists only in this package.
func TestAuth_LoginChallengeExchange(t *testing.T) {
	h := plainHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	code, raw := c.cmd("AUTH LOGIN")
	if code != 334 || !strings.Contains(raw, b64("Username:")) {
		t.Fatalf("AUTH LOGIN = %d %q, want 334 with a Username: challenge", code, raw)
	}
	code, raw = c.cmd("%s", b64("app@ngm.dev"))
	if code != 334 || !strings.Contains(raw, b64("Password:")) {
		t.Fatalf("username = %d %q, want 334 with a Password: challenge", code, raw)
	}
	if code, raw = c.cmd("%s", b64(testPassword)); code != 235 {
		t.Fatalf("password = %d %q, want 235", code, raw)
	}
	if got := h.metrics.AuthOK.Load(); got != 1 {
		t.Errorf("auth_ok = %d, want 1", got)
	}
}

// go-smtp's `AUTH LOGIN <base64>` form: the username rides in as the initial
// response and the first challenge is skipped.
func TestAuth_LoginWithAnInitialResponse(t *testing.T) {
	h := plainHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	code, raw := c.cmd("AUTH LOGIN %s", b64("app@ngm.dev"))
	if code != 334 || !strings.Contains(raw, b64("Password:")) {
		t.Fatalf("AUTH LOGIN <ir> = %d %q, want 334 with a Password: challenge", code, raw)
	}
	if code, raw = c.cmd("%s", b64(testPassword)); code != 235 {
		t.Fatalf("password = %d %q, want 235", code, raw)
	}
}

func TestAuth_LoginWithABadPasswordAnswers535(t *testing.T) {
	h := plainHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	c.cmd("AUTH LOGIN %s", b64("app@ngm.dev"))
	code, raw := c.cmd("%s", b64("hunter2"))
	if code != 535 {
		t.Errorf("AUTH LOGIN with a bad password = %d %q, want 535", code, raw)
	}
	if got := h.metrics.AuthFailed.Load(); got != 1 {
		t.Errorf("auth_failed = %d, want 1", got)
	}
}

// CRAM-MD5 and friends need a recoverable password, which is exactly what
// storing hashes rules out. Refusing the mechanism is the honest answer.
func TestAuth_UnknownMechanismIsRefused(t *testing.T) {
	h := plainHarness(t, nil)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")

	code, raw := c.cmd("AUTH CRAM-MD5")
	if code == 235 || code == 334 {
		t.Errorf("AUTH CRAM-MD5 = %d %q, want a refusal", code, raw)
	}
}

// --- the rule fields ---

// authRules refuses any message from an unauthenticated client, which is the
// policy M13 exists to make expressible.
const authRules = `
version: 1
policy:
  - name: submission-requires-auth
    match: {field: auth.authenticated, op: eq, value: false}
    then:
      - {action: reject, code: 550, message: "5.7.1 authentication required"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

func TestAuth_PolicyRuleSeesTheAuthenticatedSession(t *testing.T) {
	h := plainHarness(t, compileRulesYAML(t, authRules))

	t.Run("unauthenticated is refused", func(t *testing.T) {
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO probe.invalid")
		code, raw := c.cmd("MAIL FROM:<a@example.com>")
		if code != 550 {
			t.Errorf("MAIL without AUTH = %d %q, want 550", code, raw)
		}
	})

	t.Run("authenticated is accepted", func(t *testing.T) {
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO probe.invalid")
		if code, raw := authPlain(c, "app@ngm.dev", testPassword); code != 235 {
			t.Fatalf("AUTH = %d %q", code, raw)
		}
		code, raw := c.cmd("MAIL FROM:<a@example.com>")
		if code != 250 {
			t.Errorf("MAIL after AUTH = %d %q, want 250", code, raw)
		}
	})
}

// The regression test for the trap this milestone's plan named: Mail rebuilds
// env.Helo from heloEnv() on every transaction, so an identity written into the
// HeloEnv rather than onto the session is silently lost at MAIL — and the rule
// above would refuse a client that had just authenticated.
func TestAuth_IdentitySurvivesTheHeloEnvRebuildAndRSET(t *testing.T) {
	h := plainHarness(t, compileRulesYAML(t, authRules))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	if code, raw := authPlain(c, "app@ngm.dev", testPassword); code != 235 {
		t.Fatalf("AUTH = %d %q", code, raw)
	}

	// Two transactions with an RSET between them. resetTxn deliberately leaves
	// the identity alone — go-smtp keeps its own didAuth across RSET too.
	for i := range 2 {
		if code, raw := c.cmd("MAIL FROM:<a@example.com>"); code != 250 {
			t.Fatalf("MAIL #%d = %d %q, want 250", i+1, code, raw)
		}
		if code, raw := c.cmd("RSET"); code != 250 {
			t.Fatalf("RSET #%d = %d %q", i+1, code, raw)
		}
	}
}

// A rule reading auth.user must be inferred to the MAIL stage, not HELO.
// Connect- and helo-stage policy runs inside Backend.NewSession, before the
// client has had any opportunity to authenticate, so a HELO-stage inference
// would make this rule permanently unmatchable.
func TestAuth_FieldsAreInferredToTheMailStage(t *testing.T) {
	rs := compileRulesYAML(t, `
version: 1
policy:
  - name: known-user
    match: {field: auth.user, op: eq, value: "app@ngm.dev"}
    then:
      - {action: reject, code: 550, message: "5.7.1 no"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`)
	if len(rs.Policy) != 1 {
		t.Fatalf("expected one policy rule, got %d", len(rs.Policy))
	}
	if got := rs.Policy[0].Stage; got != ruleset.StageMail {
		t.Errorf("a rule reading auth.user is inferred at %s, want mail — connect and "+
			"helo policy run before the client can authenticate", got)
	}
}

// --- STARTTLS ---

// go-smtp discards the whole session on a STARTTLS upgrade — Logout, a nil
// session, c.helo = "" and c.didAuth = false (conn.go:944-952). So an
// authentication performed before the upgrade is forgotten, which is correct: a
// credential presented in the clear must not carry into the encrypted session.
func TestAuth_IsDiscardedByTheSTARTTLSUpgrade(t *testing.T) {
	cert, key := keypair(t)
	creds := authCreds(t)
	h := startServerTuned(t, compileRulesYAML(t, authRules), func(cfg *config.Config, b *Backend) {
		cfg.Server.TLS = config.TLSConfig{Cert: cert, Key: key, STARTTLS: true, AllowInsecureAuth: true}
		cfg.Auth = *creds
		b.Auth = func() *config.Auth { return creds }
	})

	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.greet()
	c.cmd("EHLO sender.example.com")
	if code, raw := authPlain(c, "app@ngm.dev", testPassword); code != 235 {
		t.Fatalf("AUTH before STARTTLS = %d %q", code, raw)
	}
	if code, raw := c.cmd("STARTTLS"); code != 220 {
		t.Fatalf("STARTTLS = %d %q", code, raw)
	}

	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: "devbook.local"})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = tc.SetDeadline(time.Now().Add(20 * time.Second))
	tlsClient := &client{t: t, conn: tc, r: bufio.NewReader(tc)}
	tlsClient.cmd("EHLO sender.example.com")

	// The policy rule refuses an unauthenticated MAIL, so this is the proof the
	// upgraded session starts anonymous.
	if code, raw := tlsClient.cmd("MAIL FROM:<a@example.com>"); code != 550 {
		t.Errorf("MAIL after the upgrade = %d %q, want 550 — the pre-upgrade "+
			"authentication must not carry over", code, raw)
	}

	// And it can authenticate again on the encrypted session.
	if code, raw := authPlain(tlsClient, "app@ngm.dev", testPassword); code != 235 {
		t.Fatalf("AUTH after STARTTLS = %d %q, want 235", code, raw)
	}
	if code, raw := tlsClient.cmd("MAIL FROM:<a@example.com>"); code != 250 {
		t.Errorf("MAIL after re-authenticating = %d %q, want 250", code, raw)
	}
}

// --- hot swap ---

// Credentials are read per AUTH command rather than snapshotted per session, so
// a revoked one stops working on the next attempt rather than at the next
// restart.
func TestAuth_CredentialsAreReadPerCommand(t *testing.T) {
	live := authCreds(t)
	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, b *Backend) {
		cfg.Server.TLS.AllowInsecureAuth = true
		cfg.Auth = *live
		b.Auth = func() *config.Auth { return live }
	})

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	if code, raw := authPlain(c, "app@ngm.dev", testPassword); code != 235 {
		t.Fatalf("AUTH = %d %q", code, raw)
	}

	// The operator revokes it. A second connection must be refused.
	*live = config.Auth{}

	c2 := dialClient(t, h.addr)
	c2.greet()
	if caps := func() string { _, raw := c2.cmd("EHLO probe.invalid"); return raw }(); strings.Contains(caps, "AUTH") {
		t.Errorf("EHLO still advertises AUTH after the last credential was removed:\n%s", caps)
	}
}
