package query

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/ngmaibulat/mailgw/logservice-go/rows"
)

// Result is the response envelope every search endpoint returns.
//
// The field names and the literal "success" are a contract with the console:
// AG Grid reads `total` as the exact last-row index
// (webui-fastify/public/js/grids/aggrid-common.js) and the webui proxy passes
// the whole body through verbatim.
type Result struct {
	Status  string     `json:"status"`
	Total   int64      `json:"total"`
	Records []rows.Row `json:"records"`
}

// Table names, quoted once here so no call site interpolates one.
const (
	TableConnection  = "Connection"
	TableDelivery    = "Delivery"
	TableTransaction = "Transaction"
	TableHashLookups = "HashLookups"
)

// Searcher runs the searches against a database.
type Searcher struct {
	DB *sql.DB
}

// SearchTable is the shape shared by the Connection, Delivery and Transaction
// searches: one COUNT over the filter, then one page of `SELECT *`.
//
// The count deliberately ignores limit/offset — it is the real number of
// matching rows, which is what lets the grid size its scrollbar without paging
// to the end. The same placeholder values are reused for both statements.
func (s Searcher) SearchTable(ctx context.Context, table string, allowed map[string]struct{}, q Query) (Result, error) {
	where := BuildWhere(q.Search, q.Logic(), allowed, "")
	whereSQL := ""
	if where.SQL != "" {
		whereSQL = " WHERE " + where.SQL
	}

	tbl := quoteIdent(table)

	var total int64
	countSQL := "SELECT COUNT(*) FROM " + tbl + whereSQL
	if err := s.DB.QueryRowContext(ctx, countSQL, where.Values...).Scan(&total); err != nil {
		return Result{}, fmt.Errorf("count %s: %w", table, err)
	}

	limit, offset, _ := q.LimitOffset()
	orderBy := BuildOrderBy(q.Sort, allowed, "`id` DESC", "")

	//nolint:gosec // every fragment is either a constant or allowlist-derived;
	// values only ever travel as placeholders. See the package comment.
	pageSQL := "SELECT * FROM " + tbl + whereSQL + " ORDER BY " + orderBy + " LIMIT ? OFFSET ?"
	args := append(append([]any{}, where.Values...), limit, offset)

	rs, err := s.DB.QueryContext(ctx, pageSQL, args...)
	if err != nil {
		return Result{}, fmt.Errorf("search %s: %w", table, err)
	}
	records, err := rows.Scan(rs)
	if err != nil {
		return Result{}, fmt.Errorf("search %s: %w", table, err)
	}

	return Result{Status: "success", Total: total, Records: records}, nil
}

// hashLookupFrom is the JOIN both the count and the page share.
//
// LEFT, not INNER: a HashLookups row whose transaction never produced a
// Transaction row — an attachment blocked before the message was queued — must
// still appear in the viewer. An INNER join would hide exactly the lookups an
// operator is most likely to be searching for.
const hashLookupFrom = " FROM `HashLookups` `h` LEFT JOIN `Transaction` `t` ON `t`.`uuid` = `h`.`txn_uuid`"

// hashLookupSelect lists h.* plus the Transaction columns the viewer displays
// beside each attachment.
//
// `t`.`action` is aliased to txn_action because `h`.`action` — allow or block
// for this attachment — already owns the name `action`, and that is the one the
// grid's action column reads.
const hashLookupSelect = "SELECT `h`.*, " +
	"`t`.`dt` AS `dt`, " +
	"`t`.`sender` AS `sender`, " +
	"`t`.`rcpt_list` AS `rcpt_list`, " +
	"`t`.`encoding` AS `encoding`, " +
	"`t`.`action` AS `txn_action`, " +
	"`t`.`rcpt_count_accept` AS `rcpt_count_accept`, " +
	"`t`.`rcpt_count_tempfail` AS `rcpt_count_tempfail`, " +
	"`t`.`rcpt_count_reject` AS `rcpt_count_reject`, " +
	"`t`.`delay_data_post` AS `delay_data_post`, " +
	"`t`.`data_bytes` AS `data_bytes`, " +
	"`t`.`mime_part_count` AS `mime_part_count`"

// SearchHashLookups is the one search with a JOIN.
//
// Filtering and sorting are restricted to HashLookups' own columns and are
// alias-qualified with `h`, so `action` and `createdAt` — which exist in both
// tables — are unambiguous.
func (s Searcher) SearchHashLookups(ctx context.Context, q Query) (Result, error) {
	where := BuildWhere(q.Search, q.Logic(), HashLookupFields, "h")
	whereSQL := ""
	if where.SQL != "" {
		whereSQL = " WHERE " + where.SQL
	}

	var total int64
	countSQL := "SELECT COUNT(*)" + hashLookupFrom + whereSQL
	if err := s.DB.QueryRowContext(ctx, countSQL, where.Values...).Scan(&total); err != nil {
		return Result{}, fmt.Errorf("count HashLookups: %w", err)
	}

	limit, offset, _ := q.LimitOffset()
	orderBy := BuildOrderBy(q.Sort, HashLookupFields, "`h`.`id` DESC", "h")

	pageSQL := hashLookupSelect + hashLookupFrom + whereSQL +
		" ORDER BY " + orderBy + " LIMIT ? OFFSET ?"
	args := append(append([]any{}, where.Values...), limit, offset)

	rs, err := s.DB.QueryContext(ctx, pageSQL, args...)
	if err != nil {
		return Result{}, fmt.Errorf("search HashLookups: %w", err)
	}
	records, err := rows.Scan(rs)
	if err != nil {
		return Result{}, fmt.Errorf("search HashLookups: %w", err)
	}

	return Result{Status: "success", Total: total, Records: records}, nil
}

// Columns reports the real column names of a table, using the same statement
// shape the searches use so it needs no information_schema grant.
//
// LIMIT 0 returns metadata and no rows, which is exactly what is wanted: this
// runs at startup against four tables and must not read data.
func (s Searcher) Columns(ctx context.Context, table string) ([]string, error) {
	rs, err := s.DB.QueryContext(ctx, "SELECT * FROM "+quoteIdent(table)+" LIMIT 0")
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	defer func() { _ = rs.Close() }()

	cols, err := rs.Columns()
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	return cols, nil
}

// FieldCheck reports how one table's allowlist compares with its real columns.
type FieldCheck struct {
	Table string
	// Missing names allowlisted fields that are not columns. Fatal: such a
	// field can only produce a broken query, and its presence means the binary
	// and the schema disagree about what this table is.
	Missing []string
	// Unlisted names real columns that are neither allowlisted nor deliberately
	// unfiltered. A warning, not fatal — this is the failure mode the allowlist
	// comments warn about: a filter that appears to work and returns every row.
	Unlisted []string
}

// CheckFields compares every allowlist against the live schema.
//
// It exists because BuildWhere's silent skip makes both kinds of drift invisible
// at runtime. Returning both directions separately lets the caller be strict
// about the one that can only be a bug and lenient about the one that is
// sometimes a choice.
func (s Searcher) CheckFields(ctx context.Context) ([]FieldCheck, error) {
	tables := []struct {
		name    string
		allowed map[string]struct{}
	}{
		{TableConnection, ConnectionFields},
		{TableDelivery, DeliveryFields},
		{TableTransaction, TransactionFields},
		{TableHashLookups, HashLookupFields},
	}

	var out []FieldCheck
	for _, t := range tables {
		cols, err := s.Columns(ctx, t.name)
		if err != nil {
			return nil, err
		}
		real := make(map[string]struct{}, len(cols))
		for _, c := range cols {
			real[c] = struct{}{}
		}

		check := FieldCheck{Table: t.name}
		// Iterate the column list, not the map, so the report is in table order
		// and stable between runs.
		for _, c := range cols {
			if _, ok := t.allowed[c]; ok {
				continue
			}
			if _, ok := unfiltered[c]; ok {
				continue
			}
			check.Unlisted = append(check.Unlisted, c)
		}
		for f := range t.allowed {
			if _, ok := real[f]; !ok {
				check.Missing = append(check.Missing, f)
			}
		}
		// Map iteration order is randomised, and this list ends up in a fatal
		// startup error. Sorting makes that message reproducible, which matters
		// when somebody is comparing it against a colleague's.
		sort.Strings(check.Missing)
		out = append(out, check)
	}
	return out, nil
}
