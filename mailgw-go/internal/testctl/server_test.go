package testctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/node"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// These cover routing, decoding and error mapping against a fake Control.
//
// The cases worth having here are the malformed ones a real *node.Node would
// never produce — junk bodies, missing fields, an upstream that refuses — and a
// fake is the only way to reach them without contriving a broken gateway.
// internal/node's control_test.go covers the real behaviour behind them.

type fakeControl struct {
	applied     []byte
	applyErr    error
	enrolled    adminui.Settings
	enrollErr   error
	cached      *store.CachedConfig
	cachedErr   error
	queue       []node.QueueEntry
	queueErr    error
	flushed     []string
	flushN      int
	flushErr    error
	resetCalled bool
	resetErr    error

	// The three envelope verbs share one shape, so they share one record of
	// what they were asked to do: which verb, and with which uuids.
	verb    string
	verbArg []string
	verbN   int
	verbErr error
}

func (f *fakeControl) ApplyBundle(_ context.Context, raw []byte) (node.Applied, error) {
	f.applied = raw
	if f.applyErr != nil {
		return node.Applied{}, f.applyErr
	}
	return node.Applied{VersionID: -7, Version: 1, SHA256: "abc", RestartRequired: []string{}}, nil
}

func (f *fakeControl) AppliedBundle() (*store.CachedConfig, error) {
	return f.cached, f.cachedErr
}

func (f *fakeControl) Enroll(_ context.Context, s adminui.Settings) error {
	f.enrolled = s
	return f.enrollErr
}

func (f *fakeControl) Status() node.Status {
	return node.Status{
		Build:     "test-only",
		Serving:   true,
		Listeners: []string{"127.0.0.1:31337"},
		AdminAddr: "127.0.0.1:31338",
	}
}

func (f *fakeControl) Queue() ([]node.QueueEntry, error) { return f.queue, f.queueErr }

func (f *fakeControl) Flush(uuids []string) (int, error) {
	f.flushed = uuids
	return f.flushN, f.flushErr
}

func (f *fakeControl) Release(uuids []string) (int, error) { return f.record("release", uuids) }
func (f *fakeControl) Hold(uuids []string) (int, error)    { return f.record("hold", uuids) }
func (f *fakeControl) Remove(uuids []string) (int, error)  { return f.record("remove", uuids) }

func (f *fakeControl) record(verb string, uuids []string) (int, error) {
	f.verb, f.verbArg = verb, uuids
	return f.verbN, f.verbErr
}

func (f *fakeControl) Reset(context.Context) error {
	f.resetCalled = true
	return f.resetErr
}

func serve(t *testing.T, c Control) *httptest.Server {
	t.Helper()
	s := &Server{Control: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestApplyBundle_PassesTheBodyThroughVerbatim is the load-bearing property of
// the whole API: the bytes a test sends must be the bytes the gateway applies.
//
// Anything that rewrote, re-marshalled or normalised the body here would make
// the suite exercise a document no console would ever send — which is the
// mistake M18 recorded when it rejected seeding MariaDB directly.
func TestApplyBundle_PassesTheBodyThroughVerbatim(t *testing.T) {
	f := &fakeControl{}
	srv := serve(t, f)

	body := `{"format":1,"server":"hostname: relay.test\n","logging":{}}`
	code, out := post(t, srv, "/testctl/config", body)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}
	if string(f.applied) != body {
		t.Errorf("gateway was given\n  %s\nwant the request body verbatim\n  %s", f.applied, body)
	}

	var applied node.Applied
	if err := json.Unmarshal([]byte(out), &applied); err != nil {
		t.Fatalf("response is not an Applied: %v (%s)", err, out)
	}
	if applied.VersionID != -7 {
		t.Errorf("version_id = %d, want the control's answer passed through", applied.VersionID)
	}
}

// TestApplyBundle_ReportsTheReasonVerbatim: the compiler's message is the single
// most useful thing this API returns, and paraphrasing it would throw away the
// reason a test failed.
func TestApplyBundle_ReportsTheReasonVerbatim(t *testing.T) {
	f := &fakeControl{applyErr: errors.New(`rule "Nope": unknown relay group "NoSuchGroup"`)}
	srv := serve(t, f)

	code, out := post(t, srv, "/testctl/config", `{"format":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", code, out)
	}
	if !strings.Contains(out, "NoSuchGroup") {
		t.Errorf("response %s does not name the group that is wrong", out)
	}
}

// TestApplyProfiles_ComposesABundle checks the ergonomic endpoint reaches the
// same ApplyBundle with a real bundle document rather than the profile texts.
func TestApplyProfiles_ComposesABundle(t *testing.T) {
	f := &fakeControl{}
	srv := serve(t, f)

	code, out := post(t, srv, "/testctl/config/profiles", `{
	  "server": "hostname: relay.test\n",
	  "routing": "version: 1\n",
	  "allowlist": {"allowed": [], "allow_all": true},
	  "relays": {"Outbound": [{"name":"a","exchange":"127.0.0.1","port":25}]}
	}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}

	var doc map[string]any
	if err := json.Unmarshal(f.applied, &doc); err != nil {
		t.Fatalf("what reached the gateway is not JSON: %v (%s)", err, f.applied)
	}
	if doc["format"] == nil {
		t.Error("the composed document has no format key, so ParseBundle would refuse it")
	}
	if doc["server"] != "hostname: relay.test\n" {
		t.Errorf("server text was altered: %v", doc["server"])
	}
	// logging is always present: a gateway with no logservice URLs is legal and
	// the zero value is what expresses it.
	if _, ok := doc["logging"]; !ok {
		t.Error("the composed document omits logging entirely")
	}
}

// TestFlush_EmptyBodyMeansEverything keeps `curl -XPOST .../flush` with no data
// doing the obvious thing rather than 400ing on a missing body.
func TestFlush_EmptyBodyMeansEverything(t *testing.T) {
	f := &fakeControl{flushN: 3}
	srv := serve(t, f)

	code, out := post(t, srv, "/testctl/queue/flush", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}
	if f.flushed != nil {
		t.Errorf("uuids = %v, want nil so the control flushes the whole ready queue", f.flushed)
	}
	if !strings.Contains(out, `"flushed":3`) {
		t.Errorf("response %s does not report how many were flushed", out)
	}
}

// TestFlush_PartialFailureStillReportsTheCount: some uuids may have flushed
// before one failed, and losing that count would make a test's next assertion
// inexplicable.
func TestFlush_PartialFailureStillReportsTheCount(t *testing.T) {
	f := &fakeControl{flushN: 2, flushErr: errors.New("no such envelope: zzz")}
	srv := serve(t, f)

	code, out := post(t, srv, "/testctl/queue/flush", `{"uuids":["a","b","zzz"]}`)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", code, out)
	}
	if !strings.Contains(out, `"flushed":2`) || !strings.Contains(out, "zzz") {
		t.Errorf("response %s should carry both the count and the reason", out)
	}
}

// TestEnroll_UpstreamFailureIs502 so a test can tell "I sent nonsense" from "the
// console refused me" without reading the message.
// TestEnvelopeVerbs_ReachTheRightControlMethod pins the routing table: three
// verbs that share a request shape are three easy things to wire to the wrong
// method, and the mistake would be invisible in every other test here.
func TestEnvelopeVerbs_ReachTheRightControlMethod(t *testing.T) {
	for _, tc := range []struct{ path, verb, countKey string }{
		{"/testctl/queue/release", "release", "released"},
		{"/testctl/queue/hold", "hold", "held"},
		{"/testctl/queue/remove", "remove", "removed"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			f := &fakeControl{verbN: 2}
			srv := serve(t, f)

			code, out := post(t, srv, tc.path, `{"uuids":["a","b"]}`)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body %s", code, out)
			}
			if f.verb != tc.verb {
				t.Errorf("reached Control.%s, want %s", f.verb, tc.verb)
			}
			if len(f.verbArg) != 2 || f.verbArg[0] != "a" {
				t.Errorf("uuids = %v, want the request's list", f.verbArg)
			}
			if !strings.Contains(out, `"`+tc.countKey+`":2`) {
				t.Errorf("response %s does not report the count under %q", out, tc.countKey)
			}
		})
	}
}

// TestEnvelopeVerbs_RefuseAnEmptyList is where these deliberately differ from
// flush.
//
// "Flush the queue" is a gesture an operator makes after an outage. "Release
// everything ever quarantined" is not a gesture anybody wants to make by
// leaving a field out, so an absent or empty list is an error rather than a
// wildcard — and the control must not be called at all.
func TestEnvelopeVerbs_RefuseAnEmptyList(t *testing.T) {
	for _, path := range []string{
		"/testctl/queue/release",
		"/testctl/queue/hold",
		"/testctl/queue/remove",
	} {
		for _, body := range []string{`{}`, `{"uuids":[]}`} {
			f := &fakeControl{}
			srv := serve(t, f)

			code, out := post(t, srv, path, body)
			if code != http.StatusBadRequest {
				t.Errorf("POST %s %s = %d, want 400; body %s", path, body, code, out)
			}
			if f.verb != "" {
				t.Errorf("POST %s %s reached Control.%s; it must not be called at all",
					path, body, f.verb)
			}
		}
	}
}

// TestEnvelopeVerbs_PartialFailureStillReportsTheCount mirrors flush: an
// envelope claimed by a worker mid-call is an ordinary outcome, and a caller
// told only "error" would not know how much of its request took effect.
func TestEnvelopeVerbs_PartialFailureStillReportsTheCount(t *testing.T) {
	f := &fakeControl{verbN: 1, verbErr: errors.New(`"zzz": envelope is being delivered`)}
	srv := serve(t, f)

	code, out := post(t, srv, "/testctl/queue/release", `{"uuids":["a","zzz"]}`)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", code, out)
	}
	if !strings.Contains(out, `"released":1`) || !strings.Contains(out, "zzz") {
		t.Errorf("response %s should carry both the count and the reason", out)
	}
}

func TestEnroll_UpstreamFailureIs502(t *testing.T) {
	f := &fakeControl{enrollErr: errors.New("connection refused")}
	srv := serve(t, f)

	code, _ := post(t, srv, "/testctl/enroll", `{"central_url":"https://console:4000"}`)
	if code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an upstream failure", code)
	}

	f.enrollErr = nil
	code, out := post(t, srv, "/testctl/enroll", `{"central_url":"https://console:4000","insecure_tls":true}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}
	if f.enrolled.CentralURL != "https://console:4000" || !f.enrolled.InsecureTLS {
		t.Errorf("settings reached the control as %+v", f.enrolled)
	}
	// The response has to carry the fingerprint, or a test cannot approve this
	// gateway on the console as its next step.
	if !strings.Contains(out, "fingerprint") {
		t.Errorf("enroll response %s does not include the fingerprint", out)
	}
}

// TestEnroll_RequiresACentralURL: without it the request is meaningless, and
// letting it through would store an empty URL and fail much later.
func TestEnroll_RequiresACentralURL(t *testing.T) {
	srv := serve(t, &fakeControl{})
	if code, _ := post(t, srv, "/testctl/enroll", `{}`); code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

// TestStatus_ReportsBoundListeners is the field a test suite depends on to find
// an ephemeral port.
func TestStatus_ReportsBoundListeners(t *testing.T) {
	srv := serve(t, &fakeControl{})

	code, out := get(t, srv, "/testctl/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}
	var st node.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("not a Status: %v (%s)", err, out)
	}
	if len(st.Listeners) != 1 || st.Listeners[0] != "127.0.0.1:31337" {
		t.Errorf("listeners = %v", st.Listeners)
	}
	// Same reasoning as the listeners beside it: an admin listener asked for on
	// port 0 is unreachable unless the bound address comes back out.
	if st.AdminAddr != "127.0.0.1:31338" {
		t.Errorf("admin_addr = %q, want the bound admin address", st.AdminAddr)
	}
	if st.Build != "test-only" {
		t.Errorf("build = %q; a caller must be able to tell this is not a shipped node", st.Build)
	}
}

// TestQueue_BeforeAnyConfigIs409: there is no spool until a configuration
// applies, and 409 says "wrong state" where 500 would say "I am broken".
func TestQueue_BeforeAnyConfigIs409(t *testing.T) {
	srv := serve(t, &fakeControl{queueErr: errors.New("no spool: no configuration has been applied yet")})
	if code, _ := get(t, srv, "/testctl/queue"); code != http.StatusConflict {
		t.Errorf("status = %d, want 409", code)
	}
}

// TestQueue_EmptyIsAnArrayNotNull, because `entries: null` makes every caller
// in every language special-case it.
func TestQueue_EmptyIsAnArrayNotNull(t *testing.T) {
	srv := serve(t, &fakeControl{})
	code, out := get(t, srv, "/testctl/queue")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}
	if !strings.Contains(out, `"entries":[]`) {
		t.Errorf("empty queue rendered as %s, want an empty array", out)
	}
}

// TestShowConfig_BeforeAnyConfigIs404 rather than an empty 200, which a test
// would happily assert against.
func TestShowConfig_BeforeAnyConfigIs404(t *testing.T) {
	srv := serve(t, &fakeControl{cachedErr: errors.New("no configuration has been applied yet")})
	if code, _ := get(t, srv, "/testctl/config"); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

// TestShowConfig_IsUnredacted: a test asserting on a credential it just injected
// cannot do that against "[redacted]". `mailgw-go config show` keeps redacting.
func TestShowConfig_IsUnredacted(t *testing.T) {
	raw := []byte(`{"format":1,"relays":{"Outbound":[{"auth_pass":"hunter2"}]}}`)
	srv := serve(t, &fakeControl{cached: &store.CachedConfig{
		VersionID: -7, Version: 1, SHA256: "abc", Bundle: raw,
	}})

	code, out := get(t, srv, "/testctl/config")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}
	if !strings.Contains(out, "hunter2") {
		t.Errorf("response %s redacted the password; this build exists to be inspected", out)
	}
}

// TestReset_SaysARestartIsRequired: reset cannot stop the gateway serving, and a
// caller that believed otherwise would get a false pass.
func TestReset_SaysARestartIsRequired(t *testing.T) {
	f := &fakeControl{}
	srv := serve(t, f)

	code, out := post(t, srv, "/testctl/reset", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, out)
	}
	if !f.resetCalled {
		t.Error("the control was never asked to reset")
	}
	if !strings.Contains(out, `"restart_required":true`) {
		t.Errorf("response %s does not say a restart is required", out)
	}
}

// TestUnknownRouteIs404 — the mux is the whole policy here, so a typo in a test
// must not silently hit something else.
func TestUnknownRouteIs404(t *testing.T) {
	srv := serve(t, &fakeControl{})
	if code, _ := get(t, srv, "/testctl/nope"); code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
	// Wrong method on a real route, too.
	if code, _ := get(t, srv, "/testctl/reset"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a POST route = %d, want 405", code)
	}
}
