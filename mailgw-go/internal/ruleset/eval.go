package ruleset

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// Header is a header to prepend to the routed copy of a message.
type Header struct {
	Name  string
	Value string
}

// CompiledRule is one validated, compiled rule.
type CompiledRule struct {
	Name     string
	Priority int
	Stage    Stage
	Index    int // position in the source file, for a stable sort
	Then     []Action

	// RcptScoped rules read rcpt.* and so are evaluated once per recipient.
	// Everything else is evaluated once per message.
	RcptScoped bool

	pred matcher
	src  *Rule
}

// Match reports whether the rule's predicate holds. The caller is responsible
// for only asking at or after the rule's stage.
func (r *CompiledRule) Match(env *Env) bool { return r.pred.Match(env) }

// Describe renders the rule's predicate, for `explain` and error messages.
func (r *CompiledRule) Describe() string {
	if r.src == nil {
		return ""
	}
	return r.src.Match.describe()
}

// Ruleset is an immutable, compiled configuration. Reloads build a new one and
// swap the pointer, so a session that has already started keeps the rules it
// began with.
type Ruleset struct {
	Policy  []CompiledRule
	Routes  []CompiledRule
	Default Action
	Schema  *Schema
}

// Decision is the outcome of routing one recipient.
type Decision struct {
	// Rule names the rule that produced Action, or "" when the default
	// applied.
	Rule    string
	Action  Action
	Headers []Header
	Tags    map[string]string
}

// Relay returns the relay group when the decision is to relay.
func (d Decision) Relay() (string, bool) {
	if d.Action.Kind == ActRelay {
		return d.Action.Relay, true
	}
	return "", false
}

// PolicyResult is the outcome of a policy pass at one stage.
type PolicyResult struct {
	Rule    string
	Action  *Action // nil when no rule reached a terminal action
	Headers []Header
}

// DefaultAction is what applies when no route matches: mailgw/plugins/
// npRoute.js:65 answers DENYSOFT "No route found", so a misconfiguration holds
// mail rather than bouncing it.
func DefaultAction() Action {
	return Action{Kind: ActTempfail, Code: 451, Enhanced: "4.7.1", Message: "No route found"}
}

// Compile validates a rule file against the relay table and the field schema,
// and produces an immutable ruleset.
//
// Everything that can be checked is checked here — unknown fields, type
// mismatches, bad regexes and CIDRs, unknown relay groups, unreachable actions
// — so `mailgw-go check` is a genuine gate rather than a syntax check.
func Compile(f *File, tbl *relays.Table, sc *Schema) (*Ruleset, error) {
	if sc == nil {
		sc = DefaultSchema()
	}
	rs := &Ruleset{Schema: sc, Default: DefaultAction()}

	if f.DefaultAction != nil {
		a := *f.DefaultAction
		if err := validateAction(&a, tbl, "default_action", "routes"); err != nil {
			return nil, err
		}
		if !Terminal(a.Kind) {
			return nil, fmt.Errorf("default_action: %q is not a terminal action", a.Kind)
		}
		rs.Default = a
	}

	var err error
	if rs.Policy, err = compileRules(f.Policy, tbl, sc, "policy"); err != nil {
		return nil, err
	}
	if rs.Routes, err = compileRules(f.Routes, tbl, sc, "routes"); err != nil {
		return nil, err
	}
	return rs, nil
}

func compileRules(rules []Rule, tbl *relays.Table, sc *Schema, list string) ([]CompiledRule, error) {
	out := make([]CompiledRule, 0, len(rules))

	seen := map[string]int{}
	for i := range rules {
		r := &rules[i]
		name := strings.TrimSpace(r.Name)
		if name == "" {
			name = fmt.Sprintf("%s[%d]", list, i)
		}
		path := fmt.Sprintf("%s[%d] (%s)", list, i, name)

		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("%s: duplicate rule name, already used by %s[%d]", path, list, prev)
		}
		seen[name] = i

		if r.Disabled {
			continue
		}

		n, err := compilePred(&r.Match, sc, path)
		if err != nil {
			return nil, err
		}

		stage := n.stage
		if r.Stage != "" {
			forced, err := ParseStage(r.Stage)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if forced < n.stage {
				return nil, fmt.Errorf("%s: stage %q is earlier than %q, which is when its fields become available",
					path, forced, n.stage)
			}
			stage = forced
		}

		if len(r.Then) == 0 {
			return nil, fmt.Errorf("%s: rule has no actions", path)
		}
		actions := make([]Action, 0, len(r.Then))
		for j := range r.Then {
			a := r.Then[j]
			ap := fmt.Sprintf("%s then[%d]", path, j)
			if err := validateAction(&a, tbl, ap, list); err != nil {
				return nil, err
			}
			if j > 0 && Terminal(actions[j-1].Kind) {
				return nil, fmt.Errorf("%s: unreachable — %q already ends evaluation for this rule",
					ap, actions[j-1].Kind)
			}
			// There is no message to drop before MAIL FROM, so a rule that
			// could fire at connect or helo cannot discard or quarantine one.
			// Catching it here means the session never has to decide what such
			// an action would mean.
			if (a.Kind == ActDiscard || a.Kind == ActQuarantine) && stage < StageMail {
				return nil, fmt.Errorf("%s: %q needs a message, but this rule evaluates at %s; add `stage: mail` or later",
					ap, a.Kind, stage)
			}
			actions = append(actions, a)
		}

		out = append(out, CompiledRule{
			Name:     name,
			Priority: r.Priority,
			Stage:    stage,
			Index:    i,
			Then:     actions,
			// A rule forced to `stage: rcpt` is per-recipient even if its
			// predicate happens not to read a rcpt field — that is what the
			// author asked for.
			RcptScoped: n.usesRcpt || stage == StageRcpt,
			pred:       n.matcher,
			src:        r,
		})
	}

	// Lower priority number wins; ties keep file order. Stable, so the same
	// file always produces the same precedence.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Index < out[j].Index
	})
	return out, nil
}

func validateAction(a *Action, tbl *relays.Table, path, list string) error {
	switch a.Kind {
	case "":
		return fmt.Errorf("%s: missing `action`", path)

	case ActRelay:
		if list == "policy" {
			return fmt.Errorf("%s: `relay` belongs in `routes`, not `policy`", path)
		}
		if strings.TrimSpace(a.Relay) == "" {
			return fmt.Errorf("%s: `relay` needs a relay group name", path)
		}
		// The config-level fix for RoutingTable.js:29, which tested
		// `relayname in this.relays` and so accepted "toString", handing a
		// Function to Haraka as an MX list.
		if tbl != nil && !tbl.Has(a.Relay) {
			return fmt.Errorf("%s: relay group %q is not defined in relays.json (defined: %v)",
				path, a.Relay, tbl.Names())
		}

	case ActReject:
		if a.Code == 0 {
			a.Code = 550
		}
		if a.Code < 500 || a.Code > 599 {
			return fmt.Errorf("%s: reject needs a 5xx code, got %d", path, a.Code)
		}
		if a.Enhanced == "" {
			a.Enhanced = "5.7.1"
		}
		if a.Message == "" {
			a.Message = "Rejected by policy"
		}

	case ActTempfail:
		if a.Code == 0 {
			a.Code = 451
		}
		if a.Code < 400 || a.Code > 499 {
			return fmt.Errorf("%s: tempfail needs a 4xx code, got %d", path, a.Code)
		}
		if a.Enhanced == "" {
			a.Enhanced = "4.7.1"
		}
		if a.Message == "" {
			a.Message = "Deferred by policy"
		}

	case ActDiscard, ActQuarantine:
		// No parameters.

	case ActAccept:
		// `accept` means "stop applying policy to this message". In `routes`
		// it would name no destination, leaving the recipient unroutable, so
		// it is rejected there rather than silently blackholing mail.
		if list == "routes" {
			return fmt.Errorf("%s: `accept` belongs in `policy`; a route must name a destination", path)
		}

	case ActAddHeader:
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("%s: add_header needs a `name`", path)
		}
		if strings.ContainsAny(a.Name, ":\r\n") {
			return fmt.Errorf("%s: header name %q must not contain ':' or a line break", path, a.Name)
		}

	case ActTag:
		if strings.TrimSpace(a.Key) == "" {
			return fmt.Errorf("%s: tag needs a `key`", path)
		}

	default:
		return fmt.Errorf("%s: unknown action %q", path, a.Kind)
	}

	// Anything that reaches a header or an SMTP reply must not be able to
	// inject a line break.
	for label, s := range map[string]string{"message": a.Message, "value": a.Value} {
		if strings.ContainsAny(s, "\r\n") {
			return fmt.Errorf("%s: %s must not contain a line break", path, label)
		}
	}

	if enh := strings.TrimSpace(a.Enhanced); enh != "" {
		if _, err := ParseEnhanced(enh); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// ParseEnhanced splits an enhanced status code such as "5.7.1".
func ParseEnhanced(s string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("enhanced status %q must have three parts", s)
	}
	for i, p := range parts {
		n := 0
		if p == "" {
			return out, fmt.Errorf("enhanced status %q has an empty part", s)
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, fmt.Errorf("enhanced status %q is not numeric", s)
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, nil
}

// EvalPolicy runs the message-scoped policy rules whose stage is exactly `at`,
// so each rule fires once, as early as its fields allow.
//
// Non-terminal actions (tag, add_header) accumulate and evaluation continues;
// the first terminal action wins and is returned.
func (rs *Ruleset) EvalPolicy(env *Env, at Stage) PolicyResult {
	return rs.evalPolicy(env, at, false)
}

// EvalPolicyRcpt runs the recipient-scoped policy rules at `at`, against the
// recipient currently bound in env. Callers invoke it once per recipient.
func (rs *Ruleset) EvalPolicyRcpt(env *Env, at Stage) PolicyResult {
	return rs.evalPolicy(env, at, true)
}

func (rs *Ruleset) evalPolicy(env *Env, at Stage, perRcpt bool) PolicyResult {
	var res PolicyResult
	if rs == nil {
		return res
	}
	for i := range rs.Policy {
		r := &rs.Policy[i]
		if r.Stage != at || r.RcptScoped != perRcpt || !r.Match(env) {
			continue
		}
		for _, a := range r.Then {
			switch a.Kind {
			case ActTag:
				env.SetTag(a.Key, a.Value)
			case ActAddHeader:
				res.Headers = append(res.Headers, Header{Name: a.Name, Value: a.Value})
			default:
				act := a
				res.Rule, res.Action = r.Name, &act
				return res
			}
		}
	}
	return res
}

// Route decides where one recipient goes.
//
// Rules are walked in priority order. The walk stops at the first rule whose
// stage has not been reached yet and reports "not decided": a lower-priority
// rule must not win just because a higher-priority one needs a header that has
// not arrived. Re-running at a later stage therefore always produces the same
// answer as running once at the end — the property that lets a decision be made
// at RCPT when it can be, and deferred to DATA when it cannot.
func (rs *Ruleset) Route(env *Env, at Stage) (Decision, bool) {
	if rs == nil {
		return Decision{}, false
	}

	var headers []Header
	for i := range rs.Routes {
		r := &rs.Routes[i]
		if r.Stage > at {
			return Decision{}, false
		}
		if !r.Match(env) {
			continue
		}
		for _, a := range r.Then {
			switch a.Kind {
			case ActTag:
				env.SetTag(a.Key, a.Value)
			case ActAddHeader:
				headers = append(headers, Header{Name: a.Name, Value: a.Value})
			default:
				return Decision{Rule: r.Name, Action: a, Headers: headers, Tags: env.Tags}, true
			}
		}
	}

	// No rule matched. The default only applies once every rule has genuinely
	// had its chance — at an earlier stage, "nothing matched yet" is not the
	// same as "nothing will match". Deferring here is also what preserves
	// Haraka's timing: npRoute.js runs at hook_get_mx, so "No route found"
	// arrives after DATA, not at RCPT.
	if at < StageData {
		return Decision{}, false
	}
	return Decision{Action: rs.Default, Headers: headers, Tags: env.Tags}, true
}

// Groups lists every relay group a route can select, for `check`.
func (rs *Ruleset) Groups() []string {
	seen := map[string]struct{}{}
	for i := range rs.Routes {
		for _, a := range rs.Routes[i].Then {
			if a.Kind == ActRelay {
				seen[a.Relay] = struct{}{}
			}
		}
	}
	if rs.Default.Kind == ActRelay {
		seen[rs.Default.Relay] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}
