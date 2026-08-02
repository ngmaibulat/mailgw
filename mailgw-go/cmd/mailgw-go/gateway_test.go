package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
)

// mutate returns the testdata bundle with one edit applied, loaded.
func mutate(t *testing.T, opts config.BundleOptions, edit func(*config.Bundle)) *loaded {
	t.Helper()

	var b config.Bundle
	if err := json.Unmarshal(bundleFromTestdata(t), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if edit != nil {
		edit(&b)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	l, err := loadBundle(raw, opts, "bundle v1 (version_id 1)")
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	return l
}

// withServer replaces the server profile wholesale, for the cases that need a
// specific field changed.
func withServer(body string) func(*config.Bundle) {
	return func(b *config.Bundle) { b.Server = &body }
}

// The baseline server profile, kept minimal so each case below differs in
// exactly one thing.
const baseServer = `hostname: relay.example
listen:
  - addr: "127.0.0.1:0"
outbound:
  spool_dir: /tmp/does-not-matter
`

func TestRestartRequired(t *testing.T) {
	base := mutate(t, config.BundleOptions{}, withServer(baseServer)).cfg

	cases := []struct {
		name string
		edit func(*config.Bundle)
		want []string
	}{
		{"identical", withServer(baseServer), nil},
		{"listen", withServer("hostname: relay.example\nlisten:\n  - addr: \"127.0.0.1:2626\"\noutbound:\n  spool_dir: /tmp/does-not-matter\n"), []string{"listen"}},
		{"hostname", withServer("hostname: other.example\nlisten:\n  - addr: \"127.0.0.1:0\"\noutbound:\n  spool_dir: /tmp/does-not-matter\n"), []string{"hostname"}},
		{"greeting", withServer(baseServer + "greeting: hello\n"), []string{"greeting"}},
		{"local_domains", withServer(baseServer + "local_domains: [a.example]\n"), []string{"local_domains"}},
		{"max", withServer(baseServer + "max:\n  bytes: 12345\n"), []string{"max"}},
		{"smtputf8", withServer(baseServer + "smtputf8: false\n"), []string{"smtputf8"}},
		{"inactivity_timeout", withServer(baseServer + "inactivity_timeout: 120s\n"), []string{"inactivity_timeout"}},
		// The PROXY-protocol keys live inside listen[], which restartRequired
		// compares with reflect.DeepEqual — so they need no line of their own
		// there, but that is worth pinning rather than assuming.
		{
			"proxy_protocol counts as listen",
			withServer("hostname: relay.example\nlisten:\n  - addr: \"127.0.0.1:0\"\n    proxy_protocol: true\n    proxy_trusted: [\"10.0.0.0/8\"]\noutbound:\n  spool_dir: /tmp/does-not-matter\n"),
			[]string{"listen"},
		},
		{"log", withServer(baseServer + "log:\n  level: debug\n"), []string{"log"}},
		{"dsn", withServer(baseServer + "dsn:\n  enabled: false\n"), []string{"dsn"}},
		{"attach", withServer(baseServer + "attach:\n  fail: open\n"), []string{"attach"}},
		{"events", withServer(baseServer + "events:\n  retries: 9\n"), []string{"events"}},
		{
			"outbound",
			withServer("hostname: relay.example\nlisten:\n  - addr: \"127.0.0.1:0\"\noutbound:\n  spool_dir: /tmp/does-not-matter\n  concurrency: 3\n"),
			[]string{"outbound"},
		},
		{
			"spool_dir counts as outbound",
			withServer("hostname: relay.example\nlisten:\n  - addr: \"127.0.0.1:0\"\noutbound:\n  spool_dir: /tmp/elsewhere\n"),
			[]string{"outbound"},
		},
		{
			"relays",
			func(b *config.Bundle) {
				withServer(baseServer)(b)
				for name, members := range b.Relays {
					members[0].Exchange = "moved.example"
					b.Relays[name] = members
					break
				}
			},
			[]string{"relays"},
		},
		{
			"logging",
			func(b *config.Bundle) {
				withServer(baseServer)(b)
				b.Logging.URLDelivery = "http://elsewhere:3000/api/delivery"
			},
			[]string{"logging"},
		},
		// The two things that hot-swap must NOT ask for a restart, or every
		// rule edit would look like it needed one and the flag would stop
		// meaning anything.
		{
			"allowlist only",
			func(b *config.Bundle) {
				withServer(baseServer)(b)
				b.Allowlist = json.RawMessage(`{"allowed":["10.0.0.0/8"]}`)
			},
			nil,
		},
		{
			"rules only",
			func(b *config.Bundle) {
				withServer(baseServer)(b)
				routing := "version: 1\nroutes:\n  - name: everything\n    match: {always: true}\n    then: [{action: relay, relay: Outbound}]\n"
				b.Routing = &routing
			},
			nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := mutate(t, config.BundleOptions{}, tc.edit).cfg
			got := restartRequired(base, next)
			if !slices.Equal(got, tc.want) {
				t.Errorf("restartRequired = %v, want %v", got, tc.want)
			}
		})
	}
}

// Two configurations built from the same bytes must compare equal. This guards
// against a field growing an incomparable type and quietly turning every apply
// into a restart-required one.
func TestRestartRequired_SameBytesAreEqual(t *testing.T) {
	a := mutate(t, config.BundleOptions{}, nil).cfg
	b := mutate(t, config.BundleOptions{}, nil).cfg
	if got := restartRequired(a, b); got != nil {
		t.Errorf("identical configurations reported %v", got)
	}
}

// The first apply builds the process and starts serving; every later one swaps
// the allowlist and the rules and touches nothing else.
func TestGatewayApply_FirstBringsUpThenSwaps(t *testing.T) {
	spoolDir := filepath.Join(t.TempDir(), "queue")
	opts := config.BundleOptions{SpoolDir: spoolDir}

	g := newGateway(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer g.shutdown()

	first := mutate(t, opts, withServer(baseServer))
	restart, err := g.apply(ctx, first)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if restart != nil {
		t.Errorf("the first apply has nothing to differ from, got %v", restart)
	}
	if !g.Serving() {
		t.Error("the gateway should be serving after its first apply")
	}
	if g.Spool() == nil {
		t.Error("the spool should exist after the first apply")
	}

	spool, live, rules := g.Spool(), g.live, g.rules.Load()

	// A rules-only change: the hot path.
	second := mutate(t, opts, func(b *config.Bundle) {
		withServer(baseServer)(b)
		routing := "version: 1\nroutes:\n  - name: only\n    match: {always: true}\n    then: [{action: relay, relay: Outbound}]\n"
		b.Routing = &routing
	})
	if restart, err = g.apply(ctx, second); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if restart != nil {
		t.Errorf("a rules-only change should need no restart, got %v", restart)
	}
	if g.rules.Load() == rules {
		t.Error("the rules pointer was not swapped")
	}
	if g.Spool() != spool {
		t.Error("the spool must not be rebuilt by a later apply")
	}
	if g.live != live {
		t.Error("the live config must stay the one the process started with")
	}
}

// A configuration whose rules name a relay group the running process cannot
// dial must be refused outright, leaving both pointers exactly as they were.
// Half-applying it would give a gateway rules it cannot honour.
func TestGatewayApply_RejectsRulesTheLiveRelaysCannotSatisfy(t *testing.T) {
	spoolDir := filepath.Join(t.TempDir(), "queue")
	opts := config.BundleOptions{SpoolDir: spoolDir}

	g := newGateway(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer g.shutdown()

	if _, err := g.apply(ctx, mutate(t, opts, withServer(baseServer))); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	beforeRules, beforeList := g.rules.Load(), g.allowlist.Load()

	// A bundle carrying a brand-new relay group AND a rule that routes to it.
	// It compiles against its own table, so only the recompile against the live
	// one catches it — which is the whole point of that step.
	next := mutate(t, opts, func(b *config.Bundle) {
		withServer(baseServer)(b)
		b.Relays["Fresh"] = b.Relays["Outbound"]
		routing := "version: 1\nroutes:\n  - name: to-fresh\n    match: {always: true}\n    then: [{action: relay, relay: Fresh}]\n"
		b.Routing = &routing
		b.Allowlist = json.RawMessage(`{"allowed":["10.0.0.0/8"]}`)
	})

	restart, err := g.apply(ctx, next)
	if err == nil {
		t.Fatal("expected the apply to be refused")
	}
	if !slices.Contains(restart, "relays") {
		t.Errorf("the caller should still learn what changed, got %v", restart)
	}
	if g.rules.Load() != beforeRules {
		t.Error("the rules pointer must be untouched by a failed apply")
	}
	if g.allowlist.Load() != beforeList {
		t.Error("the allowlist pointer must be untouched by a failed apply")
	}
}

// The metrics bearer token only exists through this wiring: internal/adminui's
// own tests hand it a MetricsToken closure directly, so nothing there would
// notice if the bundle field never reached the gateway. That is exactly the gap
// M16 found in M11 — three items that only took effect through cmd/mailgw-go,
// which had no test at all.
func TestGatewayApply_AdminTokenFollowsTheBundle(t *testing.T) {
	spoolDir := filepath.Join(t.TempDir(), "queue")
	opts := config.BundleOptions{SpoolDir: spoolDir}

	withToken := func(token string) func(*config.Bundle) {
		return func(b *config.Bundle) {
			withServer(baseServer)(b)
			b.Admin = &config.Admin{MetricsToken: token}
		}
	}

	g := newGateway(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer g.shutdown()

	// Before the first apply there is no configuration to have read it from, so
	// the endpoints are open — the one window this design cannot close.
	if got := g.AdminToken(); got != "" {
		t.Errorf("AdminToken() before the first apply = %q, want empty", got)
	}

	// The first apply is bringUp, a different branch from every later one.
	if _, err := g.apply(ctx, mutate(t, opts, withToken("first-token"))); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if got := g.AdminToken(); got != "first-token" {
		t.Fatalf("AdminToken() after bringUp = %q, want first-token", got)
	}

	if _, err := g.apply(ctx, mutate(t, opts, withToken("second-token"))); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := g.AdminToken(); got != "second-token" {
		t.Errorf("AdminToken() after a swap = %q, want second-token", got)
	}

	// Removing it from the bundle must actually open the endpoints again, not
	// leave the previous token enforced for ever.
	if _, err := g.apply(ctx, mutate(t, opts, func(b *config.Bundle) {
		withServer(baseServer)(b)
		b.Admin = nil
	})); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if got := g.AdminToken(); got != "" {
		t.Errorf("AdminToken() after the key was removed = %q, want empty", got)
	}
}

// A configuration that was refused is not in force, and that has to include the
// token: otherwise a bad deploy could silently unlock /metrics.
func TestGatewayApply_FailedApplyKeepsTheAdminToken(t *testing.T) {
	spoolDir := filepath.Join(t.TempDir(), "queue")
	opts := config.BundleOptions{SpoolDir: spoolDir}

	g := newGateway(discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer g.shutdown()

	if _, err := g.apply(ctx, mutate(t, opts, func(b *config.Bundle) {
		withServer(baseServer)(b)
		b.Admin = &config.Admin{MetricsToken: "good-token"}
	})); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// The same bundle the previous test uses to force a refusal, plus a token
	// that must not take effect.
	bad := mutate(t, opts, func(b *config.Bundle) {
		withServer(baseServer)(b)
		b.Relays["Fresh"] = b.Relays["Outbound"]
		routing := "version: 1\nroutes:\n  - name: to-fresh\n    match: {always: true}\n    then: [{action: relay, relay: Fresh}]\n"
		b.Routing = &routing
		b.Admin = &config.Admin{MetricsToken: "never-applied"}
	})
	if _, err := g.apply(ctx, bad); err == nil {
		t.Fatal("expected the apply to be refused")
	}

	if got := g.AdminToken(); got != "good-token" {
		t.Errorf("AdminToken() = %q, want the last-good token", got)
	}
}
