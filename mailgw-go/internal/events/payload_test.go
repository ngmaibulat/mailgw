package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The delivery payload is validated by zod at logservice/src/validation/delivery.ts
// and answers 400 on any deviation. Pin the exact bytes.
func TestDelivery_GoldenJSON(t *testing.T) {
	d := Delivery{
		UUID:         "ABCDEF01-2345-6789-ABCD-EF0123456789.1.1",
		DT:           1753776000000,
		Sender:       "me@ngm.dev",
		RcptDomain:   "ngm.dev",
		RcptList:     "test@ngm.dev",
		RcptAccepted: "test@ngm.dev",
		TLSForced:    false,
		TLS:          true,
		Auth:         true,
		Host:         "sandbox.smtp.mailtrap.io",
		IP:           "203.0.113.10",
		Port:         "2525",
		Response:     "250 2.0.0 Ok: queued as ABC123",
		Delay:        0.123,
	}

	got, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"uuid":"ABCDEF01-2345-6789-ABCD-EF0123456789.1.1","dt":1753776000000,` +
		`"sender":"me@ngm.dev","rcpt_domain":"ngm.dev","rcpt_list":"test@ngm.dev",` +
		`"rcpt_accepted":"test@ngm.dev","tls_forced":false,"tls":true,"auth":true,` +
		`"host":"sandbox.smtp.mailtrap.io","ip":"203.0.113.10","port":"2525",` +
		`"response":"250 2.0.0 Ok: queued as ABC123","delay":0.123}`
	if string(got) != want {
		t.Errorf("delivery payload drifted from the logservice contract\n got: %s\nwant: %s", got, want)
	}
}

// portSchema is z.string().regex(/^[0-9]+$/) — a numeric port is a 400.
func TestDelivery_PortIsAJSONString(t *testing.T) {
	b, err := json.Marshal(Delivery{Port: "25"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := back["port"].(string); !ok {
		t.Errorf("port must serialise as a string, got %T (%v)", back["port"], back["port"])
	}
}

// rcpt_list and rcpt_accepted are validated as a SINGLE email. Haraka sends a
// comma-joined list, which 400s and is lost; one event per recipient is the fix.
func TestDelivery_RecipientFieldsHoldOneAddress(t *testing.T) {
	for _, d := range perRecipientDeliveries("me@ngm.dev", []string{"a@x.com", "b@y.com"}) {
		if got := d.RcptList; got == "" || containsComma(got) {
			t.Errorf("rcpt_list must be exactly one address, got %q", got)
		}
		if got := d.RcptAccepted; got == "" || containsComma(got) {
			t.Errorf("rcpt_accepted must be exactly one address, got %q", got)
		}
	}
}

// perRecipientDeliveries is the shape the queue worker uses: one event each.
func perRecipientDeliveries(sender string, rcpts []string) []Delivery {
	out := make([]Delivery, 0, len(rcpts))
	for _, r := range rcpts {
		out = append(out, Delivery{
			Sender:       sender,
			RcptList:     r,
			RcptAccepted: r,
			RcptDomain:   DomainOf(r),
		})
	}
	return out
}

func containsComma(s string) bool { return strings.Contains(s, ",") }

// The connection payload's mixed casing is required by logservice's
// toConnectionRow; a rename silently produces a row of nulls.
func TestConnection_FieldNamesMatchLogservice(t *testing.T) {
	b, err := json.Marshal(Connection{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{
		"uuid", "dt", "encoding", "hello_name",
		"remoteAddr", "remotePort", // camelCase, deliberately
		"remote_host", "remote_info",
		"remote_is_local", "remote_is_private", "using_tls",
		"tran_count", "rcpt_count_accept", "rcpt_count_tempfail", "rcpt_count_reject",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
	if len(m) != len(want) {
		t.Errorf("field count: got %d, want %d (%v)", len(m), len(want), m)
	}

	// logservice has no column for these; Haraka sends them anyway.
	for _, k := range []string{"state", "pipelining"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q is dropped by logservice and should not be sent", k)
		}
	}
}

func TestQueue_FieldNamesMatchLogservice(t *testing.T) {
	b, _ := json.Marshal(Queue{})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{
		"uuid", "dt", "action", "encoding", "sender", "rcpt_list",
		"rcpt_count_accept", "rcpt_count_tempfail", "rcpt_count_reject",
		"delay_data_post", "data_bytes", "mime_part_count",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
	if len(m) != len(want) {
		t.Errorf("field count: got %d, want %d (%v)", len(m), len(want), m)
	}
}

// dt is epoch milliseconds everywhere; logservice divides by 1000 in SQL.
func TestMillis(t *testing.T) {
	ts := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	got := Millis(ts)
	if want := ts.Unix() * 1000; got != want {
		t.Errorf("got %d, want %d", got, want)
	}
	if got < 1_000_000_000_000 {
		t.Errorf("%d looks like seconds, not milliseconds", got)
	}
}

// getAddr (functions.js:128) returns "" for a null sender, not "<>" or "null".
func TestAddr_NullSenderIsEmpty(t *testing.T) {
	if got := Addr("", ""); got != "" {
		t.Errorf("null sender: got %q, want \"\"", got)
	}
	if got := Addr("me", "ngm.dev"); got != "me@ngm.dev" {
		t.Errorf("got %q", got)
	}
	// Parity with Haraka's partial-address behaviour.
	if got := Addr("me", ""); got != "me@" {
		t.Errorf("got %q, want \"me@\"", got)
	}
}

// getAddrList (functions.js:134): comma-joined, no trailing comma, "" if empty.
func TestJoinAddrs(t *testing.T) {
	cases := map[string]struct {
		in   []string
		want string
	}{
		"empty":   {nil, ""},
		"one":     {[]string{"a@x.com"}, "a@x.com"},
		"two":     {[]string{"a@x.com", "b@y.com"}, "a@x.com,b@y.com"},
		"nospace": {[]string{"a@x.com", "b@y.com", "c@z.com"}, "a@x.com,b@y.com,c@z.com"},
	}
	for name, c := range cases {
		if got := JoinAddrs(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestDomainOf(t *testing.T) {
	cases := map[string]string{
		"a@x.com":    "x.com",
		"a@b@x.com":  "x.com",
		"postmaster": "",
		"":           "",
		"@x.com":     "x.com",
	}
	for in, want := range cases {
		if got := DomainOf(in); got != want {
			t.Errorf("DomainOf(%q): got %q, want %q", in, got, want)
		}
	}
}
