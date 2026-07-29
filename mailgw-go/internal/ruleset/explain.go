package ruleset

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Rule evaluation statuses reported by Explain.
const (
	StatusMatched  = "matched"
	StatusNoMatch  = "no-match"
	StatusDeferred = "deferred" // needs a later stage; blocks the walk
	StatusSkipped  = "skipped"  // never reached, because an earlier rule decided
)

// RuleTrace records what happened to one rule during an explained evaluation.
type RuleTrace struct {
	List      string // "policy" or "routes"
	Name      string
	Priority  int
	Stage     Stage
	Predicate string
	Status    string
	Actions   []Action
}

// Explanation is the full record of one routing decision, for
// `mailgw-go explain`. Answering "why did this go there?" from a log line is
// the difference between a routing table an operator trusts and one they
// guess at.
type Explanation struct {
	Stage    Stage
	Sender   string
	Rcpt     string
	Traces   []RuleTrace
	Decision Decision
	Decided  bool
	Policy   PolicyResult
	PolicyAt Stage
}

// Explain evaluates the ruleset against env exactly as Route would, recording
// every rule it considered.
func (rs *Ruleset) Explain(env *Env, at Stage) *Explanation {
	ex := &Explanation{Stage: at}
	if env.Mail != nil {
		ex.Sender = env.Mail.From
	}
	if env.Rcpt != nil {
		ex.Rcpt = env.Rcpt.To
	}
	if rs == nil {
		return ex
	}

	// Policy runs stage by stage, so a rule fires as early as its fields
	// allow. Stop at the first terminal action, as the live path does.
	for st := StageConnect; st <= at; st++ {
		for i := range rs.Policy {
			r := &rs.Policy[i]
			if r.Stage != st {
				continue
			}
			t := RuleTrace{List: "policy", Name: r.Name, Priority: r.Priority,
				Stage: r.Stage, Predicate: r.Describe(), Actions: r.Then}
			if ex.Policy.Action != nil {
				t.Status = StatusSkipped
				ex.Traces = append(ex.Traces, t)
				continue
			}
			if !r.Match(env) {
				t.Status = StatusNoMatch
				ex.Traces = append(ex.Traces, t)
				continue
			}
			t.Status = StatusMatched
			ex.Traces = append(ex.Traces, t)
			for _, a := range r.Then {
				switch a.Kind {
				case ActTag:
					env.SetTag(a.Key, a.Value)
				case ActAddHeader:
					ex.Policy.Headers = append(ex.Policy.Headers, Header{Name: a.Name, Value: a.Value})
				default:
					act := a
					ex.Policy.Rule, ex.Policy.Action, ex.PolicyAt = r.Name, &act, st
				}
				if ex.Policy.Action != nil {
					break
				}
			}
		}
	}

	var headers []Header

	// A policy refusal ends the recipient's journey, so the live path never
	// consults the routes at all. Reporting a destination here would describe
	// something that cannot happen.
	blocked := false
	if a := ex.Policy.Action; a != nil {
		switch a.Kind {
		case ActReject, ActTempfail, ActDiscard:
			blocked = true
		}
	}

	for i := range rs.Routes {
		r := &rs.Routes[i]
		t := RuleTrace{List: "routes", Name: r.Name, Priority: r.Priority,
			Stage: r.Stage, Predicate: r.Describe(), Actions: r.Then}

		switch {
		case blocked:
			t.Status = StatusSkipped
		case r.Stage > at:
			// The walk stops here: a lower-priority rule must not win just
			// because a higher-priority one is not evaluable yet.
			t.Status = StatusDeferred
			blocked = true
		case !r.Match(env):
			t.Status = StatusNoMatch
		default:
			t.Status = StatusMatched
		}
		ex.Traces = append(ex.Traces, t)

		if t.Status != StatusMatched {
			continue
		}
		decided := false
		for _, a := range r.Then {
			switch a.Kind {
			case ActTag:
				env.SetTag(a.Key, a.Value)
			case ActAddHeader:
				headers = append(headers, Header{Name: a.Name, Value: a.Value})
			default:
				ex.Decision = Decision{Rule: r.Name, Action: a, Headers: headers, Tags: env.Tags}
				ex.Decided = true
				decided = true
			}
			if decided {
				break
			}
		}
		if decided {
			blocked = true
		}
	}

	// The default only applies once every rule has had its chance, which
	// mirrors Route.
	if !ex.Decided && !blocked && at >= StageData {
		ex.Decision = Decision{Action: rs.Default, Headers: headers, Tags: env.Tags}
		ex.Decided = true
	}
	return ex
}

// FieldsUsed lists every field name any rule mentions, sorted.
//
// `check` uses it to warn about rules that reference facts this build does not
// yet populate — a rule that can never match is worse than a rule that errors,
// because nothing reports it.
func (rs *Ruleset) FieldsUsed() []string {
	seen := map[string]struct{}{}
	for _, list := range [][]CompiledRule{rs.Policy, rs.Routes} {
		for i := range list {
			if list[i].src != nil {
				collectFields(&list[i].src.Match, seen)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func collectFields(p *Pred, into map[string]struct{}) {
	if p == nil {
		return
	}
	if p.Field != "" {
		into[p.Field] = struct{}{}
	}
	for i := range p.All {
		collectFields(&p.All[i], into)
	}
	for i := range p.Any {
		collectFields(&p.Any[i], into)
	}
	collectFields(p.Not, into)
	collectFields(p.Every, into)
}

// Print renders an explanation as human-readable text.
func (ex *Explanation) Print(w io.Writer) {
	fmt.Fprintf(w, "evaluating at stage %s\n", ex.Stage)
	if ex.Sender != "" || ex.Rcpt != "" {
		fmt.Fprintf(w, "  sender: %s\n  rcpt:   %s\n", quoteOrNull(ex.Sender), ex.Rcpt)
	}
	fmt.Fprintln(w)

	list := ""
	for _, t := range ex.Traces {
		if t.List != list {
			list = t.List
			fmt.Fprintf(w, "%s:\n", strings.ToUpper(list))
		}
		fmt.Fprintf(w, "  [%-9s] %-32s prio=%-5d stage=%-7s %s\n",
			t.Status, t.Name, t.Priority, t.Stage, t.Predicate)
	}
	fmt.Fprintln(w)

	if ex.Policy.Action != nil {
		fmt.Fprintf(w, "POLICY: %s at %s (rule %q)\n", ex.Policy.Action, ex.PolicyAt, ex.Policy.Rule)
	}
	for _, h := range ex.Policy.Headers {
		fmt.Fprintf(w, "  + %s: %s\n", h.Name, h.Value)
	}

	switch {
	case ex.Policy.Action != nil && !ex.Decided:
		fmt.Fprintf(w, "ROUTE:  not reached — policy ended this recipient\n")
	case !ex.Decided:
		fmt.Fprintf(w, "ROUTE:  undecided at this stage — a higher-priority rule needs a later stage\n")
	case ex.Decision.Rule == "":
		fmt.Fprintf(w, "ROUTE:  %s (default_action — no rule matched)\n", ex.Decision.Action)
	default:
		fmt.Fprintf(w, "ROUTE:  %s (rule %q)\n", ex.Decision.Action, ex.Decision.Rule)
	}
	for _, h := range ex.Decision.Headers {
		fmt.Fprintf(w, "  + %s: %s\n", h.Name, h.Value)
	}
	for k, v := range ex.Decision.Tags {
		fmt.Fprintf(w, "  # tag %s=%s\n", k, v)
	}
}

func quoteOrNull(s string) string {
	if s == "" {
		return "<> (null sender)"
	}
	return s
}
