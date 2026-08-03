package node

import (
	"bufio"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ratelimit"
)

// M16's lesson, and this repo's standing feedback: a package test builds its
// subject directly, and the three things that only take effect through this
// package's wiring — the listener's position in the chain, the limiter's
// lifetime across an apply, and its absence from restartRequired — had no test
// at all until this file.

const rateLimitServer = `hostname: relay.example
listen:
  - addr: "127.0.0.1:0"
outbound:
  spool_dir: /tmp/does-not-matter
ratelimit:
  connect_per_ip: {rate: 10, per: 1m}
`

// TestRestartRequired_RateLimitsNeverNeedOne is the milestone's own requirement:
// a limit an operator cannot adjust without restarting a mail server during an
// incident is a limit they will not use. So `ratelimit` must be absent from
// restartRequired, and stay absent.
func TestRestartRequired_RateLimitsNeverNeedOne(t *testing.T) {
	base := mutate(t, config.BundleOptions{}, withServer(rateLimitServer)).cfg

	for _, body := range []string{
		rateLimitServer + "  messages_per_sender: {rate: 100, per: 1h}\n",
		// Every limit removed entirely.
		"hostname: relay.example\nlisten:\n  - addr: \"127.0.0.1:0\"\n" +
			"outbound:\n  spool_dir: /tmp/does-not-matter\n",
		rateLimitServer + "  max_keys: 500\n",
	} {
		next := mutate(t, config.BundleOptions{}, withServer(body)).cfg
		if got := restartRequired(base, next); slices.Contains(got, "ratelimit") {
			t.Errorf("a rate-limit change asked for a restart: %v", got)
		}
	}
}

// TestGatewayRateLimits_SurviveAnApply is the trap the limiter's design exists
// to avoid. Rebuilding it on every apply would be simpler and would hand every
// peer a fresh allowance whenever an unrelated configuration change was
// deployed — so an attacker under pressure would be released by a routine
// deploy of something else entirely.
func TestGatewayRateLimits_SurviveAnApply(t *testing.T) {
	g := newGateway(discardLogger())

	g.setRateLimits(&config.Config{Server: config.Server{
		RateLimit: config.RateLimits{
			ConnectPerIP: config.RateLimit{Rate: 2, Per: config.Duration(time.Hour)},
		},
	}})

	l := g.Limiter()
	for i := range 2 {
		if !l.Allow(ratelimit.ConnPerIP, "10.0.0.1") {
			t.Fatalf("event %d refused inside the limit", i+1)
		}
	}
	if l.Allow(ratelimit.ConnPerIP, "10.0.0.1") {
		t.Fatal("setup: the peer should be drained")
	}

	// An apply that changes something else entirely.
	g.setRateLimits(&config.Config{Server: config.Server{
		RateLimit: config.RateLimits{
			ConnectPerIP:      config.RateLimit{Rate: 2, Per: config.Duration(time.Hour)},
			MessagesPerSender: config.RateLimit{Rate: 50, Per: config.Duration(time.Hour)},
		},
	}})

	if g.Limiter() != l {
		t.Error("the limiter was replaced by an apply; its buckets are gone")
	}
	if l.Allow(ratelimit.ConnPerIP, "10.0.0.1") {
		t.Error("a routine deploy handed a drained peer a fresh allowance")
	}
}

// TestGatewayRateLimits_ExistBeforeTheFirstApply: a managed gateway builds its
// listener chain from the first bundle, and the throttle listener reads the
// limiter on the very first connection it accepts. A nil there would panic the
// accept loop.
func TestGatewayRateLimits_ExistBeforeTheFirstApply(t *testing.T) {
	g := newGateway(discardLogger())
	if g.Limiter() == nil {
		t.Fatal("a freshly built gateway has no limiter")
	}
	// And it limits nothing until told to.
	for range 100 {
		if !g.Limiter().Allow(ratelimit.ConnPerIP, "10.0.0.1") {
			t.Fatal("an unconfigured limiter refused an event")
		}
	}
}

func TestRateLimitRules_Translation(t *testing.T) {
	got := rateLimitRules(config.RateLimits{
		ConnectPerIP:      config.RateLimit{Rate: 1, Per: config.Duration(time.Minute), Burst: 5},
		MessagesPerSender: config.RateLimit{Rate: 2, Per: config.Duration(time.Hour)},
		MessagesPerUser:   config.RateLimit{Rate: 3, Per: config.Duration(time.Hour)},
		RcptsPerDomain:    config.RateLimit{Rate: 4, Per: config.Duration(time.Minute)},
		AuthFailuresPerIP: config.RateLimit{Rate: 5, Per: config.Duration(10 * time.Minute)},
		MaxKeys:           123,
	})

	want := ratelimit.Rules{
		ConnPerIP:     ratelimit.Rule{Rate: 1, Per: time.Minute, Burst: 5},
		MsgPerSender:  ratelimit.Rule{Rate: 2, Per: time.Hour},
		MsgPerUser:    ratelimit.Rule{Rate: 3, Per: time.Hour},
		RcptPerDomain: ratelimit.Rule{Rate: 4, Per: time.Minute},
		AuthFailPerIP: ratelimit.Rule{Rate: 5, Per: 10 * time.Minute},
		MaxKeys:       123,
	}
	if got != want {
		t.Errorf("rateLimitRules:\n got %+v\nwant %+v", got, want)
	}

	// Every dimension is wired. A forgotten one would be a limit an operator
	// configures and that silently does nothing.
	if !got.Any() {
		t.Error("Any() is false for a fully-populated translation")
	}
	if empty := rateLimitRules(config.RateLimits{}); empty.Any() {
		t.Error("an empty configuration translated into an active rule")
	}
}

func TestListenerChain_PerIPRateLimit(t *testing.T) {
	// Burst of 3, and a window long enough that nothing refills mid-test.
	l := ratelimit.New(ratelimit.Rules{
		ConnPerIP: ratelimit.Rule{Rate: 3, Per: time.Hour},
	}, nil)
	addr, b, m := startChainLimited(t, false, 16, l)

	// The first three get a session.
	for i := range 3 {
		if err := greetOnce(addr); err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
	}

	// The fourth is answered 421 and closed by the listener, without ever
	// reaching the SMTP server.
	before := b.sessions.Load()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	if !strings.HasPrefix(line, "421") {
		t.Fatalf("over the rate: got %q, want a 421", line)
	}
	if !strings.Contains(line, "4.7.0") {
		t.Errorf("refusal has no enhanced code: %q", line)
	}

	if got := b.sessions.Load(); got != before {
		t.Errorf("the refused connection reached the SMTP backend (%d -> %d)", before, got)
	}
	if got := m.RateConn.Load(); got != 1 {
		t.Errorf("rate_conn = %d, want 1", got)
	}
	// The concurrency cap sits OUTSIDE this one, and a peer refused for rate
	// must never have spent one of its slots.
	if got := m.ConnThrottled.Load(); got != 0 {
		t.Errorf("conn_throttled = %d; a rate refusal consumed a cap slot", got)
	}
	// The allowlist sits inside-of-outside: the peer passed it, which is what
	// makes rate_conn a subset of conn_accepted.
	if m.ConnAccepted.Load() < 4 {
		t.Errorf("conn_accepted = %d; the refused peer should still have been allowed",
			m.ConnAccepted.Load())
	}
}

// TestListenerChain_NoLimiterIsUnchanged: with no rate limits configured the
// chain must be exactly what it was before M15 — Throttle returns the listener
// it was given rather than a wrapper that always says yes.
func TestListenerChain_NoLimiterIsUnchanged(t *testing.T) {
	addr, b, m := startChainLimited(t, false, 16, nil)

	for i := range 10 {
		if err := greetOnce(addr); err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
	}
	if got := m.RateConn.Load(); got != 0 {
		t.Errorf("rate_conn = %d with no limiter", got)
	}
	if got := b.sessions.Load(); got < 10 {
		t.Errorf("only %d of 10 connections reached the backend", got)
	}
}

// greetOnce opens a connection, reads the banner, sends EHLO and reads the
// reply, then closes.
//
// Waiting for the EHLO REPLY matters: go-smtp calls Backend.NewSession while
// handling EHLO, so a test that only read the banner would race the counter it
// is about to assert on.
func greetOnce(addr string) error {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	if _, err := br.ReadString('\n'); err != nil {
		return fmt.Errorf("banner: %w", err)
	}
	if _, err := fmt.Fprintf(c, "EHLO probe.test\r\n"); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if _, err := br.ReadString('\n'); err != nil {
		return fmt.Errorf("EHLO reply: %w", err)
	}
	return nil
}
