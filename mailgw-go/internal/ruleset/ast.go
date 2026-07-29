package ruleset

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Op is a comparison operator.
type Op string

const (
	OpEq       Op = "eq"
	OpNe       Op = "ne"
	OpContains Op = "contains"
	OpPrefix   Op = "prefix"
	OpSuffix   Op = "suffix"
	OpGlob     Op = "glob"
	OpRegex    Op = "regex"
	OpIn       Op = "in"
	OpInCIDR   Op = "in_cidr"
	OpLt       Op = "lt"
	OpLe       Op = "le"
	OpGt       Op = "gt"
	OpGe       Op = "ge"
	OpExists   Op = "exists"
	OpEmpty    Op = "empty"
)

// Action kinds.
const (
	ActRelay      = "relay"
	ActReject     = "reject"
	ActTempfail   = "tempfail"
	ActDiscard    = "discard"
	ActQuarantine = "quarantine"
	ActAddHeader  = "add_header"
	ActTag        = "tag"
	ActAccept     = "accept"
)

// Terminal reports whether an action ends evaluation for the recipient it
// applies to. `add_header` and `tag` accumulate and evaluation continues.
func Terminal(kind string) bool {
	switch kind {
	case ActRelay, ActReject, ActTempfail, ActDiscard, ActQuarantine, ActAccept:
		return true
	}
	return false
}

// Pred is one node of the on-disk predicate tree. Exactly one of All, Any, Not,
// Every or Field must be set — a node with two branch keys is a config error
// rather than a silent precedence guess.
//
// The tree is deliberately not Turing-complete: no loops, no user code, and
// regular expressions are Go's RE2, which has no backtracking and therefore no
// catastrophic-blowup class of failure.
type Pred struct {
	All   []Pred `json:"all,omitempty"`
	Any   []Pred `json:"any,omitempty"`
	Not   *Pred  `json:"not,omitempty"`
	Every *Pred  `json:"every,omitempty"`

	// Always is the explicit constant predicate. `all: []` means the same
	// thing, but an empty list does not survive a marshal/unmarshal round
	// trip, so generated files (convert-routing) use this instead.
	Always *bool `json:"always,omitempty"`

	Field  string `json:"field,omitempty"`
	Op     Op     `json:"op,omitempty"`
	Value  any    `json:"value,omitempty"`
	Values []any  `json:"values,omitempty"`

	// CI overrides the field's default case sensitivity for string comparison.
	CI *bool `json:"ci,omitempty"`
}

// Action is one consequence of a matching rule.
type Action struct {
	Kind string `json:"action"`

	Relay    string `json:"relay,omitempty"`
	Code     int    `json:"code,omitempty"`
	Enhanced string `json:"enhanced,omitempty"`
	Message  string `json:"message,omitempty"`

	// Name and Value carry an add_header; Key and Value carry a tag.
	Name  string `json:"name,omitempty"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

func (a Action) String() string {
	switch a.Kind {
	case ActRelay:
		return "relay " + a.Relay
	case ActReject, ActTempfail:
		return fmt.Sprintf("%s %d %s", a.Kind, a.Code, a.Message)
	case ActAddHeader:
		return fmt.Sprintf("add_header %s: %s", a.Name, a.Value)
	case ActTag:
		return fmt.Sprintf("tag %s=%s", a.Key, a.Value)
	}
	return a.Kind
}

// Rule is one entry of the policy or routes list.
type Rule struct {
	Name     string `json:"name"`
	Priority int    `json:"priority,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`

	// Stage forces evaluation later than the fields alone require. It can
	// never be earlier: a rule cannot see a field that does not exist yet.
	Stage string `json:"stage,omitempty"`

	Match Pred     `json:"match"`
	Then  []Action `json:"then"`
}

// File is the parsed routing.yaml.
type File struct {
	Version int `json:"version"`

	// DefaultAction applies when no route rule matches. It defaults to a
	// 451 tempfail, preserving mailgw/plugins/npRoute.js:65, which answers
	// DENYSOFT "No route found".
	DefaultAction *Action `json:"default_action,omitempty"`

	Policy []Rule `json:"policy,omitempty"`
	Routes []Rule `json:"routes,omitempty"`
}

// LoadFile reads routing.yaml (or routing.json — the parser accepts both,
// since YAML is a superset).
//
// Unknown keys are an error: a misspelled `piority:` that silently became
// priority 0 would reorder the entire table.
func LoadFile(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.UnmarshalStrict(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Version == 0 {
		f.Version = 1
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d (want 1)", path, f.Version)
	}
	return &f, nil
}

// Marshal renders the file as YAML, for `convert-routing`.
func (f *File) Marshal() ([]byte, error) { return yaml.Marshal(f) }

// branches reports which branch keys are set on a node, for validation.
func (p *Pred) branches() []string {
	var set []string
	if p.All != nil {
		set = append(set, "all")
	}
	if p.Any != nil {
		set = append(set, "any")
	}
	if p.Not != nil {
		set = append(set, "not")
	}
	if p.Every != nil {
		set = append(set, "every")
	}
	if p.Always != nil {
		set = append(set, "always")
	}
	if p.Field != "" {
		set = append(set, "field")
	}
	return set
}

// describe renders a predicate compactly for explain output and error
// messages.
func (p *Pred) describe() string {
	switch {
	case p.All != nil:
		return "all(" + describeList(p.All) + ")"
	case p.Any != nil:
		return "any(" + describeList(p.Any) + ")"
	case p.Not != nil:
		return "not(" + p.Not.describe() + ")"
	case p.Every != nil:
		return "every(" + p.Every.describe() + ")"
	case p.Always != nil:
		return fmt.Sprintf("always(%t)", *p.Always)
	case p.Field != "":
		switch p.Op {
		case OpExists, OpEmpty:
			return fmt.Sprintf("%s %s", p.Field, p.Op)
		case OpIn, OpInCIDR:
			return fmt.Sprintf("%s %s %v", p.Field, p.Op, p.Values)
		default:
			return fmt.Sprintf("%s %s %v", p.Field, p.Op, p.Value)
		}
	}
	return "<empty>"
}

func describeList(ps []Pred) string {
	parts := make([]string, 0, len(ps))
	for i := range ps {
		parts = append(parts, ps[i].describe())
	}
	return strings.Join(parts, ", ")
}
