// Package relays loads relay-group definitions and orders their members for
// delivery attempts.
package relays

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// TLS policies for an outbound connection to a relay.
const (
	TLSNone          = "none"
	TLSOpportunistic = "opportunistic"
	TLSRequired      = "required"
)

// Port accepts either a JSON string ("2525") or a number (2525). The shipped
// mailgw/config/relays.json uses the string form.
type Port int

func (p *Port) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	// Strip surrounding quotes if this is the string form.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("port %s: not a number", string(b))
	}
	*p = Port(n)
	return nil
}

func (p Port) MarshalJSON() ([]byte, error) { return json.Marshal(int(p)) }

// String renders the port for use in a host:port dial string and in the
// delivery event, which requires a JSON string (see internal/events).
func (p Port) String() string { return strconv.Itoa(int(p)) }

// Relay is one delivery target. The JSON field names match the existing
// mailgw/config/relays.json so the file is reusable byte-for-byte.
type Relay struct {
	Name     string `json:"name"`
	Exchange string `json:"exchange"`
	Port     Port   `json:"port"`
	Priority int    `json:"priority"`

	AuthUser string `json:"auth_user,omitempty"`
	AuthPass string `json:"auth_pass,omitempty"`
	// AuthPassEnv names an environment variable holding the password, so
	// credentials need not sit in the config file. It wins over AuthPass.
	AuthPassEnv string `json:"auth_pass_env,omitempty"`

	// UseMX makes Exchange a DOMAIN whose MX records are resolved at delivery
	// time, rather than a host to dial directly. The resolved exchangers are
	// tried in preference order, each inheriting this relay's port, credentials
	// and TLS policy.
	//
	// This is "smarthost named by domain", not direct-to-MX delivery: the
	// recipient's own domain is not consulted. An envelope groups recipients by
	// relay GROUP, so delivering per recipient domain would need the SMTP
	// session to bucket by domain as well — a different delivery mode with its
	// own TLS and reputation story, deliberately out of scope.
	UseMX bool `json:"use_mx,omitempty"`

	// TLS is one of none|opportunistic|required; empty means opportunistic.
	TLS string `json:"tls,omitempty"`
	// AllowInsecureAuth permits AUTH over an unencrypted connection. Off by
	// default: sending a password in the clear should be a deliberate choice.
	AllowInsecureAuth bool `json:"allow_insecure_auth,omitempty"`
}

// Addr is the dial target.
func (r Relay) Addr() string { return r.Exchange + ":" + r.Port.String() }

// Password resolves the effective password, preferring the environment.
func (r Relay) Password() string {
	if r.AuthPassEnv != "" {
		return os.Getenv(r.AuthPassEnv)
	}
	return r.AuthPass
}

// TLSPolicy normalises the configured policy, defaulting to opportunistic.
func (r Relay) TLSPolicy() string {
	switch strings.ToLower(r.TLS) {
	case TLSNone:
		return TLSNone
	case TLSRequired:
		return TLSRequired
	default:
		return TLSOpportunistic
	}
}

// String redacts the password so a Relay can be logged directly.
func (r Relay) String() string {
	auth := "none"
	if r.AuthUser != "" {
		auth = r.AuthUser + ":[redacted]"
	}
	return fmt.Sprintf("%s(%s auth=%s tls=%s prio=%d)", r.Name, r.Addr(), auth, r.TLSPolicy(), r.Priority)
}

// Group is a named set of interchangeable relays, ordered for failover.
type Group struct {
	Name    string
	Members []Relay // sorted by (Priority asc, config order)
}

// Table maps group name to group. Lookups go through Lookup, never a bare map
// index on a caller-supplied string, so a rule naming "toString" cannot resolve
// to anything (the prototype-chain bug at mailgw/plugins/RoutingTable.js:29).
type Table struct {
	groups map[string]*Group
}

// Load reads relays.json: an object mapping group name to an array of relays.
func Load(path string) (*Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var byGroup map[string][]Relay
	if err := json.Unmarshal(raw, &byGroup); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	t, err := NewTable(byGroup)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// NewTable validates a set of relay groups and builds the lookup table.
//
// Separate from Load so that tests, and any future configuration source, get
// exactly the same validation the file path does.
func NewTable(byGroup map[string][]Relay) (*Table, error) {
	if len(byGroup) == 0 {
		return nil, fmt.Errorf("no relay groups defined")
	}

	t := &Table{groups: make(map[string]*Group, len(byGroup))}
	for name, members := range byGroup {
		if len(members) == 0 {
			return nil, fmt.Errorf("relay group %q is empty", name)
		}
		for i, m := range members {
			if strings.TrimSpace(m.Exchange) == "" {
				return nil, fmt.Errorf("relay group %q member %d: missing 'exchange'", name, i)
			}
			if m.Port < 1 || m.Port > 65535 {
				return nil, fmt.Errorf("relay group %q member %q: port %d out of range", name, m.Name, int(m.Port))
			}
			switch strings.ToLower(m.TLS) {
			case "", TLSNone, TLSOpportunistic, TLSRequired:
			default:
				return nil, fmt.Errorf("relay group %q member %q: unknown tls policy %q", name, m.Name, m.TLS)
			}
		}

		g := &Group{Name: name, Members: append([]Relay(nil), members...)}
		// SliceStable keeps config order within a priority band; Attempts
		// shuffles within the band at delivery time to spread load.
		sort.SliceStable(g.Members, func(i, j int) bool {
			return g.Members[i].Priority < g.Members[j].Priority
		})
		t.groups[name] = g
	}
	return t, nil
}

// Equal reports whether two tables define the same groups with the same members
// in the same order.
//
// Defined here rather than at the call site because groups is unexported: a
// caller reaching in with reflect would break the moment Table grows a lock or
// a cache. Used to decide whether a pulled configuration needs a restart — the
// delivery runner holds its relay table for the life of the process.
func (t *Table) Equal(o *Table) bool {
	if t == nil || o == nil {
		return t == o
	}
	if len(t.groups) != len(o.groups) {
		return false
	}
	for name, g := range t.groups {
		og, ok := o.groups[name]
		if !ok || !reflect.DeepEqual(g, og) {
			return false
		}
	}
	return true
}

// Lookup returns the named group. The bool result distinguishes "absent" from
// "present but empty", and an unknown name can never resolve to an inherited
// property the way a JavaScript `in` check does.
func (t *Table) Lookup(name string) (*Group, bool) {
	if t == nil {
		return nil, false
	}
	g, ok := t.groups[name]
	return g, ok
}

// Has reports whether the named group exists. Used by config validation to
// reject rules that route to a nonexistent group at load time.
func (t *Table) Has(name string) bool {
	_, ok := t.Lookup(name)
	return ok
}

// Names returns the configured group names, sorted, for logging and validation
// error messages.
func (t *Table) Names() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.groups))
	for n := range t.groups {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AuthenticatedMX lists relays that both resolve their exchange by MX and send
// credentials, so startup and `check` can warn.
//
// Rarely what anyone means. use_mx says "whichever host the DNS names today",
// and a password is a relationship with one specific host — so this combination
// sends a credential wherever a zone the operator may not control happens to
// point. Legitimate for a smarthost whose owner runs several MX hosts under one
// account, which is why it warns rather than refuses.
func (t *Table) AuthenticatedMX() []string {
	var out []string
	for _, name := range t.Names() {
		for _, m := range t.groups[name].Members {
			if m.UseMX && m.AuthUser != "" {
				out = append(out, name+"/"+m.Name)
			}
		}
	}
	return out
}

// PlaintextCredentials lists relays carrying a literal auth_pass, so startup can
// warn about them without printing the value.
func (t *Table) PlaintextCredentials() []string {
	var out []string
	for _, name := range t.Names() {
		for _, m := range t.groups[name].Members {
			if m.AuthPass != "" && m.AuthPassEnv == "" {
				out = append(out, name+"/"+m.Name)
			}
		}
	}
	return out
}
