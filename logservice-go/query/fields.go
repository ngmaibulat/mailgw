package query

// The per-table field allowlists. A field not listed here is not filterable and
// not sortable.
//
// # The failure mode these cause, which is why they are tested
//
// BuildWhere SILENTLY SKIPS a field it does not recognise — it does not error
// and does not 400. That is deliberate and is what the console relies on, but it
// means a column added to a table and to a grid but forgotten HERE produces a
// filter that appears to work and returns EVERY ROW. logservice's own
// src/query/search.ts carries the same warning, and migration 023 repeats it.
//
// Add to these in the same commit as the column. internal/api's startup check
// compares them against the real columns and refuses to start if a listed field
// does not exist, but the reverse — a real column nobody listed — can only be a
// warning, because createdAt/updatedAt are unlisted on purpose.
var (
	// DeliveryFields is the allowlist for the Delivery table.
	DeliveryFields = set(
		"id", "uuid", "dt", "sender", "rcpt_domain", "rcpt_list", "rcpt_accepted",
		"tls_forced", "tls", "auth", "host", "ip", "port", "response", "delay",
		"gateway", "route_rule",
	)

	// ConnectionFields is the allowlist for the Connection table. Note the mixed
	// casing: remoteAddr and remotePort are camel, everything else is snake.
	// That is the shape of the table, not a typo.
	ConnectionFields = set(
		"id", "uuid", "dt", "encoding", "hello_name", "remoteAddr", "remotePort",
		"remote_host", "remote_info", "remote_is_local", "remote_is_private",
		"using_tls", "tran_count", "rcpt_count_accept", "rcpt_count_tempfail",
		"rcpt_count_reject", "gateway",
	)

	// TransactionFields is the allowlist for the Transaction table.
	//
	// route_rule is deliberately absent: routing is evaluated per RECIPIENT and
	// a Transaction row is one message, so the column exists only on Delivery.
	TransactionFields = set(
		"id", "uuid", "dt", "action", "encoding", "sender", "rcpt_list",
		"rcpt_count_accept", "rcpt_count_tempfail", "rcpt_count_reject",
		"delay_data_post", "data_bytes", "mime_part_count", "gateway",
	)

	// HashLookupFields is the allowlist for HashLookups.
	//
	// Only HashLookups' own columns. The Transaction columns the search JOINs in
	// (sender, rcpt_list, dt, …) are display-only in the viewer grid and are not
	// filterable — the grid marks them with LogGrid.display for that reason.
	HashLookupFields = set(
		"id", "txn_uuid", "md5", "contentType", "filename", "size", "action",
		"createdAt",
	)
)

// unfiltered names columns that exist in a table but are deliberately not
// filterable, so the startup check can tell "chose not to expose this" apart
// from "forgot to expose this" and only warn about the second.
//
// createdAt and updatedAt are audit bookkeeping — when the ROW was written, not
// when the event happened. `dt` is the event time and is what anybody actually
// filters on. Exposing both would offer two nearly-identical date columns whose
// difference is invisible in a grid.
var unfiltered = set("createdAt", "updatedAt")

func set(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}
