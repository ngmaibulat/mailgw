package node

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
)

// These are the tests that could not be written before M19 moved this package
// out of package main.
//
// New() runs the real bring-up — the spool, the events client, the delivery
// runner, the SMTP server and the full listener chain (proxyproto -> Meter ->
// tls -> Guard -> Throttle -> Limit) — and nothing here substitutes a literal
// for any of it. That is the gap docs/internal/dev/testing.md names and that M16
// found the hard way: M11's connection cap passed every package test and still
// broke an implicit_tls listener, because no test went through the wiring.
//
// Note New() binds no socket, so these do not need the admin UI to be listening.

// nodeForTest builds a node on a throwaway data directory.
func nodeForTest(t *testing.T) *Node {
	t.Helper()

	n, err := New(Options{
		DataDir: t.TempDir(),
		// Never bound: these tests call ApplyBundle directly rather than Run.
		AdminAddr: "127.0.0.1:0",
		Version:   "test",
		Commit:    "none",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(n.Close)
	// The bring-up logs at Info on every apply; keep the test output readable.
	n.log = discardLogger()
	n.gw.log = discardLogger()
	n.agent.log = discardLogger()
	return n
}

// TestControl_ApplyBundleBindsAnEphemeralPortAndReportsIt is the point of the
// milestone in one test.
//
// A bundle asking for port 0 is the case a test suite actually wants — parallel
// cases cannot share 2525 — and until Status() reported bound addresses there
// was no way to find out what the kernel chose. freeAddr() elsewhere in this
// package reserves and releases a port instead, which is racy by construction;
// this is the answer to it.
func TestControl_ApplyBundleBindsAnEphemeralPortAndReportsIt(t *testing.T) {
	n := nodeForTest(t)

	if n.Status().Serving {
		t.Fatal("a gateway with no configuration must not be serving")
	}
	if got := n.Status().Listeners; len(got) != 0 {
		t.Fatalf("listeners before any apply = %v, want none", got)
	}

	applied, err := n.ApplyBundle(context.Background(), bundleListeningOn(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if applied.VersionID >= 0 {
		t.Errorf("injected version_id = %d, want a negative id so it cannot collide "+
			"with the console's positive autoincrement", applied.VersionID)
	}

	st := n.Status()
	if !st.Serving {
		t.Fatal("gateway is not serving after a configuration applied")
	}
	if len(st.Listeners) != 1 {
		t.Fatalf("listeners = %v, want exactly one", st.Listeners)
	}
	if strings.HasSuffix(st.Listeners[0], ":0") {
		t.Fatalf("listener address %q is still the requested port, not the bound one", st.Listeners[0])
	}
	if st.Build != "test-only" {
		t.Errorf("build = %q, want test-only so a caller cannot mistake this for a shipped node", st.Build)
	}

	// The address is not merely reported — it answers SMTP.
	conn, err := net.DialTimeout("tcp", st.Listeners[0], 5*time.Second)
	if err != nil {
		t.Fatalf("dial the reported listener %s: %v", st.Listeners[0], err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	banner, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the greeting: %v", err)
	}
	if !strings.HasPrefix(banner, "220 ") {
		t.Errorf("greeting = %q, want a 220", banner)
	}
}

// TestControl_ApplyBundleIsIdempotentForIdenticalBytes pins the version-id
// derivation.
//
// Re-injecting an unchanged bundle must reuse its row rather than pile up a new
// one, which is the behaviour the console already has for redeploying an
// unchanged configuration. A counter would have given two ids for one
// configuration and made "what is this node running?" ambiguous after a
// re-apply.
func TestControl_ApplyBundleIsIdempotentForIdenticalBytes(t *testing.T) {
	n := nodeForTest(t)
	raw := bundleListeningOn(t, "127.0.0.1:0")

	first, err := n.ApplyBundle(context.Background(), raw)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := n.ApplyBundle(context.Background(), raw)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if first.VersionID != second.VersionID {
		t.Errorf("same bundle got version ids %d and %d; identical bytes must be one version",
			first.VersionID, second.VersionID)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("digest changed between applies: %s vs %s", first.SHA256, second.SHA256)
	}

	// A bundle that differs by one byte must not land on the same row.
	var b config.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	server := "hostname: other.test\nlisten:\n  - addr: \"127.0.0.1:0\"\n"
	b.Server = &server
	changed, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	other, err := n.ApplyBundle(context.Background(), changed)
	if err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if other.VersionID == first.VersionID {
		t.Error("two different bundles share a version id")
	}
}

// TestControl_BadBundleKeepsTheRunningConfiguration is the fail-closed contract,
// asserted through the injection path rather than assumed from the pull path.
//
// A bundle the rule compiler rejects must leave the gateway serving what it was
// already serving. The injection path is a new caller of applyCached, and a new
// caller is exactly where that contract could have been lost.
func TestControl_BadBundleKeepsTheRunningConfiguration(t *testing.T) {
	n := nodeForTest(t)

	if _, err := n.ApplyBundle(context.Background(), bundleListeningOn(t, "127.0.0.1:0")); err != nil {
		t.Fatalf("the good bundle should apply: %v", err)
	}
	before := n.Status()

	// A route naming a relay group the bundle does not define. ruleset.Compile
	// refuses it, which is the same failure a console typo produces.
	var b config.Bundle
	if err := json.Unmarshal(bundleListeningOn(t, "127.0.0.1:0"), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	bad := "version: 1\nroutes:\n  - name: Nope\n    match: {always: true}\n" +
		"    then: [{action: relay, relay: NoSuchGroup}]\n"
	b.Routing = &bad
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := n.ApplyBundle(context.Background(), raw); err == nil {
		t.Fatal("a bundle naming an undefined relay group was accepted")
	} else if !strings.Contains(err.Error(), "NoSuchGroup") {
		t.Errorf("error %q does not name the group that is wrong; an operator cannot act on it", err)
	}

	after := n.Status()
	if !after.Serving {
		t.Error("a rejected bundle stopped the gateway serving; it must keep the last good configuration")
	}
	if len(after.Listeners) != len(before.Listeners) || after.Listeners[0] != before.Listeners[0] {
		t.Errorf("listeners changed on a rejected bundle: %v -> %v", before.Listeners, after.Listeners)
	}

	// And the reason reached the store, which is how it reaches the console.
	cached, err := n.AppliedBundle()
	if err != nil {
		t.Fatalf("AppliedBundle: %v", err)
	}
	if cached.VersionID != before.AppliedVersonID {
		t.Errorf("applied version moved to %d on a failed apply, want %d",
			cached.VersionID, before.AppliedVersonID)
	}
}

// TestControl_ProfilesComposeTheSameBundle checks that the ergonomic endpoint is
// sugar and not a second format: the profile texts must produce a document
// config.ParseBundle accepts, with no key the bundle path does not already have.
func TestControl_ProfilesComposeTheSameBundle(t *testing.T) {
	p := Profiles{
		Server:    "hostname: relay.test\nlisten:\n  - addr: \"127.0.0.1:0\"\n",
		Routing:   "version: 1\nroutes:\n  - name: d\n    match: {always: true}\n    then: [{action: relay, relay: Outbound}]\n",
		Allowlist: json.RawMessage(`{"allowed": [], "allow_all": true}`),
		Relays: map[string][]RelayProfile{
			"Outbound": {json.RawMessage(`{"name":"a","exchange":"127.0.0.1","port":25}`)},
		},
	}

	raw, err := p.Bundle()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	parsed, err := config.ParseBundle(raw)
	if err != nil {
		t.Fatalf("the composed document is not a valid bundle: %v", err)
	}
	if parsed.Format != config.BundleFormat {
		t.Errorf("format = %d, want %d", parsed.Format, config.BundleFormat)
	}

	n := nodeForTest(t)
	if _, err := n.ApplyBundle(context.Background(), raw); err != nil {
		t.Fatalf("a composed bundle must apply like any other: %v", err)
	}
	if !n.Status().Serving {
		t.Error("gateway is not serving after applying a composed bundle")
	}
}

// TestControl_RejectsJunk keeps the obvious operator mistakes readable rather
// than surfacing them as a complaint about a missing format field.
func TestControl_RejectsJunk(t *testing.T) {
	n := nodeForTest(t)

	for _, tc := range []struct{ name, body, want string }{
		{"empty", "", "empty"},
		{"not json", "hostname: relay.test", "not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := n.ApplyBundle(context.Background(), []byte(tc.body))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestNew_RefusesWithoutAnAdminAddress: a node nobody can provision is a node
// that can never become useful, and it exits 2 rather than 1 because typing the
// flag wrong is a usage error.
func TestNew_RefusesWithoutAnAdminAddress(t *testing.T) {
	if _, err := New(Options{DataDir: t.TempDir()}); err != ErrNoAdminAddr {
		t.Fatalf("New with no -admin returned %v, want ErrNoAdminAddr", err)
	}
	if got := Serve(context.Background(), Options{DataDir: t.TempDir()}); got != 2 {
		t.Errorf("exit code = %d, want 2 for a usage error", got)
	}
}
