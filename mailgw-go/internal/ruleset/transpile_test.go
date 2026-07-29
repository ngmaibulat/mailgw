package ruleset

import (
	"strings"
	"testing"
)

// The transpiler's whole justification is that it changes nothing. These tests
// check that claim directly, by running both matchers over the same envelopes
// and requiring identical answers — not by reading the generated rules.

// envelopeMatrix is a cross product of senders and recipients chosen to hit the
// edges: case differences, subdomains, bare local parts, the null sender, and
// addresses with more than one "@".
func envelopeMatrix() (senders, rcpts []string) {
	senders = []string{
		"", // null sender: every bounce uses it
		"me@ngm.dev", "ME@NGM.DEV", "me@mail.ngm.dev",
		"me@elsewhere.com", "postmaster", "a@b@ngm.dev",
	}
	rcpts = []string{
		"you@ngm.dev", "YOU@NGM.DEV", "you@mail.ngm.dev",
		"you@elsewhere.com", "you@ngm.dev.evil.com", "postmaster",
		"ops@ngm.dev", "a@b@elsewhere.com",
	}
	return senders, rcpts
}

// assertEquivalent requires the compiled ruleset to agree with the legacy
// matcher on every envelope in the matrix.
func assertEquivalent(t *testing.T, table *LegacyTable, groups ...string) {
	t.Helper()

	rs, err := Compile(table.Transpile(), relayTable(t, groups...), DefaultSchema())
	if err != nil {
		t.Fatalf("Compile(Transpile()): %v", err)
	}

	senders, rcpts := envelopeMatrix()
	for _, s := range senders {
		for _, r := range rcpts {
			wantGroup, wantOK := table.Find(s, r)

			d, ok := rs.Route(EnvForPair(s, r), StageData)
			if !ok {
				t.Fatalf("sender=%q rcpt=%q: the data stage must always decide", s, r)
			}
			gotGroup, gotOK := d.Relay()

			if gotOK != wantOK || gotGroup != wantGroup {
				t.Errorf("sender=%q rcpt=%q: legacy gave (%q,%v), DSL gave (%q,%v) via rule %q",
					s, r, wantGroup, wantOK, gotGroup, gotOK, d.Rule)
			}
			// Where the legacy table had no route, Haraka answered DENYSOFT
			// "No route found"; the DSL must reach the same default.
			if !wantOK && d.Action.Kind != ActTempfail {
				t.Errorf("sender=%q rcpt=%q: unrouted mail must tempfail, got %s", s, r, d.Action)
			}
		}
	}
}

func TestTranspile_MatchesTheShippedTable(t *testing.T) {
	assertEquivalent(t, &LegacyTable{Rules: []LegacyRule{
		{RouteName: "To Exchange", RcptDomain: "ngm.dev", Relay: "Exchange"},
		{RouteName: "Default", Relay: "Outbound"},
	}}, "Exchange", "Outbound")
}

func TestTranspile_MatchesEveryFieldCombination(t *testing.T) {
	assertEquivalent(t, &LegacyTable{Rules: []LegacyRule{
		{RouteName: "exact pair", Sender: "me@ngm.dev", Rcpt: "you@ngm.dev", Relay: "Exchange"},
		{RouteName: "sender domain", SenderDomain: "ngm.dev", Relay: "Outbound"},
		{RouteName: "rcpt domain", RcptDomain: "elsewhere.com", Relay: "Exchange"},
		{RouteName: "both domains", SenderDomain: "mail.ngm.dev", RcptDomain: "ngm.dev", Relay: "Outbound"},
		{RouteName: "rcpt only", Rcpt: "postmaster", Relay: "Exchange"},
	}}, "Exchange", "Outbound")
}

// No catch-all, so most of the matrix falls through to the default action —
// the case where the two implementations are easiest to get subtly wrong.
func TestTranspile_MatchesWhenMostMailIsUnrouted(t *testing.T) {
	assertEquivalent(t, &LegacyTable{Rules: []LegacyRule{
		{RouteName: "only ngm.dev", RcptDomain: "ngm.dev", Relay: "Exchange"},
	}}, "Exchange", "Outbound")
}

func TestTranspile_PreservesFirstMatchWins(t *testing.T) {
	assertEquivalent(t, &LegacyTable{Rules: []LegacyRule{
		{RouteName: "first", RcptDomain: "ngm.dev", Relay: "Exchange"},
		{RouteName: "shadowed", RcptDomain: "ngm.dev", Relay: "Outbound"},
		{RouteName: "catch all", Relay: "Outbound"},
	}}, "Exchange", "Outbound")
}

func TestTranspile_CatchAllRoundTripsThroughYAML(t *testing.T) {
	table := &LegacyTable{Rules: []LegacyRule{
		{RouteName: "To Exchange", RcptDomain: "ngm.dev", Relay: "Exchange"},
		{RouteName: "Default", Relay: "Outbound"},
	}}

	out, err := table.Transpile().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// An empty `all: []` would vanish on marshal and leave a predicate with no
	// branch key, which is why the catch-all is emitted as `always: true`.
	if !strings.Contains(string(out), "always: true") {
		t.Fatalf("the catch-all must survive marshalling:\n%s", out)
	}

	reloaded, err := LoadFile(writeFile(t, "routing.yaml", string(out)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	rs, err := Compile(reloaded, relayTable(t, "Exchange", "Outbound"), DefaultSchema())
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}

	senders, rcpts := envelopeMatrix()
	for _, s := range senders {
		for _, r := range rcpts {
			want, _ := table.Find(s, r)
			d, _ := rs.Route(EnvForPair(s, r), StageData)
			got, _ := d.Relay()
			if got != want {
				t.Errorf("after a YAML round trip, sender=%q rcpt=%q: got %q, want %q", s, r, got, want)
			}
		}
	}
}

func TestTranspile_KeepsRuleOrderViaAscendingPriorities(t *testing.T) {
	f := (&LegacyTable{Rules: []LegacyRule{
		{RouteName: "a", Relay: "Exchange"},
		{RouteName: "b", Relay: "Outbound"},
		{RouteName: "c", Relay: "Exchange"},
	}}).Transpile()

	for i := 1; i < len(f.Routes); i++ {
		if f.Routes[i].Priority <= f.Routes[i-1].Priority {
			t.Errorf("rule %d has priority %d, not above %d",
				i, f.Routes[i].Priority, f.Routes[i-1].Priority)
		}
	}
	// Spaced out so an operator can insert a rule without renumbering.
	if f.Routes[1].Priority-f.Routes[0].Priority < 2 {
		t.Error("priorities should leave room between converted rules")
	}
}

func TestTranspile_UnnamedRulesGetPositionalNames(t *testing.T) {
	f := (&LegacyTable{Rules: []LegacyRule{{Relay: "Exchange"}}}).Transpile()
	if f.Routes[0].Name == "" {
		t.Error("every rule needs a name, or explain output is useless")
	}
}
