package query

import (
	"encoding/json"
	"reflect"
	"testing"
)

// params decodes a JSON search array the way a real request would, so the tests
// exercise the same any-typed values the handler sees rather than hand-built Go
// values that could never arrive.
func params(t *testing.T, raw string) []Param {
	t.Helper()
	var p []Param
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("bad test fixture %q: %v", raw, err)
	}
	return p
}

// Every operator's exact SQL and placeholder values. Ported from
// logservice/tests/builder.test.ts.
func TestBuildWhere_OperatorsRenderTheExpectedSQL(t *testing.T) {
	allowed := set("sender", "id", "dt")

	cases := []struct {
		name   string
		search string
		sql    string
		values []any
	}{
		{"is", `[{"field":"sender","operator":"is","value":"a@b.com"}]`,
			"`sender` = ?", []any{"a@b.com"}},
		{"equals sign", `[{"field":"sender","operator":"=","value":"a@b.com"}]`,
			"`sender` = ?", []any{"a@b.com"}},
		{"begins", `[{"field":"sender","operator":"begins","value":"foo"}]`,
			"`sender` LIKE ?", []any{"foo%"}},
		{"contains", `[{"field":"sender","operator":"contains","value":"foo"}]`,
			"`sender` LIKE ?", []any{"%foo%"}},
		{"ends", `[{"field":"sender","operator":"ends","value":"foo"}]`,
			"`sender` LIKE ?", []any{"%foo"}},
		{"between", `[{"field":"dt","operator":"between","value":[1,2]}]`,
			"`dt` BETWEEN ? AND ?", []any{int64(1), int64(2)}},
		{"greater than", `[{"field":"id","operator":">","value":5}]`,
			"`id` > ?", []any{int64(5)}},
		{"more", `[{"field":"id","operator":"more","value":5}]`,
			"`id` > ?", []any{int64(5)}},
		{"greater or equal", `[{"field":"id","operator":">=","value":5}]`,
			"`id` >= ?", []any{int64(5)}},
		{"less than", `[{"field":"id","operator":"<","value":5}]`,
			"`id` < ?", []any{int64(5)}},
		{"less", `[{"field":"id","operator":"less","value":5}]`,
			"`id` < ?", []any{int64(5)}},
		{"less or equal", `[{"field":"id","operator":"<=","value":5}]`,
			"`id` <= ?", []any{int64(5)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildWhere(params(t, tc.search), "AND", allowed, "")
			if got.SQL != tc.sql {
				t.Errorf("SQL = %q, want %q", got.SQL, tc.sql)
			}
			if !reflect.DeepEqual(got.Values, tc.values) {
				t.Errorf("values = %#v, want %#v", got.Values, tc.values)
			}
		})
	}
}

func TestBuildWhere_UnknownOperatorEmitsNoClause(t *testing.T) {
	got := BuildWhere(params(t, `[{"field":"sender","operator":"regexp","value":"x"}]`),
		"AND", set("sender"), "")
	if got.SQL != "" {
		t.Errorf("SQL = %q, want empty — an unknown operator must emit nothing", got.SQL)
	}
}

// The silent skip. This is the documented behaviour AND the documented hazard:
// a field nobody allowlisted produces no filter at all, so the query returns
// every row rather than an error.
func TestBuildWhere_UnknownFieldIsSkippedSilently(t *testing.T) {
	got := BuildWhere(params(t, `[{"field":"password","operator":"is","value":"x"}]`),
		"AND", set("sender"), "")
	if got.SQL != "" {
		t.Errorf("SQL = %q, want empty", got.SQL)
	}
	if len(got.Values) != 0 {
		t.Errorf("values = %#v, want none", got.Values)
	}
}

// `if (!item.value && item.value !== 0)`: the JS falsy set, minus zero.
func TestBuildWhere_FalsyValuesAreSkippedExceptZero(t *testing.T) {
	allowed := set("tls", "sender")

	cases := []struct {
		name   string
		search string
		want   string
	}{
		{"empty string", `[{"field":"sender","operator":"is","value":""}]`, ""},
		{"null", `[{"field":"sender","operator":"is","value":null}]`, ""},
		{"false", `[{"field":"tls","operator":"is","value":false}]`, ""},
		{"zero is kept", `[{"field":"tls","operator":"is","value":0}]`, "`tls` = ?"},
		{"true is kept", `[{"field":"tls","operator":"is","value":true}]`, "`tls` = ?"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildWhere(params(t, tc.search), "AND", allowed, "")
			if got.SQL != tc.want {
				t.Errorf("SQL = %q, want %q", got.SQL, tc.want)
			}
		})
	}
}

// A `between` whose value is not a two-element array drops the whole condition
// rather than becoming a half-open range.
func TestBuildWhere_BetweenNeedsATwoElementArray(t *testing.T) {
	for _, raw := range []string{
		`[{"field":"dt","operator":"between","value":5}]`,
		`[{"field":"dt","operator":"between","value":[1]}]`,
		`[{"field":"dt","operator":"between","value":[1,2,3]}]`,
		`[{"field":"dt","operator":"between","value":"1,2"}]`,
	} {
		got := BuildWhere(params(t, raw), "AND", set("dt"), "")
		if got.SQL != "" {
			t.Errorf("%s: SQL = %q, want empty", raw, got.SQL)
		}
	}
}

func TestBuildWhere_JoinsWithAndOrOr(t *testing.T) {
	allowed := set("a", "b")
	search := `[{"field":"a","operator":"is","value":1},{"field":"b","operator":"is","value":2}]`

	if got := BuildWhere(params(t, search), "AND", allowed, ""); got.SQL != "`a` = ? AND `b` = ?" {
		t.Errorf("AND: SQL = %q", got.SQL)
	}
	if got := BuildWhere(params(t, search), "OR", allowed, ""); got.SQL != "`a` = ? OR `b` = ?" {
		t.Errorf("OR: SQL = %q", got.SQL)
	}
}

// The injection payloads from logservice/tests/builder.test.ts:113-159. The
// joiner is re-normalised at SQL assembly, so nothing but a literal "OR" can
// produce an OR and everything else degrades to the more restrictive AND.
func TestBuildWhere_SearchLogicCannotInject(t *testing.T) {
	allowed := set("a", "b")
	search := `[{"field":"a","operator":"is","value":1},{"field":"b","operator":"is","value":2}]`

	for _, logic := range []string{
		"OR 1=1 OR",
		"AND (SELECT 1 FROM Users) AND",
		"AND SLEEP(5) AND",
		"UNION SELECT * FROM Users",
		"; DROP TABLE Users; --",
		"",
		"and",
		"nonsense",
	} {
		got := BuildWhere(params(t, search), logic, allowed, "")
		if got.SQL != "`a` = ? AND `b` = ?" {
			t.Errorf("logic %q produced %q, want the AND form", logic, got.SQL)
		}
	}
}

func TestBuildWhere_SearchLogicIsCaseInsensitiveForOr(t *testing.T) {
	allowed := set("a", "b")
	search := `[{"field":"a","operator":"is","value":1},{"field":"b","operator":"is","value":2}]`
	for _, logic := range []string{"or", "Or", "OR"} {
		if got := BuildWhere(params(t, search), logic, allowed, ""); got.SQL != "`a` = ? OR `b` = ?" {
			t.Errorf("logic %q produced %q, want the OR form", logic, got.SQL)
		}
	}
}

func TestBuildWhere_TablePrefixQualifiesColumns(t *testing.T) {
	got := BuildWhere(params(t, `[{"field":"md5","operator":"is","value":"abc"}]`),
		"AND", set("md5"), "h")
	if got.SQL != "`h`.`md5` = ?" {
		t.Errorf("SQL = %q, want `h`.`md5` = ?", got.SQL)
	}
}

// A number in a LIKE pattern must render the way JavaScript interpolated it —
// "42", never "42.000000".
func TestBuildWhere_NumericLikeValuesRenderWithoutTrailingZeros(t *testing.T) {
	got := BuildWhere(params(t, `[{"field":"a","operator":"contains","value":42}]`),
		"AND", set("a"), "")
	if len(got.Values) != 1 || got.Values[0] != "%42%" {
		t.Errorf("values = %#v, want [%%42%%]", got.Values)
	}
}

func TestBuildOrderBy(t *testing.T) {
	allowed := set("id", "dt")

	cases := []struct {
		name string
		sort string
		want string
	}{
		{"empty falls back", `[]`, "`id` DESC"},
		{"defaults to DESC", `[{"field":"dt"}]`, "`dt` DESC"},
		{"asc is honoured", `[{"field":"dt","direction":"asc"}]`, "`dt` ASC"},
		{"uppercase ASC", `[{"field":"dt","direction":"ASC"}]`, "`dt` ASC"},
		{"junk direction is DESC", `[{"field":"dt","direction":"; DROP TABLE"}]`, "`dt` DESC"},
		{"unknown field falls back", `[{"field":"password"}]`, "`id` DESC"},
		{"multi column", `[{"field":"dt","direction":"asc"},{"field":"id"}]`, "`dt` ASC, `id` DESC"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sorts []Sort
			if err := json.Unmarshal([]byte(tc.sort), &sorts); err != nil {
				t.Fatalf("bad fixture: %v", err)
			}
			if got := BuildOrderBy(sorts, allowed, "`id` DESC", ""); got != tc.want {
				t.Errorf("BuildOrderBy = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildOrderBy_PrefixQualifies(t *testing.T) {
	sorts := []Sort{{Field: "md5", Direction: "asc"}}
	if got := BuildOrderBy(sorts, set("md5"), "`h`.`id` DESC", "h"); got != "`h`.`md5` ASC" {
		t.Errorf("BuildOrderBy = %q", got)
	}
}

// Anything that is not a JSON object yields the defaults — never an error, so a
// malformed q shows the console a first page rather than a broken grid.
func TestParse_NonObjectsDegradeToDefaults(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", `"x"`, "5", "{not json", `[1,2,3]`} {
		q := Parse(raw)
		if len(q.Search) != 0 || q.SearchLogic != "" || len(q.Sort) != 0 {
			t.Errorf("Parse(%q) = %#v, want the zero Query", raw, q)
		}
		if limit, offset, _ := q.LimitOffset(); limit != DefaultLimit || offset != 0 {
			t.Errorf("Parse(%q) limit/offset = %d/%d, want %d/0", raw, limit, offset, DefaultLimit)
		}
	}
}

func TestParse_ReadsAWellFormedQuery(t *testing.T) {
	q := Parse(`{"limit":50,"offset":10,"searchLogic":"OR","search":[{"field":"a","operator":"is","value":"b"}],"sort":[{"field":"id","direction":"asc"}]}`)
	limit, offset, clamped := q.LimitOffset()
	if limit != 50 || offset != 10 || clamped {
		t.Errorf("limit/offset/clamped = %d/%d/%v, want 50/10/false", limit, offset, clamped)
	}
	if q.Logic() != "OR" {
		t.Errorf("Logic = %q, want OR", q.Logic())
	}
	if len(q.Search) != 1 || q.Search[0].Field != "a" {
		t.Errorf("Search = %#v", q.Search)
	}
}

// The one behaviour this port deliberately changes: the Bun service put q.limit
// straight into LIMIT with no ceiling.
func TestLimitOffset_IsClampedRatherThanRejected(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantLimit   int
		wantOffset  int
		wantClamped bool
	}{
		{"absent", `{}`, DefaultLimit, 0, false},
		{"in range", `{"limit":250}`, 250, 0, false},
		{"at the cap", `{"limit":1000}`, MaxLimit, 0, false},
		{"over the cap", `{"limit":100000000}`, MaxLimit, 0, true},
		{"negative", `{"limit":-5}`, DefaultLimit, 0, true},
		{"zero is honoured", `{"limit":0}`, 0, 0, false},
		{"non-numeric", `{"limit":"lots"}`, DefaultLimit, 0, false},
		{"negative offset", `{"offset":-5}`, DefaultLimit, 0, false},
		{"offset kept", `{"offset":40}`, DefaultLimit, 40, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, offset, clamped := Parse(tc.raw).LimitOffset()
			if limit != tc.wantLimit || offset != tc.wantOffset || clamped != tc.wantClamped {
				t.Errorf("limit/offset/clamped = %d/%d/%v, want %d/%d/%v",
					limit, offset, clamped, tc.wantLimit, tc.wantOffset, tc.wantClamped)
			}
		})
	}
}

// Regression guard ported from logservice/tests/searchfields.test.ts.
func TestAllowlists_CarryTheGatewayAndRouteRuleColumns(t *testing.T) {
	for name, fields := range map[string]map[string]struct{}{
		"Delivery":    DeliveryFields,
		"Connection":  ConnectionFields,
		"Transaction": TransactionFields,
	} {
		if _, ok := fields["gateway"]; !ok {
			t.Errorf("%s allowlist is missing `gateway` — the filter would silently return every row", name)
		}
	}

	if _, ok := DeliveryFields["route_rule"]; !ok {
		t.Error("Delivery allowlist is missing `route_rule`")
	}
	// route_rule exists only on Delivery: routing is per recipient, and a
	// Transaction row is one message.
	if _, ok := TransactionFields["route_rule"]; ok {
		t.Error("Transaction allowlist has `route_rule`, which is not a column on that table")
	}
	if _, ok := ConnectionFields["route_rule"]; ok {
		t.Error("Connection allowlist has `route_rule`, which is not a column on that table")
	}
}
