package ruleset

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// matcher is a compiled predicate.
type matcher interface {
	Match(*Env) bool
}

// node pairs a compiled predicate with the stage at which it becomes
// evaluable — the maximum stage of every field it mentions — and with whether
// it reads anything about the current recipient.
type node struct {
	matcher
	stage Stage
	// usesRcpt marks a predicate that reads rcpt.* fields, and therefore has
	// to be evaluated once per recipient rather than once per message. This is
	// the structural fix for npRoute.js:55, which evaluated the whole message
	// against rcpt_to[0].
	usesRcpt bool
}

// merge folds a child node's requirements into a parent's.
func (n *node) merge(c node) {
	if c.stage > n.stage {
		n.stage = c.stage
	}
	n.usesRcpt = n.usesRcpt || c.usesRcpt
}

type matchFunc func(*Env) bool

func (f matchFunc) Match(e *Env) bool { return f(e) }

// constNode is the compiled form of `all: []` (always true) and `any: []`
// (always false).
type constNode bool

func (c constNode) Match(*Env) bool { return bool(c) }

type allNode []matcher

func (n allNode) Match(e *Env) bool {
	for _, m := range n {
		if !m.Match(e) {
			return false
		}
	}
	return true
}

type anyNode []matcher

func (n anyNode) Match(e *Env) bool {
	for _, m := range n {
		if m.Match(e) {
			return true
		}
	}
	return false
}

type notNode struct{ inner matcher }

func (n notNode) Match(e *Env) bool { return !n.inner.Match(e) }

// leaf is a compiled field/op/value comparison.
//
// Over a list-valued field a leaf is existential — "some attachment is a .exe".
// `every` sets universal, giving "every attachment is". The existential default
// is what makes `not: {field: attachment.filename, op: glob, value: "*.exe"}`
// read as "no attachment is a .exe", which is what an operator writing a
// blocklist means.
type leaf struct {
	field     string
	op        Op
	universal bool
	fn        func(Value) bool
}

func (l *leaf) Match(e *Env) bool {
	v := e.Lookup(l.field)
	if !v.OK {
		// An absent field is empty and matches nothing else. This is why the
		// schema registry matters: a typo'd field name would otherwise be a
		// rule that quietly never fires.
		return l.op == OpEmpty
	}
	if v.List {
		if len(v.Elems) == 0 {
			return l.op == OpEmpty || l.universal
		}
		if l.universal {
			for i := range v.Elems {
				if !l.test(v.Elems[i]) {
					return false
				}
			}
			return true
		}
		for i := range v.Elems {
			if l.test(v.Elems[i]) {
				return true
			}
		}
		return false
	}
	return l.test(v)
}

func (l *leaf) test(v Value) bool {
	switch l.op {
	case OpExists:
		return v.nonEmpty()
	case OpEmpty:
		return !v.nonEmpty()
	}
	return l.fn(v)
}

// compilePred turns one AST node into a matcher, reporting the stage at which
// it can first be evaluated.
func compilePred(p *Pred, sc *Schema, path string) (node, error) {
	set := p.branches()
	if len(set) == 0 {
		return node{}, fmt.Errorf("%s: empty predicate; use `all: []` for 'always true'", path)
	}
	if len(set) > 1 {
		return node{}, fmt.Errorf("%s: predicate has several branch keys (%s); exactly one of all/any/not/every/field is allowed",
			path, strings.Join(set, ", "))
	}

	switch {
	case p.All != nil:
		return compileList(p.All, sc, path, "all")
	case p.Any != nil:
		return compileList(p.Any, sc, path, "any")
	case p.Not != nil:
		inner, err := compilePred(p.Not, sc, path+".not")
		if err != nil {
			return node{}, err
		}
		return node{matcher: notNode{inner: inner.matcher}, stage: inner.stage, usesRcpt: inner.usesRcpt}, nil
	case p.Always != nil:
		return node{matcher: constNode(*p.Always), stage: StageConnect}, nil
	case p.Every != nil:
		if len(p.Every.branches()) != 1 || p.Every.Field == "" {
			return node{}, fmt.Errorf("%s.every: must wrap a single field predicate, not a combinator", path)
		}
		return compileLeaf(p.Every, sc, path+".every", true)
	default:
		return compileLeaf(p, sc, path, false)
	}
}

func compileList(ps []Pred, sc *Schema, path, kind string) (node, error) {
	if len(ps) == 0 {
		// `all: []` is the idiomatic catch-all rule; `any: []` never matches.
		return node{matcher: constNode(kind == "all"), stage: StageConnect}, nil
	}
	ms := make([]matcher, 0, len(ps))
	out := node{stage: StageConnect}
	for i := range ps {
		n, err := compilePred(&ps[i], sc, fmt.Sprintf("%s.%s[%d]", path, kind, i))
		if err != nil {
			return node{}, err
		}
		ms = append(ms, n.matcher)
		out.merge(n)
	}
	if kind == "all" {
		out.matcher = allNode(ms)
	} else {
		out.matcher = anyNode(ms)
	}
	return out, nil
}

func compileLeaf(p *Pred, sc *Schema, path string, universal bool) (node, error) {
	def, ok := sc.Lookup(p.Field)
	if !ok {
		msg := fmt.Sprintf("%s: unknown field %q", path, p.Field)
		if s := sc.suggest(p.Field); len(s) > 0 {
			msg += fmt.Sprintf(" (did you mean %s?)", strings.Join(s, ", "))
		}
		return node{}, fmt.Errorf("%s", msg)
	}
	if p.Op == "" {
		return node{}, fmt.Errorf("%s: field %q has no operator", path, p.Field)
	}
	if universal && !def.List {
		return node{}, fmt.Errorf("%s: `every` needs a multi-valued field; %q is single-valued", path, p.Field)
	}

	ci := def.CI
	if p.CI != nil {
		ci = *p.CI
	}

	l := &leaf{field: p.Field, op: p.Op, universal: universal}

	switch p.Op {
	case OpExists, OpEmpty:
		if p.Value != nil || p.Values != nil {
			return node{}, fmt.Errorf("%s: operator %q takes no value", path, p.Op)
		}
		return node{matcher: l, stage: def.Stage, usesRcpt: def.Stage == StageRcpt}, nil

	case OpIn, OpInCIDR:
		if len(p.Values) == 0 {
			return node{}, fmt.Errorf("%s: operator %q needs a non-empty `values` list", path, p.Op)
		}
		if p.Value != nil {
			return node{}, fmt.Errorf("%s: operator %q uses `values`, not `value`", path, p.Op)
		}
	default:
		if p.Value == nil {
			return node{}, fmt.Errorf("%s: operator %q needs a `value`", path, p.Op)
		}
		if p.Values != nil {
			return node{}, fmt.Errorf("%s: operator %q uses `value`, not `values`", path, p.Op)
		}
	}

	fn, err := compileOp(p, def, ci, path)
	if err != nil {
		return node{}, err
	}
	l.fn = fn
	return node{matcher: l, stage: def.Stage, usesRcpt: def.Stage == StageRcpt}, nil
}

// compileOp builds the scalar comparator. Every type mismatch is caught here,
// at load time, rather than producing a rule that never fires.
func compileOp(p *Pred, def FieldDef, ci bool, path string) (func(Value) bool, error) {
	switch p.Op {
	case OpEq, OpNe:
		eq, err := compileEq(p.Value, def, ci, path)
		if err != nil {
			return nil, err
		}
		if p.Op == OpNe {
			return func(v Value) bool { return !eq(v) }, nil
		}
		return eq, nil

	case OpContains, OpPrefix, OpSuffix:
		if def.Kind != KindString {
			return nil, fmt.Errorf("%s: operator %q needs a string field; %q is %s", path, p.Op, def.Name, def.Kind)
		}
		lit, err := toString(p.Value, path)
		if err != nil {
			return nil, err
		}
		if ci {
			lit = strings.ToLower(lit)
		}
		return func(v Value) bool {
			s := v.S
			if ci {
				s = strings.ToLower(s)
			}
			switch p.Op {
			case OpContains:
				return strings.Contains(s, lit)
			case OpPrefix:
				return strings.HasPrefix(s, lit)
			default:
				return strings.HasSuffix(s, lit)
			}
		}, nil

	case OpGlob:
		if def.Kind != KindString {
			return nil, fmt.Errorf("%s: `glob` needs a string field; %q is %s", path, def.Name, def.Kind)
		}
		lit, err := toString(p.Value, path)
		if err != nil {
			return nil, err
		}
		g, err := compileGlob(lit, def.DotGlob, ci)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return func(v Value) bool { return g.match(v.S) }, nil

	case OpRegex:
		if def.Kind != KindString {
			return nil, fmt.Errorf("%s: `regex` needs a string field; %q is %s", path, def.Name, def.Kind)
		}
		lit, err := toString(p.Value, path)
		if err != nil {
			return nil, err
		}
		if ci {
			lit = "(?i)" + lit
		}
		// Go's regexp is RE2: linear time, no backtracking, so a rule cannot
		// be written that hangs the gateway on a crafted address.
		re, err := regexp.Compile(lit)
		if err != nil {
			return nil, fmt.Errorf("%s: bad regex: %w", path, err)
		}
		return func(v Value) bool { return re.MatchString(v.S) }, nil

	case OpIn:
		return compileIn(p.Values, def, ci, path)

	case OpInCIDR:
		if def.Kind != KindIP {
			return nil, fmt.Errorf("%s: `in_cidr` needs an ip field; %q is %s", path, def.Name, def.Kind)
		}
		nets := make([]netip.Prefix, 0, len(p.Values))
		for _, raw := range p.Values {
			s, err := toString(raw, path)
			if err != nil {
				return nil, err
			}
			pfx, err := netip.ParsePrefix(strings.TrimSpace(s))
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a CIDR prefix: %w", path, s, err)
			}
			nets = append(nets, pfx.Masked())
		}
		return func(v Value) bool {
			addr := v.IP.Unmap()
			for _, n := range nets {
				if n.Contains(addr) {
					return true
				}
			}
			return false
		}, nil

	case OpLt, OpLe, OpGt, OpGe:
		if def.Kind != KindInt {
			return nil, fmt.Errorf("%s: operator %q needs a numeric field; %q is %s", path, p.Op, def.Name, def.Kind)
		}
		lit, err := toInt(p.Value, path)
		if err != nil {
			return nil, err
		}
		return func(v Value) bool {
			switch p.Op {
			case OpLt:
				return v.I < lit
			case OpLe:
				return v.I <= lit
			case OpGt:
				return v.I > lit
			default:
				return v.I >= lit
			}
		}, nil
	}

	return nil, fmt.Errorf("%s: unknown operator %q", path, p.Op)
}

func compileEq(raw any, def FieldDef, ci bool, path string) (func(Value) bool, error) {
	switch def.Kind {
	case KindString:
		lit, err := toString(raw, path)
		if err != nil {
			return nil, err
		}
		if ci {
			return func(v Value) bool { return strings.EqualFold(v.S, lit) }, nil
		}
		return func(v Value) bool { return v.S == lit }, nil
	case KindInt:
		lit, err := toInt(raw, path)
		if err != nil {
			return nil, err
		}
		return func(v Value) bool { return v.I == lit }, nil
	case KindBool:
		lit, err := toBool(raw, path)
		if err != nil {
			return nil, err
		}
		return func(v Value) bool { return v.B == lit }, nil
	case KindIP:
		lit, err := toIP(raw, path)
		if err != nil {
			return nil, err
		}
		return func(v Value) bool { return v.IP.Unmap() == lit }, nil
	}
	return nil, fmt.Errorf("%s: field %q has an unsupported kind", path, def.Name)
}

func compileIn(raws []any, def FieldDef, ci bool, path string) (func(Value) bool, error) {
	switch def.Kind {
	case KindString:
		set := make(map[string]struct{}, len(raws))
		for _, raw := range raws {
			s, err := toString(raw, path)
			if err != nil {
				return nil, err
			}
			if ci {
				s = strings.ToLower(s)
			}
			set[s] = struct{}{}
		}
		return func(v Value) bool {
			s := v.S
			if ci {
				s = strings.ToLower(s)
			}
			_, ok := set[s]
			return ok
		}, nil

	case KindInt:
		set := make(map[int64]struct{}, len(raws))
		for _, raw := range raws {
			i, err := toInt(raw, path)
			if err != nil {
				return nil, err
			}
			set[i] = struct{}{}
		}
		return func(v Value) bool { _, ok := set[v.I]; return ok }, nil

	case KindIP:
		set := make(map[netip.Addr]struct{}, len(raws))
		for _, raw := range raws {
			a, err := toIP(raw, path)
			if err != nil {
				return nil, err
			}
			set[a] = struct{}{}
		}
		return func(v Value) bool { _, ok := set[v.IP.Unmap()]; return ok }, nil
	}
	return nil, fmt.Errorf("%s: `in` does not apply to a %s field", path, def.Kind)
}

// ── literal coercion ────────────────────────────────────────────────────────
//
// YAML and JSON both arrive as float64/string/bool, so a number written
// unquoted still needs converting.

func toString(raw any, path string) (string, error) {
	switch v := raw.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), nil
		}
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	}
	return "", fmt.Errorf("%s: expected a string value, got %T", path, raw)
}

func toInt(raw any, path string) (int64, error) {
	switch v := raw.(type) {
	case float64:
		if v != float64(int64(v)) {
			return 0, fmt.Errorf("%s: %v is not a whole number", path, v)
		}
		return int64(v), nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %q is not a number", path, v)
		}
		return n, nil
	}
	return 0, fmt.Errorf("%s: expected a number, got %T", path, raw)
}

func toBool(raw any, path string) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return false, fmt.Errorf("%s: %q is not true or false", path, v)
		}
		return b, nil
	}
	return false, fmt.Errorf("%s: expected true or false, got %T", path, raw)
}

func toIP(raw any, path string) (netip.Addr, error) {
	s, err := toString(raw, path)
	if err != nil {
		return netip.Addr{}, err
	}
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%s: %q is not an IP address: %w", path, s, err)
	}
	return a.Unmap(), nil
}
