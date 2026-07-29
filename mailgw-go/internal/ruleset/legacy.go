// Package ruleset evaluates routing and policy rules.
//
// legacy.go implements the Haraka-compatible routing table: the four-field,
// exact-match model from mailgw/plugins/Route.js and RoutingTable.js. M1 routes
// exclusively through this, so behaviour is bit-for-bit comparable with the
// gateway it replaces; the general predicate DSL lands in M2 and compiles into
// the same decision type.
package ruleset

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// LegacyRule mirrors one entry of mailgw/config/routing.json.
//
// An empty (or absent) predicate field is a wildcard, exactly as
// Route.js:getCheckerFunction treats null/undefined/"".
type LegacyRule struct {
	RouteName    string `json:"routename"`
	Sender       string `json:"sender"`
	SenderDomain string `json:"sender_domain"`
	Rcpt         string `json:"rcpt"`
	RcptDomain   string `json:"rcpt_domain"`
	Relay        string `json:"relay"`
}

// LegacyTable is an ordered list of rules; the first match wins
// (RoutingTable.js:22).
type LegacyTable struct {
	Rules []LegacyRule
}

// LoadLegacyRouting reads routing.json. Haraka's npRoute.js:102-110 builds its
// rules with Object.values(), so the file works as either a JSON array or an
// object map; both are accepted here.
func LoadLegacyRouting(path string) (*LegacyTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var rules []LegacyRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		var byName map[string]LegacyRule
		if err2 := json.Unmarshal(raw, &byName); err2 != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		// Object form: Object.values() order is insertion order in V8, but Go
		// maps are unordered, so an object-shaped routing.json cannot have a
		// dependable precedence. Reject it rather than route unpredictably.
		if len(byName) > 1 {
			return nil, fmt.Errorf("%s: object form has no dependable rule order; use a JSON array", path)
		}
		for _, r := range byName {
			rules = append(rules, r)
		}
	}

	if len(rules) == 0 {
		return nil, fmt.Errorf("%s: no routes defined", path)
	}
	return &LegacyTable{Rules: rules}, nil
}

// Validate checks every rule against the relay table.
//
// This is where the prototype-chain bug at RoutingTable.js:29 is closed: that
// code tests `relayname in this.relays`, so a rule naming "toString" passes and
// hands a Function to Haraka as an MX list. relays.Table.Has is a plain map
// lookup, and an unknown name is a hard config error at load rather than a
// surprise at delivery time.
func (t *LegacyTable) Validate(tbl *relays.Table) error {
	for i, r := range t.Rules {
		name := r.RouteName
		if name == "" {
			name = fmt.Sprintf("#%d", i)
		}
		if strings.TrimSpace(r.Relay) == "" {
			return fmt.Errorf("route %s: 'relay' is required", name)
		}
		if !tbl.Has(r.Relay) {
			return fmt.Errorf("route %s: relay group %q is not defined in relays.json (defined: %v)",
				name, r.Relay, tbl.Names())
		}
	}
	return nil
}

// Find returns the relay-group name for a (sender, rcpt) pair, and whether any
// rule matched. Rules are tried in order; the first match wins.
func (t *LegacyTable) Find(sender, rcpt string) (string, bool) {
	for _, r := range t.Rules {
		if r.match(sender, rcpt) {
			return r.Relay, true
		}
	}
	return "", false
}

// FindRule is Find, but also reports which rule won — used by `explain` and by
// the parity harness.
func (t *LegacyTable) FindRule(sender, rcpt string) (LegacyRule, bool) {
	for _, r := range t.Rules {
		if r.match(sender, rcpt) {
			return r, true
		}
	}
	return LegacyRule{}, false
}

// match ANDs the four predicates, as Route.js:24-28 does.
func (r LegacyRule) match(sender, rcpt string) bool {
	return eqOrWildcard(r.Sender, sender) &&
		eqOrWildcard(r.SenderDomain, Domain(sender)) &&
		eqOrWildcard(r.Rcpt, rcpt) &&
		eqOrWildcard(r.RcptDomain, Domain(rcpt))
}

// eqOrWildcard reproduces Route.js:getCheckerFunction — an empty expectation
// matches anything, otherwise comparison is case-insensitive equality.
func eqOrWildcard(expected, actual string) bool {
	if expected == "" {
		return true
	}
	return strings.EqualFold(expected, actual)
}

// Domain extracts the domain from an address.
//
// Route.js:16 uses addr.substring(addr.lastIndexOf("@") + 1), which returns the
// *whole string* when there is no "@" — so a bare local part like "postmaster"
// is reported as the domain "postmaster" and can match a rcpt_domain rule it has
// nothing to do with. Here an address without "@" has no domain.
func Domain(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return ""
	}
	return addr[i+1:]
}
