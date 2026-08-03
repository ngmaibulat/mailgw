package node

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
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
	if cached.VersionID != before.AppliedVersionID {
		t.Errorf("applied version moved to %d on a failed apply, want %d",
			cached.VersionID, before.AppliedVersionID)
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

// TestControl_FailedApplyDoesNotBecomeTheDesiredVersion.
//
// A rejected bundle must not leave desired_version_id pointing at itself, or the
// next restart comes up on a configuration that cannot apply and falls back —
// which looks, from a test's point of view, like the reset did not take.
//
// This is where injection deliberately differs from the pull loop: there,
// desired_version_id is the console's intent and is authoritative even when the
// bundle fails, because an operator has to be able to see what they asked for.
func TestControl_FailedApplyDoesNotBecomeTheDesiredVersion(t *testing.T) {
	n := nodeForTest(t)

	good := bundleListeningOn(t, "127.0.0.1:0")
	applied, err := n.ApplyBundle(context.Background(), good)
	if err != nil {
		t.Fatalf("the good bundle should apply: %v", err)
	}

	var b config.Bundle
	if err := json.Unmarshal(good, &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	bad := "version: 1\nroutes:\n  - name: n\n    match: {always: true}\n" +
		"    then: [{action: relay, relay: Ghost}]\n"
	b.Routing = &bad
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := n.ApplyBundle(context.Background(), raw); err == nil {
		t.Fatal("a bundle naming an undefined relay group was accepted")
	}

	desired, err := n.store.DesiredVersionID()
	if err != nil {
		t.Fatalf("DesiredVersionID: %v", err)
	}
	if desired != applied.VersionID {
		t.Errorf("desired version = %d after a failed apply, want the last good one %d",
			desired, applied.VersionID)
	}

	// And that is what a restart would boot.
	if boot := bootConfig(n.store, discardLogger()); boot == nil || boot.VersionID != applied.VersionID {
		t.Errorf("a restart would boot %+v, want version %d", boot, applied.VersionID)
	}
}

// TestControl_AppliedCarriesAnEmptyRestartList, not null: a JSON null there
// makes every caller special-case it.
func TestControl_AppliedCarriesAnEmptyRestartList(t *testing.T) {
	n := nodeForTest(t)
	applied, err := n.ApplyBundle(context.Background(), bundleListeningOn(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	if applied.RestartRequired == nil {
		t.Error("restart_required is nil; it must marshal as [] rather than null")
	}
}

// spoolEnvelope puts a minimal, valid envelope into the running gateway's
// spool, in the given queue.
//
// The three verbs below act on a spool a real gateway owns, so the fixture goes
// in through queue.Spool rather than by writing files: an envelope this package
// hand-wrote could fail validate() in a way the gateway never would, and the
// test would then be asserting on a shape nothing produces.
func spoolEnvelope(t *testing.T, n *Node, conn string, quarantined bool) string {
	t.Helper()

	sp := n.gw.Spool()
	if sp == nil {
		t.Fatal("no spool: apply a configuration first")
	}

	txn := conn + ".1"
	env := &queue.Envelope{
		UUID:     txn + ".1",
		TxnUUID:  txn,
		ConnUUID: conn,
		// Named for an ancestor, which is what Envelope.validate allows and what
		// lets gcBody rule out a referrer from its filename alone.
		Body:     txn + ".eml",
		MailFrom: "sender@example.test",
		Rcpts:    []queue.Recipient{{Addr: "rcpt@partner.test"}},
		RelayGrp: "Outbound",
		QueuedAt: time.Now().UnixMilli(),
		// Due in an hour, so the delivery runner these tests brought up leaves a
		// queued fixture alone. The fixture's relay group resolves to a real
		// internet host, and a unit test that let the runner claim its subject
		// would be racing a DNS lookup.
		NextAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	name, size, err := sp.WriteBody(txn, strings.NewReader("Subject: fixture\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatalf("write the body: %v", err)
	}
	env.Body, env.BodySize = name, size

	if quarantined {
		err = sp.Quarantine(env)
	} else {
		err = sp.Enqueue(env)
	}
	if err != nil {
		t.Fatalf("spool the envelope: %v", err)
	}
	return env.UUID
}

// queueOf returns the queue directory an envelope is currently in.
func queueOf(t *testing.T, n *Node, uuid string) string {
	t.Helper()
	entries, err := n.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	for _, e := range entries {
		if e.UUID == uuid {
			return e.Queue
		}
	}
	return ""
}

// TestControl_EnvelopeVerbsReachTheSpool wires the three verbs to the queue a
// running gateway owns.
//
// What each verb DOES is already pinned next door — Release rebuilding the ready
// filename, Hold being its inverse, Remove collecting the body
// (internal/queue/inspect_test.go). This is the adapter: that Release, Hold and
// Remove reach the live spool at all, and report a count for what moved.
//
// It matters because quarantine is a decision the ruleset can make and its only
// exit was `mailgw-go mailq release` — a CLI this binary deliberately does not
// carry, which made quarantine a one-way door for anything automated.
func TestControl_EnvelopeVerbsReachTheSpool(t *testing.T) {
	n := nodeForTest(t)
	if _, err := n.ApplyBundle(context.Background(), bundleListeningOn(t, "127.0.0.1:0")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}

	held := spoolEnvelope(t, n, "AAAAAAAA-0000-0000-0000-000000000001", true)
	if got := queueOf(t, n, held); got != queue.QueueQuarantine {
		t.Fatalf("fixture landed in %q, want quarantine", got)
	}
	if got, err := n.Release([]string{held}); err != nil || got != 1 {
		t.Fatalf("Release = %d, %v; want 1, nil", got, err)
	}
	// Released mail is due immediately and Release nudges the scheduler, so by
	// now the runner may already have claimed it. Anything other than quarantine
	// is the assertion: it left, and the scheduler could see it.
	if got := queueOf(t, n, held); got == queue.QueueQuarantine {
		t.Errorf("after Release the envelope is still quarantined")
	}

	// Hold works on an envelope that is not due for an hour, so the runner is
	// not competing for it and the outcome is exact.
	ready := spoolEnvelope(t, n, "AAAAAAAA-0000-0000-0000-000000000002", false)
	if got, err := n.Hold([]string{ready}); err != nil || got != 1 {
		t.Fatalf("Hold = %d, %v; want 1, nil", got, err)
	}
	if got := queueOf(t, n, ready); got != queue.QueueQuarantine {
		t.Errorf("after Hold the envelope is in %q, want quarantine", got)
	}

	// And Remove takes it out of the listing entirely.
	if got, err := n.Remove([]string{ready}); err != nil || got != 1 {
		t.Fatalf("Remove = %d, %v; want 1, nil", got, err)
	}
	if got := queueOf(t, n, ready); got != "" {
		t.Errorf("the envelope is still in %q after Remove", got)
	}
}

// TestControl_EnvelopeVerbsRefuseAnEmptyList is where these differ from Flush,
// on purpose.
//
// Flush with no uuids means "the whole ready queue", which is the gesture an
// operator makes after an outage. "Release everything ever quarantined" is not a
// gesture anybody wants to make by leaving a field out.
func TestControl_EnvelopeVerbsRefuseAnEmptyList(t *testing.T) {
	n := nodeForTest(t)
	if _, err := n.ApplyBundle(context.Background(), bundleListeningOn(t, "127.0.0.1:0")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}

	for name, op := range map[string]func([]string) (int, error){
		"Release": n.Release,
		"Hold":    n.Hold,
		"Remove":  n.Remove,
	} {
		if got, err := op(nil); err == nil {
			t.Errorf("%s(nil) = %d, nil; an empty list must be refused, not read as everything",
				name, got)
		}
	}
}

// TestControl_EnvelopeVerbsNeedASpool: before the first apply there is no spool,
// and the answer has to name that rather than surface as a nil dereference.
func TestControl_EnvelopeVerbsNeedASpool(t *testing.T) {
	n := nodeForTest(t)

	for name, op := range map[string]func([]string) (int, error){
		"Release": n.Release,
		"Hold":    n.Hold,
		"Remove":  n.Remove,
	} {
		got, err := op([]string{"whatever"})
		if err == nil {
			t.Errorf("%s before any configuration = %d, nil; want an error naming the missing spool",
				name, got)
			continue
		}
		if !strings.Contains(err.Error(), "no spool") {
			t.Errorf("%s error = %q, want it to name the missing spool", name, err)
		}
	}
}

// TestControl_PartialFailureReportsWhatMoved: an envelope claimed by a worker
// mid-call is an ordinary outcome, and a caller told only "error" would not know
// how much of its request took effect.
func TestControl_PartialFailureReportsWhatMoved(t *testing.T) {
	n := nodeForTest(t)
	if _, err := n.ApplyBundle(context.Background(), bundleListeningOn(t, "127.0.0.1:0")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}

	uuid := spoolEnvelope(t, n, "AAAAAAAA-0000-0000-0000-000000000004", true)

	got, err := n.Release([]string{uuid, "no-such-envelope"})
	if err == nil {
		t.Fatal("Release of a missing uuid returned no error")
	}
	if got != 1 {
		t.Errorf("Release = %d, want 1 — the one that moved must still be counted", got)
	}
}

// TestControl_StatusReportsTheBoundAdminAddress, for exactly the reason
// Listeners does: an admin listener asked for on port 0 is unreachable unless
// the bound address comes back out, and /metrics, /readyz and /healthz all live
// there.
func TestControl_StatusReportsTheBoundAdminAddress(t *testing.T) {
	n := nodeForTest(t)

	// New binds nothing, so there is no address yet — and the honest answer is
	// empty rather than the address that was requested.
	if got := n.Status().AdminAddr; got != "" {
		t.Errorf("admin_addr before Run = %q, want empty", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = n.ui.ListenAndServe(ctx, "127.0.0.1:0") }()

	deadline := time.Now().Add(5 * time.Second)
	for n.Status().AdminAddr == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	addr := n.Status().AdminAddr
	if addr == "" {
		t.Fatal("admin_addr is still empty after the admin UI started listening")
	}
	if strings.HasSuffix(addr, ":0") {
		t.Fatalf("admin_addr = %q is the requested port, not the bound one", addr)
	}

	// Reported is not enough: it has to answer.
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz on the reported admin address: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
}

// TestControl_ApplyBundleDoesNotTieTheGatewayToTheRequest is the defect the
// Tier-B delivery suite found on its first run.
//
// applyCached's context governs the lifetime of everything bringUp starts — the
// delivery runner and the failed-events replayer. Every other caller passes a
// process-lifetime context: boot, the poll loop, the WebSocket, SIGHUP. This one
// is reached from an HTTP handler, and passing r.Context() through cancelled the
// runner the instant the response was written, so the gateway accepted mail and
// delivered none of it — visible only to a test that actually delivers.
func TestControl_ApplyBundleDoesNotTieTheGatewayToTheRequest(t *testing.T) {
	n := nodeForTest(t)

	// A context that is already finished, standing in for a request whose
	// response has been written.
	reqCtx, cancel := context.WithCancel(context.Background())
	if _, err := n.ApplyBundle(reqCtx, bundleListeningOn(t, "127.0.0.1:0")); err != nil {
		t.Fatalf("ApplyBundle: %v", err)
	}
	cancel()

	// Give a cancellation the chance to propagate, had one been wired up.
	time.Sleep(50 * time.Millisecond)

	// The runner is the thing that would have died. Nudging a dead one is
	// silent, so the observable proof is that it still claims work: an envelope
	// that is due must leave the ready queue.
	uuid := spoolEnvelope(t, n, "AAAAAAAA-0000-0000-0000-00000000000A", false)
	if _, err := n.Flush([]string{uuid}); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if queueOf(t, n, uuid) != queue.QueueReady {
			return // Claimed: the runner is alive.
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the envelope was never claimed; the delivery runner was cancelled with the request " +
		"that applied the configuration")
}
