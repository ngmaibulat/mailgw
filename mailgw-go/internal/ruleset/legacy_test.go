package ruleset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func relayTable(t *testing.T, groups ...string) *relays.Table {
	t.Helper()
	body := "{"
	for i, g := range groups {
		if i > 0 {
			body += ","
		}
		body += `"` + g + `":[{"name":"m","exchange":"h.example.com","port":25}]`
	}
	body += "}"
	tbl, err := relays.Load(writeFile(t, "relays.json", body))
	if err != nil {
		t.Fatalf("relays.Load: %v", err)
	}
	return tbl
}

// --- Domain: mirrors the getDomain cases in mailgw/tests/Route.test.js ---

func TestDomain_UsesLastAtSign(t *testing.T) {
	cases := map[string]string{
		"user@example.com":       "example.com",
		"weird@name@ngm.dev":     "ngm.dev", // last "@" wins
		"a@b.co.uk":              "b.co.uk",
		"user@":                  "",
		"@example.com":           "example.com",
		"UPPER@Example.COM":      "Example.COM", // no case folding here
		"first.last@sub.ngm.dev": "sub.ngm.dev",
	}
	for in, want := range cases {
		if got := Domain(in); got != want {
			t.Errorf("Domain(%q): got %q, want %q", in, got, want)
		}
	}
}

// Route.js:16 returns the whole string when there is no "@", so "postmaster"
// reports itself as a domain. That is a bug and this pins the fix.
func TestDomain_NoAtSignHasNoDomain(t *testing.T) {
	for _, in := range []string{"postmaster", "", "localpart"} {
		if got := Domain(in); got != "" {
			t.Errorf("Domain(%q): got %q, want \"\" (Route.js:16 would return the whole string)", in, got)
		}
	}
}

// A bare local part must not satisfy a rcpt_domain rule.
func TestDomain_BareLocalPartDoesNotMatchDomainRule(t *testing.T) {
	tbl := &LegacyTable{Rules: []LegacyRule{
		{RouteName: "exchange", RcptDomain: "postmaster", Relay: "Exchange"},
		{RouteName: "default", Relay: "Outbound"},
	}}
	got, ok := tbl.Find("a@x.com", "postmaster")
	if !ok {
		t.Fatal("the catch-all should match")
	}
	if got != "Outbound" {
		t.Errorf("got %q, want Outbound — a bare local part must not match a rcpt_domain rule", got)
	}
}

// --- matching: the wildcard / exact / case-insensitivity / AND cases ---

func TestMatch_EmptyFieldsAreWildcards(t *testing.T) {
	r := LegacyRule{Relay: "Outbound"}
	if !r.match("anyone@anywhere.com", "someone@else.org") {
		t.Error("an all-empty rule must match everything")
	}
	if !r.match("", "") {
		t.Error("an all-empty rule must match empty addresses too")
	}
}

func TestMatch_ExactFields(t *testing.T) {
	cases := []struct {
		name string
		rule LegacyRule
		s, r string
		want bool
	}{
		{"sender match", LegacyRule{Sender: "me@ngm.dev"}, "me@ngm.dev", "x@y.com", true},
		{"sender miss", LegacyRule{Sender: "me@ngm.dev"}, "you@ngm.dev", "x@y.com", false},
		{"sender_domain match", LegacyRule{SenderDomain: "ngm.dev"}, "me@ngm.dev", "x@y.com", true},
		{"sender_domain miss", LegacyRule{SenderDomain: "ngm.dev"}, "me@other.com", "x@y.com", false},
		{"rcpt match", LegacyRule{Rcpt: "x@y.com"}, "me@ngm.dev", "x@y.com", true},
		{"rcpt miss", LegacyRule{Rcpt: "x@y.com"}, "me@ngm.dev", "z@y.com", false},
		{"rcpt_domain match", LegacyRule{RcptDomain: "y.com"}, "me@ngm.dev", "x@y.com", true},
		{"rcpt_domain miss", LegacyRule{RcptDomain: "y.com"}, "me@ngm.dev", "x@z.com", false},
	}
	for _, c := range cases {
		if got := c.rule.match(c.s, c.r); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMatch_IsCaseInsensitive(t *testing.T) {
	r := LegacyRule{Sender: "Me@NGM.dev", RcptDomain: "Example.COM"}
	if !r.match("me@ngm.DEV", "x@example.com") {
		t.Error("addresses and domains must compare case-insensitively")
	}
}

// Route.js:24-28 ANDs all four predicates.
func TestMatch_AndsAllPredicates(t *testing.T) {
	r := LegacyRule{SenderDomain: "ngm.dev", RcptDomain: "partner.com"}
	if !r.match("me@ngm.dev", "them@partner.com") {
		t.Error("both predicates satisfied should match")
	}
	if r.match("me@other.com", "them@partner.com") {
		t.Error("sender_domain unsatisfied must not match")
	}
	if r.match("me@ngm.dev", "them@other.com") {
		t.Error("rcpt_domain unsatisfied must not match")
	}
}

// A subdomain does NOT match its parent — parity with Haraka, and one of the
// limitations the M2 DSL exists to lift.
func TestMatch_SubdomainDoesNotMatchParent(t *testing.T) {
	r := LegacyRule{RcptDomain: "ngm.dev"}
	if r.match("me@x.com", "them@mail.ngm.dev") {
		t.Error("exact equality means mail.ngm.dev must not match ngm.dev")
	}
}

// A null sender (MAIL FROM:<>) arrives as "" and must still route via the
// catch-all rather than erroring.
func TestMatch_NullSenderRoutesViaCatchAll(t *testing.T) {
	tbl := &LegacyTable{Rules: []LegacyRule{
		{RouteName: "exchange", RcptDomain: "ngm.dev", Relay: "Exchange"},
		{RouteName: "default", Relay: "Outbound"},
	}}
	if got, ok := tbl.Find("", "them@elsewhere.com"); !ok || got != "Outbound" {
		t.Errorf("got (%q,%v), want (Outbound,true)", got, ok)
	}
	if got, ok := tbl.Find("", "them@ngm.dev"); !ok || got != "Exchange" {
		t.Errorf("got (%q,%v), want (Exchange,true)", got, ok)
	}
}

// --- table behaviour: mirrors mailgw/tests/RoutingTable.test.js ---

func TestFind_FirstMatchWins(t *testing.T) {
	tbl := &LegacyTable{Rules: []LegacyRule{
		{RouteName: "first", RcptDomain: "ngm.dev", Relay: "Exchange"},
		{RouteName: "second", RcptDomain: "ngm.dev", Relay: "Outbound"},
	}}
	got, ok := tbl.Find("me@x.com", "you@ngm.dev")
	if !ok || got != "Exchange" {
		t.Errorf("got (%q,%v), want (Exchange,true)", got, ok)
	}
	r, _ := tbl.FindRule("me@x.com", "you@ngm.dev")
	if r.RouteName != "first" {
		t.Errorf("winning rule: got %q, want first", r.RouteName)
	}
}

func TestFind_NoMatch(t *testing.T) {
	tbl := &LegacyTable{Rules: []LegacyRule{
		{RouteName: "only", RcptDomain: "ngm.dev", Relay: "Exchange"},
	}}
	if got, ok := tbl.Find("me@x.com", "you@elsewhere.com"); ok {
		t.Errorf("got (%q,true), want no match", got)
	}
}

func TestFind_EmptyTable(t *testing.T) {
	tbl := &LegacyTable{}
	if _, ok := tbl.Find("a@b.com", "c@d.com"); ok {
		t.Error("an empty table must not match")
	}
}

// --- validation ---

func TestValidate_RejectsUnknownRelayGroup(t *testing.T) {
	tbl := &LegacyTable{Rules: []LegacyRule{
		{RouteName: "bad", Relay: "NoSuchGroup"},
	}}
	if err := tbl.Validate(relayTable(t, "Exchange", "Outbound")); err == nil {
		t.Fatal("an unknown relay group must be rejected at load time")
	}
}

// RoutingTable.js:29 uses `relayname in this.relays`, which walks the prototype
// chain — so a rule naming "toString" resolves truthy and hands a Function to
// Haraka. These names must be rejected outright.
func TestValidate_RejectsInheritedPropertyNames(t *testing.T) {
	for _, name := range []string{"toString", "constructor", "valueOf", "hasOwnProperty", "__proto__"} {
		tbl := &LegacyTable{Rules: []LegacyRule{{RouteName: "x", Relay: name}}}
		if err := tbl.Validate(relayTable(t, "Exchange")); err == nil {
			t.Errorf("relay %q must be rejected, not resolved via the prototype chain", name)
		}
	}
}

func TestValidate_RequiresRelay(t *testing.T) {
	tbl := &LegacyTable{Rules: []LegacyRule{{RouteName: "x", Relay: "  "}}}
	if err := tbl.Validate(relayTable(t, "Exchange")); err == nil {
		t.Fatal("a rule without a relay must be rejected")
	}
}

func TestValidate_AcceptsTheShippedRoutingTable(t *testing.T) {
	tbl, err := LoadLegacyRouting("../../testdata/config/routing.json")
	if err != nil {
		t.Fatalf("LoadLegacyRouting: %v", err)
	}
	if err := tbl.Validate(relayTable(t, "Exchange", "Outbound")); err != nil {
		t.Fatalf("the shipped routing.json should validate: %v", err)
	}

	// It routes ngm.dev to Exchange and everything else to Outbound.
	if got, _ := tbl.Find("me@x.com", "you@ngm.dev"); got != "Exchange" {
		t.Errorf("ngm.dev: got %q, want Exchange", got)
	}
	if got, _ := tbl.Find("me@x.com", "you@elsewhere.com"); got != "Outbound" {
		t.Errorf("catch-all: got %q, want Outbound", got)
	}
}

// --- loading ---

func TestLoadLegacyRouting_RejectsEmptyAndMalformed(t *testing.T) {
	if _, err := LoadLegacyRouting(writeFile(t, "routing.json", `[]`)); err == nil {
		t.Error("an empty route list must be rejected")
	}
	if _, err := LoadLegacyRouting(writeFile(t, "routing.json", `not json`)); err == nil {
		t.Error("malformed JSON must be rejected")
	}
	if _, err := LoadLegacyRouting(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing file must be rejected")
	}
}

// An object-shaped routing.json has no dependable order in Go, and order is
// precedence here, so it must be refused rather than routed arbitrarily.
func TestLoadLegacyRouting_RejectsAmbiguousObjectForm(t *testing.T) {
	body := `{"a":{"rcpt_domain":"ngm.dev","relay":"Exchange"},"b":{"relay":"Outbound"}}`
	if _, err := LoadLegacyRouting(writeFile(t, "routing.json", body)); err == nil {
		t.Error("a multi-entry object form must be rejected as order-ambiguous")
	}
}
