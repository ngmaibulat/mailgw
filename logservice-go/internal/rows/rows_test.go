package rows

import (
	"database/sql"
	"encoding/json"
	"testing"
)

// The type mapping is the whole point of this package: a naive []byte scan makes
// every value a JSON string, which is a silent wire change the console renders
// identically.
func TestHolderFor_MapsEveryTypeInTheseTables(t *testing.T) {
	cases := []struct {
		dbType string
		want   string
	}{
		// Delivery.port, every rcpt_count_*, tls/auth/tls_forced, data_bytes,
		// mime_part_count, and every id.
		{"INT", "*sql.NullInt64"},
		{"INTEGER", "*sql.NullInt64"},
		{"BIGINT", "*sql.NullInt64"},
		{"SMALLINT", "*sql.NullInt64"},
		{"MEDIUMINT", "*sql.NullInt64"},
		{"TINYINT", "*sql.NullInt64"},

		// Delivery.delay, Transaction.delay_data_post.
		{"DOUBLE", "*sql.NullFloat64"},
		{"FLOAT", "*sql.NullFloat64"},
		{"DECIMAL", "*sql.NullFloat64"},

		// Text, and DATETIME — which stays a string deliberately; see the
		// package comment.
		{"VARCHAR", "*sql.NullString"},
		{"TEXT", "*sql.NullString"},
		{"LONGTEXT", "*sql.NullString"},
		{"CHAR", "*sql.NullString"},
		{"DATETIME", "*sql.NullString"},
		{"TIMESTAMP", "*sql.NullString"},

		// A type this build has not seen must degrade to a string rather than
		// panic: logservice does not own every table it can be pointed at.
		{"GEOMETRY", "*sql.NullString"},
		{"", "*sql.NullString"},
	}

	for _, tc := range cases {
		t.Run(tc.dbType, func(t *testing.T) {
			got := typeName(holderFor(tc.dbType))
			if got != tc.want {
				t.Errorf("holderFor(%q) = %s, want %s", tc.dbType, got, tc.want)
			}
		})
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *sql.NullInt64:
		return "*sql.NullInt64"
	case *sql.NullFloat64:
		return "*sql.NullFloat64"
	case *sql.NullString:
		return "*sql.NullString"
	default:
		return "unknown"
	}
}

// An integer column must marshal as a JSON number, not a string. This is the
// exact regression the package exists to prevent.
func TestValue_MarshalsWithTheRightJSONType(t *testing.T) {
	cases := []struct {
		name   string
		holder any
		want   string
	}{
		{"int", &sql.NullInt64{Int64: 42, Valid: true}, `42`},
		{"int zero", &sql.NullInt64{Int64: 0, Valid: true}, `0`},
		{"float", &sql.NullFloat64{Float64: 1.23, Valid: true}, `1.23`},
		{"float zero", &sql.NullFloat64{Float64: 0, Valid: true}, `0`},
		{"string", &sql.NullString{String: "250 OK", Valid: true}, `"250 OK"`},
		{"empty string", &sql.NullString{String: "", Valid: true}, `""`},
		// A DATETIME arrives as raw MySQL text and stays that way.
		{"datetime", &sql.NullString{String: "2026-08-03 18:34:37", Valid: true},
			`"2026-08-03 18:34:37"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := json.Marshal(value(tc.holder))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(out) != tc.want {
				t.Errorf("value() marshalled as %s, want %s", out, tc.want)
			}
		})
	}
}

// SQL NULL must reach JSON as null, never as a zero. A `delay` of 0 and a
// `delay` that was never recorded are different facts, and every column in these
// tables except id/createdAt/updatedAt is nullable.
func TestValue_NullBecomesJSONNull(t *testing.T) {
	for _, holder := range []any{
		&sql.NullInt64{Valid: false},
		&sql.NullFloat64{Valid: false},
		&sql.NullString{Valid: false},
	} {
		if v := value(holder); v != nil {
			t.Errorf("%T with Valid=false produced %#v, want nil", holder, v)
		}
		out, _ := json.Marshal(value(holder))
		if string(out) != "null" {
			t.Errorf("%T with Valid=false marshalled as %s, want null", holder, out)
		}
	}
}

// A whole row round-trips to the object shape the console's grids index into.
func TestRow_MarshalsAsAnObjectOfMixedTypes(t *testing.T) {
	r := Row{
		"id":            value(&sql.NullInt64{Int64: 7, Valid: true}),
		"uuid":          value(&sql.NullString{String: "X.1.1", Valid: true}),
		"delay":         value(&sql.NullFloat64{Float64: 0.123, Valid: true}),
		"dt":            value(&sql.NullString{String: "2026-08-03 18:34:37", Valid: true}),
		"route_rule":    value(&sql.NullString{Valid: false}),
		"rcpt_count_ok": value(&sql.NullInt64{Int64: 0, Valid: true}),
	}

	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"delay":0.123,"dt":"2026-08-03 18:34:37","id":7,"rcpt_count_ok":0,"route_rule":null,"uuid":"X.1.1"}`
	if string(out) != want {
		t.Errorf("row marshalled as\n  %s\nwant\n  %s", out, want)
	}
}
