// Package query translates the caller-supplied `?q=<json>` search payload into
// SQL.
//
// It is a deliberate, close port of logservice/src/query/builder.ts, including
// the behaviours that look like bugs and are not:
//
//   - an unrecognised field is SKIPPED SILENTLY, never a 400;
//   - a falsy value is skipped, except the number 0;
//   - a malformed `q` yields the defaults rather than an error.
//
// The console has been built against all three for as long as it has existed;
// "fixing" one turns a grid that shows everything into a grid that shows an
// error. What this port does change is stated where it happens: the limit is
// bounded, which the original never did.
//
// # Where injection is prevented
//
// Identifiers (column names, the sort direction, the AND/OR joiner) are never
// interpolated from input: a column must be present in an allowlist and is then
// written from the allowlist's own key, and the direction and joiner are
// normalised to literals. Values only ever reach SQL as placeholders. The
// connection this runs on has multi-statement support switched OFF (see
// internal/db) so even a hypothetical escape cannot become a second statement.
package query

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Defaults and bounds for pagination.
const (
	// DefaultLimit matches the Bun service.
	DefaultLimit = 100

	// MaxLimit is new. The Bun service put q.limit straight into `LIMIT ?` with
	// no ceiling, so `{"limit":100000000}` was an unauthenticated request to
	// serialise an entire table that grows forever.
	//
	// Over-large values are CLAMPED, not rejected. A 400 here would be a status
	// the console has never had to handle, and 4xx carries a specific meaning to
	// the other caller in this system: mailgw-go's event client treats any 4xx
	// as permanent and spills the event to disk rather than retrying. 4xx is
	// reserved for "this request can never succeed".
	MaxLimit = 1000
)

// Param is one filter from the `search` array.
type Param struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	// Value is a string, a number, a bool, or a two-element array for `between`.
	Value any `json:"value"`
}

// Sort is one entry from the `sort` array.
type Sort struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

// Query is the decoded `q` parameter.
//
// Limit and Offset are `any` rather than int on purpose: the wire type is
// whatever JSON the caller sent, and the Bun service accepted anything. Coerce
// reduces them to sane integers instead of failing the request.
type Query struct {
	Limit       any     `json:"limit"`
	Offset      any     `json:"offset"`
	Search      []Param `json:"search"`
	SearchLogic string  `json:"searchLogic"`
	Sort        []Sort  `json:"sort"`
}

// Parse decodes the `q` query parameter.
//
// Anything that is not a JSON object — absent, empty, malformed, a bare string,
// `null`, an array — yields the zero Query and therefore the defaults. This
// mirrors parseSearchQuery in builder.ts, whose comment explains the reasoning:
// a bare cast asserts a shape the input never has to honour. Never an error:
// the console sends this on every grid page and a 400 would be a blank grid
// where the old one showed rows.
func Parse(raw string) Query {
	var q Query
	if raw == "" {
		return q
	}
	// Confirm it is an object before decoding into the struct, so `[1,2]` or
	// `"x"` degrade to defaults rather than producing a decode error we would
	// have to decide what to do with.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return Query{}
	}
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		// A well-formed object with a wrongly-typed member — `"search": "x"`.
		// The Bun version would iterate that string and skip every character for
		// having no `.value`, i.e. no conditions at all. Same outcome.
		return Query{}
	}
	return q
}

// LimitOffset returns the bounded page window.
//
// A limit that is absent, negative, non-numeric or above MaxLimit is corrected
// rather than refused; clamped reports whether that happened so the caller can
// log it at Debug. An explicit 0 is honoured — `LIMIT 0` is how a caller asks
// for the count alone, and the search endpoints return `total` independently of
// the page.
func (q Query) LimitOffset() (limit, offset int, clamped bool) {
	limit, ok := coerceInt(q.Limit)
	switch {
	case !ok:
		limit = DefaultLimit
	case limit < 0:
		limit, clamped = DefaultLimit, true
	case limit > MaxLimit:
		limit, clamped = MaxLimit, true
	}

	offset, ok = coerceInt(q.Offset)
	if !ok || offset < 0 {
		offset = 0
	}
	return limit, offset, clamped
}

// coerceInt accepts the JSON shapes a number can arrive as. A float with a
// fractional part is truncated rather than rejected, which is what putting it
// into a MySQL LIMIT would have done anyway.
func coerceInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// Where is a rendered WHERE body and its placeholder values.
type Where struct {
	SQL    string
	Values []any
}

// BuildWhere renders the filter list.
//
// prefix, when non-empty, qualifies every column with a table alias — needed by
// the HashLookups search, which JOINs Transaction and would otherwise have two
// candidate `action` and `createdAt` columns.
//
// Returns an empty SQL string when nothing survived, which the caller renders as
// no WHERE clause at all.
func BuildWhere(params []Param, logic string, allowed map[string]struct{}, prefix string) Where {
	var (
		conds  []string
		values []any
	)

	for _, p := range params {
		if skipValue(p.Value) {
			continue
		}
		if _, ok := allowed[p.Field]; !ok {
			// Silently. See the note on the allowlists in fields.go: this is the
			// documented behaviour and also the documented hazard.
			continue
		}

		col := quoteCol(p.Field, prefix)

		switch p.Operator {
		case "is", "=":
			if v, ok := scalar(p.Value); ok {
				conds = append(conds, col+" = ?")
				values = append(values, v)
			}
		case "begins":
			if s, ok := likeText(p.Value); ok {
				conds = append(conds, col+" LIKE ?")
				values = append(values, s+"%")
			}
		case "contains":
			if s, ok := likeText(p.Value); ok {
				conds = append(conds, col+" LIKE ?")
				values = append(values, "%"+s+"%")
			}
		case "ends":
			if s, ok := likeText(p.Value); ok {
				conds = append(conds, col+" LIKE ?")
				values = append(values, "%"+s)
			}
		case "between":
			// Only a two-element array qualifies. Anything else drops the whole
			// condition rather than becoming a half-open range.
			pair, ok := p.Value.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			lo, okLo := scalar(pair[0])
			hi, okHi := scalar(pair[1])
			if !okLo || !okHi {
				continue
			}
			conds = append(conds, col+" BETWEEN ? AND ?")
			values = append(values, lo, hi)
		case ">", "more":
			if v, ok := scalar(p.Value); ok {
				conds = append(conds, col+" > ?")
				values = append(values, v)
			}
		case ">=":
			if v, ok := scalar(p.Value); ok {
				conds = append(conds, col+" >= ?")
				values = append(values, v)
			}
		case "<", "less":
			if v, ok := scalar(p.Value); ok {
				conds = append(conds, col+" < ?")
				values = append(values, v)
			}
		case "<=":
			if v, ok := scalar(p.Value); ok {
				conds = append(conds, col+" <= ?")
				values = append(values, v)
			}
		default:
			// Unknown operator emits no clause, exactly as the switch in
			// builder.ts falls through.
		}
	}

	if len(conds) == 0 {
		return Where{}
	}

	// Re-normalise the joiner HERE, at assembly, not only where q was parsed.
	//
	// This is the second of two layers and builder.ts:95-101 says not to
	// simplify it away: `logic` originates in caller-supplied JSON, a type
	// annotation is erased at runtime in TypeScript, and interpolating it would
	// be an injection. Go's type system does make the TypeScript-specific half
	// of that argument moot — the value really is a string here — but the
	// defence is kept because the property it guarantees is the one that
	// matters: nothing but the literal "OR" can ever produce an OR, and every
	// other input degrades to the more restrictive AND.
	// logservice/tests/builder.test.ts pins this with injection payloads, and
	// those cases are ported in builder_test.go.
	op := "AND"
	if strings.ToUpper(logic) == "OR" {
		op = "OR"
	}

	return Where{SQL: strings.Join(conds, " "+op+" "), Values: values}
}

// BuildOrderBy renders the ORDER BY body.
//
// Every field is checked against the same allowlist and dropped silently
// otherwise, and the direction is normalised to the literal ASC or DESC —
// DESC by default and for anything unrecognised, matching builder.ts. Returns
// fallback when nothing valid remains, so the query always has a deterministic
// order and pagination cannot repeat or skip rows.
func BuildOrderBy(sorts []Sort, allowed map[string]struct{}, fallback, prefix string) string {
	var parts []string
	for _, s := range sorts {
		if _, ok := allowed[s.Field]; !ok {
			continue
		}
		dir := "DESC"
		if strings.ToLower(s.Direction) == "asc" {
			dir = "ASC"
		}
		parts = append(parts, quoteCol(s.Field, prefix)+" "+dir)
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, ", ")
}

// quoteCol backtick-quotes an allowlisted column, optionally alias-qualified.
//
// The name has already been matched against an allowlist, so it cannot contain a
// backtick; the doubling is belt-and-braces for the day somebody adds a field to
// an allowlist by pasting it from somewhere.
func quoteCol(field, prefix string) string {
	q := "`" + strings.ReplaceAll(field, "`", "``") + "`"
	if prefix == "" {
		return q
	}
	return "`" + strings.ReplaceAll(prefix, "`", "``") + "`." + q
}

// skipValue reproduces `if (!item.value && item.value !== 0) continue`.
//
// JavaScript's falsy set, minus the number zero: an empty string, null, false
// and absent are all dropped, but 0 is a real filter value — `tls = 0` is how
// the console asks for unencrypted deliveries, and dropping it would silently
// widen that filter to every row.
func skipValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return false // including 0, deliberately
	default:
		return false
	}
}

// scalar converts a JSON value into something database/sql will accept.
//
// A float that is exactly integral becomes an int64, so `id = 42` compares as an
// integer rather than as 42.0 — the comparison succeeds either way in MySQL, but
// the integer form is what shows up in a slow-query log and what an index sees
// without a conversion.
//
// A non-scalar (an object, or an array outside `between`) reports false and its
// condition is dropped. The Bun version would have handed the array to the
// driver; database/sql refuses it at exec time, which would turn one malformed
// filter into a 500 for the whole request.
func scalar(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return t, true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, false
		}
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t), true
		}
		return t, true
	default:
		return nil, false
	}
}

// likeText renders a value for a LIKE pattern the way JavaScript's template
// interpolation would: 42 becomes "42", not "42.000000".
//
// Wildcards inside the caller's value are deliberately NOT escaped, matching the
// original. `contains` is a substring search and the grids rely on nothing more;
// escaping would change what every existing saved filter matches.
func likeText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return "", false
		}
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// Logic returns the joiner to hand BuildWhere, defaulting to AND.
func (q Query) Logic() string {
	if strings.ToUpper(q.SearchLogic) == "OR" {
		return "OR"
	}
	return "AND"
}

// quoteIdent quotes a table name. Every caller passes a compile-time constant;
// it exists so nobody is tempted to interpolate one instead.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
