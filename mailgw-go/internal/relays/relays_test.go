package relays

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRelays(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "relays.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
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

func TestLoad_ShippedShape(t *testing.T) {
	tbl, err := Load(writeRelays(t, shippedShape))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

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
	tbl, err := Load(writeRelays(t, shippedShape))
	if err != nil {
		t.Fatalf("Load: %v", err)
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
		if _, err := Load(writeRelays(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoad_SortsByPriority(t *testing.T) {
	tbl, err := Load(writeRelays(t, `{"A":[
		{"name":"third","exchange":"c","port":25,"priority":20},
		{"name":"first","exchange":"a","port":25,"priority":0},
		{"name":"second","exchange":"b","port":25,"priority":10}
	]}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
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
	tbl, err := Load(writeRelays(t, `{"A":[
		{"name":"hi-1","exchange":"a","port":25,"priority":0},
		{"name":"hi-2","exchange":"b","port":25,"priority":0},
		{"name":"lo-1","exchange":"c","port":25,"priority":10}
	]}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
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
	tbl, _ := Load(writeRelays(t, shippedShape))
	g, _ := tbl.Lookup("Exchange")
	before := []string{g.Members[0].Name, g.Members[1].Name}
	for i := 0; i < 20; i++ {
		_ = g.Attempts(nil)
	}
	if g.Members[0].Name != before[0] || g.Members[1].Name != before[1] {
		t.Error("Attempts must not reorder the group's own slice")
	}
}

func TestPassword_PrefersEnv(t *testing.T) {
	t.Setenv("MAILGW_TEST_RELAY_PASS", "from-env")
	r := Relay{AuthPass: "from-file", AuthPassEnv: "MAILGW_TEST_RELAY_PASS"}
	if got := r.Password(); got != "from-env" {
		t.Errorf("got %q, want from-env", got)
	}
	if got := (Relay{AuthPass: "from-file"}).Password(); got != "from-file" {
		t.Errorf("got %q, want from-file", got)
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
	tbl, _ := Load(writeRelays(t, shippedShape))
	got := tbl.PlaintextCredentials()
	if len(got) != 3 {
		t.Errorf("got %v, want 3 entries", got)
	}
}
