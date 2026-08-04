package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"

	"github.com/ngmaibulat/mailgw/logservice-go/internal/api"
	"github.com/ngmaibulat/mailgw/logservice-go/internal/db"
	"github.com/ngmaibulat/mailgw/logservice-go/internal/migrate"
	"github.com/ngmaibulat/mailgw/logservice-go/internal/query"
)

// runServe applies pending migrations and then serves.
//
// # Migrate before binding, not after
//
// The Bun service did not migrate on start at all — that is what the separate
// db-migrator container was for. Doing it here means a fresh volume comes up
// working with no ordering to get right. Binding first and migrating in the
// background would be worse than not migrating: every gateway that reached the
// listener during the window would get a 500 per audit event, and mailgw-go
// retries a 5xx a few times and then spills the event to the gateway's disk.
//
// A failed migration is fatal. A logservice serving against a half-migrated
// schema answers 500 to some endpoints and 200 to others, which is harder to
// diagnose than a container that will not start.
func runServe(ctx context.Context, log *slog.Logger) error {
	// Checked before anything touches the network: an empty embedded set means
	// this binary would report "schema up to date" against an empty database.
	if err := migrate.Check(); err != nil {
		return err
	}

	cfg := dbConfigFromEnv()

	if err := migrateOnStart(ctx, cfg, log); err != nil {
		return err
	}

	// The request-serving pool. Separate from the migration connection, and
	// without multi-statement support — see internal/db for why that split is
	// load-bearing rather than tidy.
	pool, err := db.Open(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	if err := db.Wait(ctx, pool, cfg, log); err != nil {
		return err
	}

	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		// Once, loudly, at startup — the Bun service warned here too. Not per
		// request: a fleet of gateways would turn it into the whole log.
		log.Warn("API_KEY is not set — every request will be accepted, including from anything that can reach this port")
	}

	srv := &api.Server{
		DB:      pool,
		APIKey:  apiKey,
		Version: version,
		Log:     log,
	}

	// Run after the pool is up so it can read the real schema, and before the
	// listener binds so a mismatch is a startup failure rather than a filter
	// that quietly returns every row.
	if err := checkFields(ctx, pool, log); err != nil {
		return err
	}

	// Migrations are done and the schema checks out.
	srv.MarkReady()

	return srv.ListenAndServe(ctx, listenAddr())
}

// migrateOnStart runs the migrations on their own connection and closes it.
//
// The connection is closed before the serving pool opens, so the multi-statement
// capability exists for the few hundred milliseconds it is needed and not for
// the lifetime of the process.
func migrateOnStart(ctx context.Context, cfg db.Config, log *slog.Logger) error {
	m, err := db.OpenForMigrations(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	if err := db.Wait(ctx, m, cfg, log); err != nil {
		return err
	}
	return migrate.Run(ctx, m, log)
}

// checkFields compares the search allowlists against the live schema.
//
// The two directions are treated differently on purpose, because they mean
// different things:
//
//   - An allowlisted field that is NOT a column can only produce a broken query,
//     and it means the binary and the schema disagree about what a table is.
//     Fatal.
//   - A column nobody allowlisted is the failure mode the allowlist comments and
//     migration 023 both warn about — a filter that appears to work and returns
//     every row. A warning, not fatal, because it is sometimes a deliberate
//     choice (createdAt and updatedAt are excluded on purpose) and because
//     refusing to start over a column somebody added would take the audit trail
//     down for a cosmetic gap.
func checkFields(ctx context.Context, pool *sql.DB, log *slog.Logger) error {
	checks, err := query.Searcher{DB: pool}.CheckFields(ctx)
	if err != nil {
		return fmt.Errorf("check search fields: %w", err)
	}

	var broken []string
	for _, c := range checks {
		for _, f := range c.Unlisted {
			log.Warn("column is not searchable because no allowlist names it; "+
				"a filter on it would silently match every row",
				"table", c.Table, "column", f)
		}
		if len(c.Missing) > 0 {
			broken = append(broken, fmt.Sprintf("%s: %v", c.Table, c.Missing))
		}
	}

	if len(broken) > 0 {
		return fmt.Errorf(
			"the search allowlists name fields that are not columns: %v — "+
				"this build and this database disagree about the schema; check that every "+
				"migration applied", broken)
	}
	return nil
}

// listenAddr resolves PORT into a bind address.
//
// 0.0.0.0 because this is a container service other hosts reach — the edge
// gateways POST their audit events to it across the network. Binding loopback
// would be safer and would make the product not work.
func listenAddr() string {
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(envInt("PORT", 3000)))
}
