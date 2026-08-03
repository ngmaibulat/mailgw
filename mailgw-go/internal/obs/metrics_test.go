package obs

import (
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// wantKeys is the Snapshot contract, in sorted order.
//
// These strings travel in the heartbeat and webui-fastify stores them, so
// renaming one breaks the console for every gateway that has already reported.
// If this test fails because a key was renamed, add a new key instead.
var wantKeys = []string{
	"auth_failed",
	"auth_ok",
	"bytes_in",
	"conn_accepted",
	"conn_denied",
	"conn_throttled",
	"deliver_attempts",
	"deliver_bounced",
	"deliver_connfail",
	"deliver_connreused",
	"deliver_deferred",
	"deliver_ok",
	"deliver_pool_full",
	"deliver_tls_downgraded",
	"dkim_sign_failed",
	"dkim_signed",
	"dkim_verified",
	"dsn_generated",
	"dsn_notify_suppressed",
	"dsn_suppressed",
	"dsn_unroutable",
	"env_expired",
	"env_quarantined",
	"env_queued",
	"env_released",
	"events_dropped",
	"events_replay_failed",
	"events_replayed",
	"events_spilled",
	"msg_accepted",
	"msg_discarded",
	"msg_loop_rejected",
	"msg_quarantined",
	"msg_rejected",
	"msg_tempfailed",
	"proxy_dropped",
	"rate_auth",
	"rate_conn",
	"rate_rcpt_domain",
	"rate_sender",
	"rate_user",
	"rcpt_accepted",
	"rcpt_discarded",
	"rcpt_rejected",
	"rcpt_tempfailed",
	"spf_checked",
	"spf_failed",
}

func TestSnapshot_KeysAreStable(t *testing.T) {
	got := make([]string, 0, len(wantKeys))
	for k := range New().Snapshot() {
		got = append(got, k)
	}
	slices.Sort(got)

	if !slices.Equal(got, wantKeys) {
		t.Errorf("Snapshot keys =\n  %v\nwant\n  %v\n\n"+
			"Renaming a key breaks the console, which stores it verbatim. Add a new key instead.",
			got, wantKeys)
	}
}

func TestSnapshot_ReportsValues(t *testing.T) {
	m := New()
	m.MsgAccepted.Add(3)
	m.DeliverConnFail.Add(7)

	s := m.Snapshot()
	if s["msg_accepted"] != 3 {
		t.Errorf("msg_accepted = %d, want 3", s["msg_accepted"])
	}
	if s["deliver_connfail"] != 7 {
		t.Errorf("deliver_connfail = %d, want 7", s["deliver_connfail"])
	}
	// Every key is present even at zero, so the console never has to
	// distinguish "not reported" from "nothing happened".
	if len(s) != len(wantKeys) {
		t.Errorf("snapshot has %d keys, want %d", len(s), len(wantKeys))
	}
}

// The counters table is written by hand, so this is the guard that a new field
// cannot be added to Metrics and silently never exported.
func TestCounters_TableCoversEveryField(t *testing.T) {
	seen := map[string]int{}
	m := New()
	for _, c := range counters {
		// Identity, not value: two entries could both read zero.
		seen[fieldNameOf(t, m, c.get(m))]++
	}

	typ := reflect.TypeOf(Metrics{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type != reflect.TypeOf(atomic.Int64{}) {
			continue
		}
		switch seen[f.Name] {
		case 1:
		case 0:
			t.Errorf("Metrics.%s is in no counters entry, so it is never exported", f.Name)
		default:
			t.Errorf("Metrics.%s appears in %d counters entries", f.Name, seen[f.Name])
		}
	}
}

// fieldNameOf resolves which Metrics field a getter returned, by pointer.
func fieldNameOf(t *testing.T, m *Metrics, p *atomic.Int64) string {
	t.Helper()
	v := reflect.ValueOf(m).Elem()
	for i := range v.NumField() {
		if v.Field(i).Addr().Interface() == any(p) {
			return v.Type().Field(i).Name
		}
	}
	t.Fatalf("a counters entry returned a pointer that is not a Metrics field")
	return ""
}

func TestCounters_KeysFitTheConsoleSchema(t *testing.T) {
	// webui-fastify/src/validation/agent.ts caps a metrics key at 64 chars and
	// rejects the whole report if one is longer — which loses the heartbeat as
	// well as the metrics.
	for _, c := range counters {
		if len(c.key) > 64 {
			t.Errorf("Snapshot key %q is %d chars; the console rejects over 64", c.key, len(c.key))
		}
	}
}

func TestWritePrometheus_Exposition(t *testing.T) {
	m := New()
	m.ConnAccepted.Add(5)
	m.MsgAccepted.Add(2)

	var sb strings.Builder
	err := m.WritePrometheus(&sb, Gauges{
		QueueOK: true, QueueReady: 4, QueueInflight: 1,
		QueueQuarantine: 2, QueueDead: 3, RejectedEvents: 6,
		ConfigVersion: 7, Approved: true, Serving: true, Managed: true,
		Version: "1.2.3", Commit: "abc123",
	})
	if err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := sb.String()

	for _, want := range []string{
		"# HELP mailgw_connections_accepted_total ",
		"# TYPE mailgw_connections_accepted_total counter\n",
		"mailgw_connections_accepted_total 5\n",
		"mailgw_messages_accepted_total 2\n",
		"mailgw_build_info{version=\"1.2.3\",commit=\"abc123\"} 1\n",
		"mailgw_config_version 7\n",
		"mailgw_managed 1\n",
		"mailgw_approved 1\n",
		"mailgw_serving 1\n",
		"mailgw_queue_ready 4\n",
		"mailgw_queue_inflight 1\n",
		"mailgw_queue_quarantine 2\n",
		"mailgw_queue_dead 3\n",
		"mailgw_failed_events_rejected 6\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}

	if !strings.HasSuffix(out, "\n") {
		t.Error("exposition must end with a newline")
	}

	// Every counter carries _total, and no series is emitted twice.
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(line, " ")
		if seen[name] {
			t.Errorf("series %q emitted more than once", name)
		}
		seen[name] = true
	}
	for _, c := range counters {
		if !strings.HasSuffix(c.name, "_total") {
			t.Errorf("counter %q should end in _total", c.name)
		}
	}
}

func TestWritePrometheus_EscapesLabelValues(t *testing.T) {
	// version and commit come straight from -ldflags, so a build pipeline can
	// put anything in them. An unescaped quote produces an exposition no
	// scraper can parse.
	var sb strings.Builder
	if err := New().WritePrometheus(&sb, Gauges{
		Version: `1.0"x\y`,
		Commit:  "ab\ncd",
	}); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}

	want := `mailgw_build_info{version="1.0\"x\\y",commit="ab\ncd"} 1`
	if !strings.Contains(sb.String(), want) {
		t.Errorf("build_info line is not escaped; want %s\n---\n%s", want, sb.String())
	}
}

func TestWritePrometheus_OmitsQueueWhenUnknown(t *testing.T) {
	// A managed gateway has no spool until its first configuration applies.
	// Reporting 0 there would read as "drained" on a dashboard.
	var sb strings.Builder
	if err := New().WritePrometheus(&sb, Gauges{QueueOK: false, QueueReady: 9}); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	if strings.Contains(sb.String(), "mailgw_queue_") {
		t.Errorf("queue series must be omitted when QueueOK is false\n---\n%s", sb.String())
	}
}

func TestNilMetricsAreUsable(t *testing.T) {
	// Discard and the obs() accessors mean this should never happen in
	// practice, but a /metrics scrape must not be able to panic the process.
	var m *Metrics
	if got := len(m.Snapshot()); got != 0 {
		t.Errorf("nil Snapshot has %d keys, want 0", got)
	}
	var sb strings.Builder
	if err := m.WritePrometheus(&sb, Gauges{}); err != nil {
		t.Fatalf("nil WritePrometheus: %v", err)
	}
	if sb.Len() == 0 {
		t.Error("nil WritePrometheus wrote nothing")
	}
}
