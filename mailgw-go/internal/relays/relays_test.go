package relays

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseTable decodes the relay-group JSON a bundle carries and builds a table
// from it, which is the only way a table is built now — there is no relays.json
// on disk to read.
func parseTable(t *testing.T, content string) (*Table, error) {
	t.Helper()
	var byGroup map[string][]Relay
	if err := json.Unmarshal([]byte(content), &byGroup); err != nil {
		return nil, err
	}
	return NewTable(byGroup)
}

func mustTable(t *testing.T, content string) *Table {
	t.Helper()
	tbl, err := parseTable(t, content)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tbl
}

// The shipped mailgw/config/relays.json writes the port as a string.
func TestPort_AcceptsStringAndNumber(t *testing.T) {
	var v struct {
		A Port `json:"a"`
		B Port `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a":"2525","b":25}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.A != 2525 {
		t.Errorf(`string form: got %d, want 2525`, int(v.A))
	}
	if v.B != 25 {
		t.Errorf("number form: got %d, want 25", int(v.B))
	}
}

func TestPort_MarshalsAsNumberButStringifiesForEvents(t *testing.T) {
	b, err := json.Marshal(Port(2525))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "2525" {
		t.Errorf("got %s, want 2525", b)
	}
	// The logservice delivery schema requires the port as a JSON string.
	if got := Port(2525).String(); got != "2525" {
		t.Errorf("String(): got %q, want \"2525\"", got)
	}
}

func TestPort_RejectsGarbage(t *testing.T) {
	var p Port
	if err := json.Unmarshal([]byte(`"not-a-port"`), &p); err == nil {
		t.Error("expected an error for a non-numeric port")
	}
}

// Mirrors the shape of the real mailgw/config/relays.json.
const shippedShape = `{
  "Exchange": [
    {"name":"Exch-01","auth_user":"u","auth_pass":"p","priority":0,"exchange":"sandbox.smtp.mailtrap.io","port":"2525"},
    {"name":"Exch-02","auth_user":"u","auth_pass":"p","priority":0,"exchange":"sandbox.smtp.mailtrap.io","port":"2525"}
  ],
  "Outbound": [
    {"name":"Default","auth_user":"u","auth_pass":"p","priority":0,"exchange":"sandbox.smtp.mailtrap.io","port":"2525"}
  ]
}`

func TestNewTable_ShippedShape(t *testing.T) {
	tbl := mustTable(t, shippedShape)

	g, ok := tbl.Lookup("Exchange")
	if !ok {
		t.Fatal("Exchange group should exist")
	}
	if len(g.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(g.Members))
	}
	if got := g.Members[0].Addr(); got != "sandbox.smtp.mailtrap.io:2525" {
		t.Errorf("Addr(): got %q", got)
	}
	if got := tbl.Names(); len(got) != 2 || got[0] != "Exchange" || got[1] != "Outbound" {
		t.Errorf("Names(): got %v", got)
	}
}

// The JavaScript `relayname in this.relays` check at RoutingTable.js:29 walks
// the prototype chain, so a relay named "toString" resolves truthy and hands a
// Function to Haraka as an MX. A Go map lookup cannot, and this pins it.
func TestLookup_DoesNotResolveInheritedNames(t *testing.T) {
	tbl, err := parseTable(t, shippedShape)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	for _, name := range []string{"toString", "constructor", "valueOf", "__proto__", "hasOwnProperty"} {
		if _, ok := tbl.Lookup(name); ok {
			t.Errorf("%q must not resolve to a relay group", name)
		}
		if tbl.Has(name) {
			t.Errorf("Has(%q) must be false", name)
		}
	}
}

func TestLoad_RejectsInvalidConfigs(t *testing.T) {
	cases := map[string]string{
		"empty file":     `{}`,
		"empty group":    `{"A":[]}`,
		"missing host":   `{"A":[{"name":"x","port":25}]}`,
		"port too big":   `{"A":[{"name":"x","exchange":"h","port":70000}]}`,
		"port zero":      `{"A":[{"name":"x","exchange":"h","port":0}]}`,
		"bad tls policy": `{"A":[{"name":"x","exchange":"h","port":25,"tls":"maybe"}]}`,
	}
	for name, body := range cases {
		if _, err := parseTable(t, body); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNewTable_SortsByPriority(t *testing.T) {
	tbl, err := parseTable(t, `{"A":[
		{"name":"third","exchange":"c","port":25,"priority":20},
		{"name":"first","exchange":"a","port":25,"priority":0},
		{"name":"second","exchange":"b","port":25,"priority":10}
	]}`)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	g, _ := tbl.Lookup("A")
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if g.Members[i].Name != w {
			t.Errorf("position %d: got %q, want %q", i, g.Members[i].Name, w)
		}
	}
}

// Attempts must never reorder across priority bands, however it shuffles within
// them.
func TestAttempts_PreservesPriorityBands(t *testing.T) {
	tbl, err := parseTable(t, `{"A":[
		{"name":"hi-1","exchange":"a","port":25,"priority":0},
		{"name":"hi-2","exchange":"b","port":25,"priority":0},
		{"name":"lo-1","exchange":"c","port":25,"priority":10}
	]}`)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	g, _ := tbl.Lookup("A")

	for i := 0; i < 50; i++ {
		got := g.Attempts(nil)
		if len(got) != 3 {
			t.Fatalf("got %d attempts, want 3", len(got))
		}
		if got[2].Name != "lo-1" {
			t.Fatalf("lower-priority relay must come last, got %q", got[2].Name)
		}
		if got[0].Priority != 0 || got[1].Priority != 0 {
			t.Fatalf("priority-0 band must occupy the first two slots, got %v", got)
		}
	}
}

func TestAttempts_DoesNotMutateGroup(t *testing.T) {
	tbl, _ := parseTable(t, shippedShape)
	g, _ := tbl.Lookup("Exchange")
	before := []string{g.Members[0].Name, g.Members[1].Name}
	for i := 0; i < 20; i++ {
		_ = g.Attempts(nil)
	}
	if g.Members[0].Name != before[0] || g.Members[1].Name != before[1] {
		t.Error("Attempts must not reorder the group's own slice")
	}
}

func TestPassword_ComesFromTheBundle(t *testing.T) {
	if got := (Relay{AuthPass: "hunter2"}).Password(); got != "hunter2" {
		t.Errorf("got %q, want hunter2", got)
	}
}

// auth_pass_env must be REFUSED, not ignored.
//
// This gateway reads no environment, so the field can only ever resolve to the
// empty string — and an empty password is not an error a relay reports
// usefully: it answers "535 authentication failed", which sends the operator to
// check a credential that was never sent. Ignoring the field would silently
// fall back to auth_pass, which for a relay configured this way is empty too.
//
// Revert the check in NewTable and this test fails with the table building
// happily and Password() returning "".
func TestNewTable_RefusesAuthPassEnv(t *testing.T) {
	_, err := parseTable(t, `{"A":[
		{"name":"one","exchange":"a","port":25,"auth_user":"u","auth_pass_env":"RELAY_PASS"}
	]}`)
	if err == nil {
		t.Fatal("auth_pass_env must be refused: it would authenticate with an empty password")
	}
	if !strings.Contains(err.Error(), "auth_pass_env") {
		t.Errorf("the error must name the offending field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "empty password") {
		t.Errorf("the error must say what would go wrong, got: %v", err)
	}
}

func TestString_RedactsPassword(t *testing.T) {
	r := Relay{Name: "x", Exchange: "h", Port: 25, AuthUser: "u", AuthPass: "hunter2"}
	s := r.String()
	if want := "[redacted]"; !strings.Contains(s, want) {
		t.Errorf("got %q, want it to contain %q", s, want)
	}
	if strings.Contains(s, "hunter2") {
		t.Errorf("password leaked into %q", s)
	}
}

func TestPlaintextCredentials(t *testing.T) {
	tbl, _ := parseTable(t, shippedShape)
	got := tbl.PlaintextCredentials()
	if len(got) != 3 {
		t.Errorf("got %v, want 3 entries", got)
	}
}
