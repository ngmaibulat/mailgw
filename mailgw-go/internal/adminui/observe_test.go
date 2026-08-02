package adminui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

// readyState is a gateway that satisfies every readiness condition.
func readyState() State {
	v := 7
	return State{
		GatewayUID:     "11111111-2222-3333-4444-555555555555",
		CentralURL:     "https://console.example",
		Approval:       "approved",
		AppliedVersion: &v,
		Serving:        true,
	}
}

// Like /healthz, /metrics must answer on a server with nothing wired up: it is
// scraped from the moment the process starts, which on a managed node is long
// before any configuration applies.
func TestAdminUI_MetricsNeedsNoStore(t *testing.T) {
	rec := get(t, &Server{}, "/metrics")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mailgw_build_info") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestAdminUI_MetricsExposesBuildInfoAndCounters(t *testing.T) {
	m := obs.New()
	m.MsgAccepted.Add(4)

	s := &Server{
		Version: "1.0.0",
		Commit:  "deadbee",
		Metrics: m,
		Gauges: func() obs.Gauges {
			return obs.Gauges{Managed: true, Approved: true, Serving: true, ConfigVersion: 7}
		},
	}
	rec := get(t, s, "/metrics")

	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`mailgw_build_info{version="1.0.0",commit="deadbee"} 1`,
		"mailgw_messages_accepted_total 4",
		"mailgw_config_version 7",
		"mailgw_serving 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics is missing %q\n---\n%s", want, body)
		}
	}
	// The spool does not exist here, so the queue depths are unknown rather
	// than zero.
	if strings.Contains(body, "mailgw_queue_") {
		t.Error("queue series must be omitted when the spool has not been read")
	}
}

func TestAdminUI_ReadyzNotReadyListsReasons(t *testing.T) {
	s := &Server{Store: testStore(t), State: func() State { return State{} }}
	rec := get(t, s, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		reasonUnprovisioned, reasonNotApproved, reasonNoConfig, reasonNotListening,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing the reason %q\n---\n%s", want, body)
		}
	}
}

func TestAdminUI_ReadyzReadyWhenApprovedAppliedAndServing(t *testing.T) {
	s := &Server{Store: testStore(t), State: readyState}
	rec := get(t, s, "/readyz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// A console outage is not a gateway outage.
//
// If readiness ever grew a freshness check, one console going down would 503
// the whole fleet at once and every load balancer would drain them together —
// a management-plane failure turned into a mail failure. This test exists to
// stop that being added helpfully.
func TestAdminUI_ReadyzIgnoresConsoleErrors(t *testing.T) {
	st := readyState()
	st.LastError = "dial tcp 10.0.0.1:443: connection refused"
	st.RestartRequired = []string{"relays"}
	st.ApplyError = "rule 3: unknown field msg.nonsense"

	s := &Server{Store: testStore(t), State: func() State { return st }}
	rec := get(t, s, "/readyz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a gateway serving mail on a good "+
			"configuration is ready, whatever the console is doing", rec.Code)
	}
	// LastError is operator-supplied text and must never be echoed here, where
	// the output is scraped and stored.
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("readyz must not echo LastError: %q", rec.Body.String())
	}
}

// In file mode the gateway owns its configuration, so provisioning and approval
// are not merely satisfied — they do not apply.
func TestAdminUI_ReadyzFileMode(t *testing.T) {
	serving := &Server{State: func() State { return State{Serving: true} }}
	if rec := get(t, serving, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("serving file-mode gateway = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	idle := &Server{State: func() State { return State{} }}
	rec := get(t, idle, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-serving file-mode gateway = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, reasonNotListening) {
		t.Errorf("body is missing %q: %s", reasonNotListening, body)
	}
	for _, unwanted := range []string{reasonUnprovisioned, reasonNotApproved, reasonNoConfig} {
		if strings.Contains(body, unwanted) {
			t.Errorf("file mode must not report %q: %s", unwanted, body)
		}
	}
}

// Liveness and readiness answer different questions. Chaining one onto the
// other gets a gateway killed for waiting on an operator.
func TestAdminUI_HealthzIgnoresReadiness(t *testing.T) {
	s := &Server{Store: testStore(t), State: func() State { return State{} }}

	if rec := get(t, s, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", rec.Code)
	}
	if rec := get(t, s, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 even when not ready", rec.Code)
	}
}
