package node

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/central"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// fakeApplier stands in for the gateway so these tests never open a socket or
// create a spool. What matters here is the bookkeeping around apply, not apply
// itself — that is gateway_test.go's job.
type fakeApplier struct {
	mu      sync.Mutex
	calls   int
	last    *Loaded
	restart []string
	err     error
}

func (f *fakeApplier) apply(_ context.Context, l *Loaded) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.restart, f.err
	}
	f.last = l
	return f.restart, nil
}

func (f *fakeApplier) snapshot() (int, *Loaded) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.last
}

// console is a stub Central Management that answers the three routes the pull
// loop uses. It counts config fetches, which is how the rollback test proves
// cached bytes were reused.
type console struct {
	t *testing.T

	mu            sync.Mutex
	desiredID     int64
	desiredSHA    string
	desiredNumber int
	bundles       map[int64][]byte
	configHits    int
	configStatus  int
	reports       []json.RawMessage
}

func newConsole(t *testing.T) *console {
	return &console{t: t, bundles: map[int64][]byte{}}
}

func (c *console) deploy(versionID int64, version int, sha string, bundle []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bundles[versionID] = bundle
	c.desiredID, c.desiredNumber, c.desiredSHA = versionID, version, sha
}

func (c *console) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /agent/status", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		writeJSON(w, map[string]any{
			"status":             "ok",
			"approval":           "approved",
			"desired_version_id": c.desiredID,
			"desired_version":    c.desiredNumber,
			"bundle_sha256":      c.desiredSHA,
		})
	})

	mux.HandleFunc("GET /agent/config", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.configHits++
		if c.configStatus != 0 {
			w.WriteHeader(c.configStatus)
			writeJSON(w, map[string]any{"status": "error", "message": "nope"})
			return
		}
		writeJSON(w, map[string]any{
			"status":        "ok",
			"version_id":    c.desiredID,
			"version":       c.desiredNumber,
			"bundle_sha256": c.desiredSHA,
			"bundle":        json.RawMessage(c.bundles[c.desiredID]),
		})
	})

	mux.HandleFunc("POST /agent/report", func(w http.ResponseWriter, r *http.Request) {
		// io.ReadAll rather than a single Read into a ContentLength-sized
		// buffer: one Read is not obliged to fill it, and the report grew a
		// metrics object in M6 that pushes it past the size where that
		// reliably happened to work.
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reports = append(c.reports, body)
		c.mu.Unlock()
		writeJSON(w, map[string]any{"status": "ok"})
	})

	return mux
}

func (c *console) hits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.configHits
}

func (c *console) lastReport() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reports) == 0 {
		c.t.Fatal("no report was sent")
	}
	var out map[string]any
	if err := json.Unmarshal(c.reports[len(c.reports)-1], &out); err != nil {
		c.t.Fatalf("report is not JSON: %v (%s)", err, c.reports[len(c.reports)-1])
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// pullFixture wires an agent to a stub console and a real temp store.
func pullFixture(t *testing.T) (*agent, *console, *fakeApplier, *central.Client, func()) {
	t.Helper()

	c := newConsole(t)
	srv := httptest.NewServer(c.handler())

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	fa := &fakeApplier{}
	a := newAgent(st, "test", discardLogger())
	a.gw = fa

	client := &central.Client{BaseURL: srv.URL, ID: "gw-1", Key: key, Log: discardLogger()}

	return a, c, fa, client, func() {
		srv.Close()
		_ = st.Close()
	}
}

func status(c *console) *central.StatusResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, sha := c.desiredID, c.desiredSHA
	return &central.StatusResponse{Approval: "approved", DesiredVersionID: &id, BundleSHA256: &sha}
}

func TestPull_AppliesANewVersionAndReportsIt(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	bundle := bundleFromTestdata(t)
	c.deploy(42, 7, "sha-of-v7", bundle)

	ctx := context.Background()
	a.pull(ctx, client, status(c))

	calls, last := fa.snapshot()
	if calls != 1 {
		t.Fatalf("apply called %d times, want 1", calls)
	}
	if last == nil || last.cfg == nil {
		t.Fatal("apply was handed nothing usable")
	}

	// Cached, and marked applied — the applied row is what a restart boots from.
	applied, err := a.store.AppliedConfig()
	if err != nil || applied == nil {
		t.Fatalf("AppliedConfig: %v, %v", applied, err)
	}
	if applied.VersionID != 42 || applied.Version != 7 || applied.SHA256 != "sha-of-v7" {
		t.Errorf("cached the wrong thing: %+v", applied)
	}

	a.report(ctx, client)
	r := c.lastReport()
	if got, ok := r["applied_version_id"].(float64); !ok || int64(got) != 42 {
		t.Errorf("applied_version_id = %v, want 42", r["applied_version_id"])
	}
	// Explicit null, not absent: the console merges on !== undefined, so an
	// omitted field could never clear a stale error.
	if v, present := r["apply_error"]; !present || v != nil {
		t.Errorf("apply_error should be an explicit null, got %#v (present=%v)", v, present)
	}
	if v, present := r["restart_required"]; !present || v != false {
		t.Errorf("restart_required should be present and false, got %#v (present=%v)", v, present)
	}
}

// A version already applied must not be fetched again on every tick.
func TestPull_SameVersionIsANoOp(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	c.deploy(42, 7, "sha-of-v7", bundleFromTestdata(t))
	ctx := context.Background()

	a.pull(ctx, client, status(c))
	a.pull(ctx, client, status(c))
	a.pull(ctx, client, status(c))

	if calls, _ := fa.snapshot(); calls != 1 {
		t.Errorf("apply called %d times, want 1", calls)
	}
	if c.hits() != 1 {
		t.Errorf("the console was asked for the config %d times, want 1", c.hits())
	}
}

// A bundle that does not compile must leave the running configuration alone and
// put the compiler's message where the console can show it.
func TestPull_BadBundleKeepsTheRunningConfiguration(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	ctx := context.Background()

	// First, a good one, so there is something to keep running.
	c.deploy(1, 1, "sha-v1", bundleFromTestdata(t))
	a.pull(ctx, client, status(c))
	_, good := fa.snapshot()

	// Then one whose ruleset names a field that does not exist.
	var b map[string]any
	if err := json.Unmarshal(bundleFromTestdata(t), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b["routing"] = "version: 1\nroutes:\n  - name: x\n    match: {field: mail.nosuchfield, op: eq, value: a}\n    then: [{action: relay, relay: Outbound}]\n"
	broken, _ := json.Marshal(b)
	c.deploy(2, 2, "sha-v2", broken)

	a.pull(ctx, client, status(c))

	calls, last := fa.snapshot()
	if calls != 1 {
		t.Errorf("a bundle that does not compile must never reach apply (calls=%d)", calls)
	}
	if last != good {
		t.Error("the previously applied configuration was replaced")
	}

	// Still on v1, and v2 carries the reason.
	applied, _ := a.store.AppliedConfig()
	if applied == nil || applied.VersionID != 1 {
		t.Errorf("applied version = %+v, want v1 to still be running", applied)
	}
	failed, _ := a.store.ConfigByVersionID(2)
	if failed == nil || failed.ApplyError == "" {
		t.Fatalf("v2 should carry an apply error, got %+v", failed)
	}
	if !strings.Contains(failed.ApplyError, "nosuchfield") {
		t.Errorf("the compiler's message should survive: %q", failed.ApplyError)
	}

	a.report(ctx, client)
	r := c.lastReport()
	if got, ok := r["applied_version_id"].(float64); !ok || int64(got) != 1 {
		t.Errorf("applied_version_id = %v, want 1", r["applied_version_id"])
	}
	msg, ok := r["apply_error"].(string)
	if !ok || !strings.Contains(msg, "nosuchfield") {
		t.Errorf("apply_error = %#v, want the compiler's message", r["apply_error"])
	}
}

// The console's schema rejects an apply_error over 4096 characters, and a
// rejected report loses the heartbeat too.
func TestPull_ApplyErrorIsTruncated(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	fa.err = &longError{n: 20000}
	c.deploy(1, 1, "sha-v1", bundleFromTestdata(t))

	ctx := context.Background()
	a.pull(ctx, client, status(c))

	failed, _ := a.store.ConfigByVersionID(1)
	if failed == nil {
		t.Fatal("the failed version should still be cached")
	}
	if len(failed.ApplyError) > maxApplyError+len("… (truncated)") {
		t.Errorf("apply error is %d bytes, want it capped near %d", len(failed.ApplyError), maxApplyError)
	}
	if !strings.HasSuffix(failed.ApplyError, "(truncated)") {
		t.Error("truncation should be visible rather than silent")
	}
}

// A partial apply still counts as applied: boot reads the applied row, so
// leaving it unmarked would make the restart the console just asked for boot
// straight back into the old configuration.
func TestPull_RestartRequiredStillMarksApplied(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	fa.restart = []string{"relays"}
	c.deploy(9, 3, "sha-v3", bundleFromTestdata(t))

	ctx := context.Background()
	a.pull(ctx, client, status(c))

	applied, _ := a.store.AppliedConfig()
	if applied == nil || applied.VersionID != 9 {
		t.Fatalf("a partial apply must still be recorded, got %+v", applied)
	}

	a.report(ctx, client)
	if v := c.lastReport()["restart_required"]; v != true {
		t.Errorf("restart_required = %#v, want true", v)
	}

	// ...and it must go back to false once a later configuration needs nothing,
	// or the console's banner would be permanent.
	fa.restart = nil
	c.deploy(10, 4, "sha-v4", bundleFromTestdata(t))
	a.pull(ctx, client, status(c))
	a.report(ctx, client)
	if v, present := c.lastReport()["restart_required"]; !present || v != false {
		t.Errorf("restart_required = %#v (present=%v), want false", v, present)
	}
}

// Rollback is the console repointing at a version whose bytes we already hold.
// It must not need a fetch, which is what makes what runs afterwards
// byte-identical to what ran before.
func TestPull_RollbackReusesCachedBytes(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	ctx := context.Background()
	v1 := bundleFromTestdata(t)

	c.deploy(1, 1, "sha-v1", v1)
	a.pull(ctx, client, status(c))

	c.deploy(2, 2, "sha-v2", v1) // different version, same content: fine
	a.pull(ctx, client, status(c))

	hitsBefore := c.hits()

	// Roll back: desired points at v1 again, whose bytes are still cached.
	c.deploy(1, 1, "sha-v1", v1)
	a.pull(ctx, client, status(c))

	if c.hits() != hitsBefore {
		t.Errorf("rollback fetched the config again (%d -> %d); it should reuse the cache",
			hitsBefore, c.hits())
	}
	if calls, _ := fa.snapshot(); calls != 3 {
		t.Errorf("apply called %d times, want 3", calls)
	}
	applied, _ := a.store.AppliedConfig()
	if applied == nil || applied.VersionID != 1 {
		t.Errorf("applied version = %+v, want v1", applied)
	}

	// And the rollback has to survive a restart. bootConfig prefers the most
	// recently fetched bundle, so reusing cached bytes must still refresh that
	// timestamp — otherwise the next boot would come back up on v2, silently
	// undoing what the operator just did.
	if boot := bootConfig(a.store, discardLogger()); boot == nil || boot.VersionID != 1 {
		t.Errorf("boot would come up on %+v, want v1", boot)
	}
}

// A console that is down or answering errors must change nothing at all.
func TestPull_ConsoleFailureChangesNothing(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	c.deploy(1, 1, "sha-v1", bundleFromTestdata(t))
	c.configStatus = http.StatusInternalServerError

	a.pull(context.Background(), client, status(c))

	if calls, _ := fa.snapshot(); calls != 0 {
		t.Errorf("apply called %d times, want 0", calls)
	}
	if applied, _ := a.store.AppliedConfig(); applied != nil {
		t.Errorf("nothing should have been applied, got %+v", applied)
	}
	if cached, _ := a.store.LatestConfig(); cached != nil {
		t.Errorf("nothing should have been cached, got %+v", cached)
	}
}

// A version that failed is not re-fetched on the next tick: re-failing it every
// 15 seconds buries the real error and hammers the console for nothing.
func TestPull_FailedVersionBacksOff(t *testing.T) {
	a, c, fa, client, done := pullFixture(t)
	defer done()

	fa.err = &longError{n: 10}
	c.deploy(1, 1, "sha-v1", bundleFromTestdata(t))

	ctx := context.Background()
	a.pull(ctx, client, status(c))
	a.pull(ctx, client, status(c))
	a.pull(ctx, client, status(c))

	if calls, _ := fa.snapshot(); calls != 1 {
		t.Errorf("apply attempted %d times, want 1 within the backoff window", calls)
	}
	if c.hits() != 1 {
		t.Errorf("the console was asked %d times, want 1", c.hits())
	}
}

// bootConfig follows the console's recorded intent, not a timestamp heuristic.
func TestBootConfig(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	log := discardLogger()
	save := func(id int64, v int, sha string) {
		t.Helper()
		if err := st.SaveConfig(&store.CachedConfig{
			VersionID: id, Version: v, SHA256: sha, Bundle: []byte("{}"),
		}); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
	}

	if got := bootConfig(st, log); got != nil {
		t.Errorf("an empty cache should yield nothing, got %+v", got)
	}

	// Nothing heard from a console yet: run what last applied.
	save(1, 1, "a")
	if err := st.MarkApplied(1, time.Now()); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	if got := bootConfig(st, log); got == nil || got.VersionID != 1 {
		t.Fatalf("want v1, got %+v", got)
	}

	// v2 arrived, applied, and reported restart_required. This is that restart:
	// it must come up on v2, or the operator's action did nothing.
	save(2, 2, "b")
	if err := st.MarkApplied(2, time.Now()); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	if err := st.SetDesiredVersionID(2); err != nil {
		t.Fatalf("SetDesiredVersionID: %v", err)
	}
	if got := bootConfig(st, log); got == nil || got.VersionID != 2 {
		t.Fatalf("want v2, got %+v", got)
	}

	// The operator rolls back to v1. Even though v2 was applied more recently
	// and cached later, boot must honour the rollback.
	if err := st.SetDesiredVersionID(1); err != nil {
		t.Fatalf("SetDesiredVersionID: %v", err)
	}
	if got := bootConfig(st, log); got == nil || got.VersionID != 1 {
		t.Fatalf("want the rolled-back v1, got %+v", got)
	}

	// The console wants something we never managed to fetch: fall back to the
	// last configuration that applied cleanly (v2 here) rather than to nothing.
	if err := st.SetDesiredVersionID(99); err != nil {
		t.Fatalf("SetDesiredVersionID: %v", err)
	}
	if got := bootConfig(st, log); got == nil || got.VersionID != 2 {
		t.Fatalf("want a fallback to the applied v2, got %+v", got)
	}
}

// AppliedConfig must answer "which one is running now", not "which has the
// bigger id". A rollback lands in the same second as the deploy it undoes, so
// the timestamp alone cannot decide it.
func TestAppliedConfig_RollbackWithinTheSameSecond(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	now := time.Now()
	for _, c := range []struct {
		id int64
		v  int
	}{{1, 1}, {2, 2}} {
		if err := st.SaveConfig(&store.CachedConfig{
			VersionID: c.id, Version: c.v, SHA256: "x", Bundle: []byte("{}"),
		}); err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
	}

	// Deploy v2, then roll back to v1, both stamped with the same second.
	if err := st.MarkApplied(2, now); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	if err := st.MarkApplied(1, now); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	applied, err := st.AppliedConfig()
	if err != nil {
		t.Fatalf("AppliedConfig: %v", err)
	}
	if applied == nil || applied.VersionID != 1 {
		t.Fatalf("applied = %+v, want v1 — the rollback, not the version it undid", applied)
	}
}

// longError produces a message of a known length without a huge literal.
type longError struct{ n int }

func (e *longError) Error() string { return strings.Repeat("x", e.n) }

// The heartbeat carries the counter snapshot. The console already validates and
// (from M6) stores it, so the keys here are a cross-service contract.
func TestReport_CarriesTheMetricsSnapshot(t *testing.T) {
	a, c, _, client, done := pullFixture(t)
	defer done()

	a.metrics = obs.New()
	a.metrics.MsgAccepted.Add(3)
	a.metrics.DeliverOK.Add(11)

	a.report(context.Background(), client)

	raw, ok := c.lastReport()["metrics"]
	if !ok {
		t.Fatal("the report carried no metrics object")
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("metrics is %T, want an object", raw)
	}

	if got := m["msg_accepted"]; got != float64(3) {
		t.Errorf("msg_accepted = %v, want 3", got)
	}
	if got := m["deliver_ok"]; got != float64(11) {
		t.Errorf("deliver_ok = %v, want 11", got)
	}

	// Every counter is reported, always. A mismatch here means a counter was
	// added to internal/obs without the console side being considered — the
	// failure mode where a new metric silently never reaches the fleet view.
	if want := len(obs.New().Snapshot()); len(m) != want {
		t.Errorf("the report carries %d metrics keys, want %d", len(m), want)
	}
}

// An agent with no registry still reports; the field is simply absent.
func TestReport_WithoutMetricsIsStillValid(t *testing.T) {
	a, c, _, client, done := pullFixture(t)
	defer done()

	a.report(context.Background(), client)

	if _, present := c.lastReport()["metrics"]; present {
		t.Error("metrics must be omitted, not sent empty, when there is no registry")
	}
}

// One report per poll. The heartbeat deliberately has no ticker of its own, and
// this is what notices if someone adds one.
func TestReport_OneReportPerPoll(t *testing.T) {
	a, c, _, client, done := pullFixture(t)
	defer done()

	a.metrics = obs.New()
	const polls = 3
	for range polls {
		a.report(context.Background(), client)
	}

	c.mu.Lock()
	got := len(c.reports)
	c.mu.Unlock()
	if got != polls {
		t.Errorf("%d polls produced %d reports, want %d", polls, got, polls)
	}
}

// TestReport_InjectedVersionIsNotReportedToTheConsole.
//
// applied_version_id is a ConfigVersions.id — the console's own positive
// autoincrement — and its schema is `positive().nullable()`. A configuration
// injected through the test control API has no row there, and its id is
// deliberately negative so the two spaces cannot collide.
//
// Reporting it anyway made the console answer 400 to EVERY heartbeat, so an
// enrolled gateway that had ever been injected into logged "cannot reach
// Central Management" every 15 seconds and went stale in the fleet view. Null
// is the answer the field already documents for a node running something the
// console did not issue.
func TestReport_InjectedVersionIsNotReportedToTheConsole(t *testing.T) {
	a, c, _, client, done := pullFixture(t)
	defer done()

	injected := &store.CachedConfig{
		VersionID: -5717624045415299225, // as injectedVersionID produces
		Version:   1,
		SHA256:    "deadbeef",
		Bundle:    []byte(`{"format":1}`),
		FetchedAt: time.Now(),
	}
	if err := a.store.SaveConfig(injected); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := a.store.MarkApplied(injected.VersionID, time.Now()); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	a.report(context.Background(), client)

	got, present := c.lastReport()["applied_version_id"]
	if !present {
		t.Fatal("applied_version_id must be sent as an explicit null, not omitted: " +
			"the console merges on `!== undefined`, so a missing field cannot clear a stale value")
	}
	if got != nil {
		t.Errorf("applied_version_id = %v, want null; the console's schema is "+
			"positive().nullable() and an injected id is negative", got)
	}
}

// The console's own versions are still reported, which is the whole point of
// the field.
func TestReport_ConsoleVersionIsReported(t *testing.T) {
	a, c, _, client, done := pullFixture(t)
	defer done()

	deployed := &store.CachedConfig{
		VersionID: 42,
		Version:   7,
		SHA256:    "cafe",
		Bundle:    []byte(`{"format":1}`),
		FetchedAt: time.Now(),
	}
	if err := a.store.SaveConfig(deployed); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := a.store.MarkApplied(deployed.VersionID, time.Now()); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	a.report(context.Background(), client)

	if got := c.lastReport()["applied_version_id"]; got != float64(42) {
		t.Errorf("applied_version_id = %v, want 42", got)
	}
}
