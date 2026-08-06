// Package rows turns a `SELECT *` result set into JSON-ready values with the
// same types the Bun service produced.
//
// # Why this package exists
//
// The search endpoints return whatever columns the table has, so there is no
// struct to scan into. The obvious Go approach — scan every column into
// []byte — makes every value a JSON STRING, so `"id": 42` becomes `"id": "42"`
// and `"delay": 1.23` becomes `"delay": "1.23"`. That is a wire change for every
// consumer of the search API, and a silent one: the console's grids render a
// stringified number identically.
//
// So each column is scanned according to its declared MySQL type, read from
// sql.Rows.ColumnTypes(). The four result sets this service returns span only
// INT, DOUBLE, VARCHAR, TEXT and DATETIME; the rest of the mapping is
// future-proofing for a column somebody adds later.
//
// # DATETIME stays a string, and that is a decision
//
// The DSN deliberately does not set parseTime, so a DATETIME arrives as the raw
// "2006-01-02 15:04:05" MySQL renders. Bun produced a JavaScript Date, which
// JSON.stringify writes as "2006-01-02T15:04:05.000Z". Both render identically
// in the console, whose formatter is
//
//	String(p.value).slice(0, 19).replace("T", " ")
//
// (webui-fastify/public/js/grids/aggrid-common.js), and nothing else reads a
// date out of this API.
//
// The alternative — parseTime=true and a time.Time marshalled as RFC 3339 —
// would attach a TIMEZONE that the stored value does not have. A MariaDB
// DATETIME carries none, so Go would stamp it with the connection's location and
// the API would start reporting a different instant from the one an operator
// sees running the same SELECT in `mysql`. For an audit trail, the two agreeing
// is worth more than matching Bun's punctuation.
//
// This is the ONLY intended difference between the two services' JSON. Anything
// else that differs is a bug — see the plan's verification section.
//
// # Exported, not internal, because logservice-fiber shares it
//
// M23 put a second implementation beside this one. The two differ in their HTTP
// layer and in nothing else, which is the only reason comparing them means
// anything — so everything below HTTP lives here and is imported by both.
// internal/api is what stays internal. Do not add an HTTP type to this package.
//
// This package in particular is shared rather than copied: its whole job is
// keeping the JSON types identical, so two copies of it would be two chances to
// disagree about the one thing it exists to pin.
package rows

import (
	"database/sql"
	"fmt"
)

// Row is one result row. A map, so the JSON object carries whatever columns the
// SELECT returned.
//
// Go marshals map keys in sorted order, so the JSON key order is alphabetical
// rather than the table's column order. Nothing depends on it: AG Grid, the
// w2ui viewers and the dashboard all index by field name, and
// tests/api/logservice.e2e.test.ts reads records[0].<field>. Preserving column
// order would mean an ordered type and a custom MarshalJSON for no reader.
type Row map[string]any

// Scan reads every row of rs, closing it.
//
// The returned slice is never nil: the search endpoints put it straight into
// `records`, and a nil slice marshals to `null` where the console expects `[]`.
// The Bun service always returned an array here, and aggrid-common.js calls
// .length on it without a guard.
func Scan(rs *sql.Rows) ([]Row, error) {
	defer func() { _ = rs.Close() }()

	cols, err := rs.Columns()
	if err != nil {
		return nil, fmt.Errorf("read result columns: %w", err)
	}
	types, err := rs.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("read result column types: %w", err)
	}

	out := make([]Row, 0, 16)
	for rs.Next() {
		holders := make([]any, len(cols))
		for i, ct := range types {
			holders[i] = holderFor(ct.DatabaseTypeName())
		}
		if err := rs.Scan(holders...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		row := make(Row, len(cols))
		for i, name := range cols {
			row[name] = value(holders[i])
		}
		out = append(out, row)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return out, nil
}

// holderFor picks a scan destination for a declared MySQL type.
//
// Every holder is a Null* type: these tables are almost entirely nullable — only
// id, createdAt and updatedAt are NOT NULL — and a NULL must reach JSON as null,
// not as a zero that reads like a real measurement. A `delay` of 0 and a `delay`
// that was never recorded are different facts.
//
// The default is NullString rather than a panic or an error: a column type this
// build has not seen is then a display quirk in one grid, not a 500 for the
// whole endpoint. logservice does not own every table it can be pointed at.
func holderFor(dbType string) any {
	switch dbType {
	// INT covers Delivery.port, every rcpt_count_*, tls/auth/tls_forced,
	// data_bytes and mime_part_count. TINYINT is how migration 022's
	// allow_insecure_auth and 025's use_mx are declared.
	case "INT", "INTEGER", "BIGINT", "SMALLINT", "MEDIUMINT", "TINYINT", "YEAR":
		return new(sql.NullInt64)

	// DOUBLE is Delivery.delay and Transaction.delay_data_post. DECIMAL is here
	// for completeness; scanning it as a float loses exactness, but no column in
	// this schema is DECIMAL and a future money column would want its own
	// handling anyway.
	case "DOUBLE", "FLOAT", "DECIMAL", "NEWDECIMAL":
		return new(sql.NullFloat64)

	// DATETIME/TIMESTAMP/DATE arrive as their raw MySQL text because the DSN
	// does not set parseTime. See the package comment for why that is the
	// choice and not an oversight.
	default:
		return new(sql.NullString)
	}
}

// value unwraps a holder into the value that gets marshalled, turning an invalid
// (SQL NULL) holder into a Go nil so encoding/json writes `null`.
func value(holder any) any {
	switch h := holder.(type) {
	case *sql.NullInt64:
		if !h.Valid {
			return nil
		}
		return h.Int64
	case *sql.NullFloat64:
		if !h.Valid {
			return nil
		}
		return h.Float64
	case *sql.NullString:
		if !h.Valid {
			return nil
		}
		return h.String
	default:
		// Unreachable: holderFor returns one of the three above. Returning nil
		// rather than panicking keeps a future editing mistake out of the
		// request path.
		return nil
	}
}
