// Package migrate applies the SQL files in the migrations package to MariaDB.
//
// # This service owns the schema for the whole stack
//
// Not just the log tables. The console (webui-fastify) describes the tables it
// queries in db/schema.ts but deliberately does not own or migrate them, and
// there is no drizzle-kit there for that reason. Fifteen of the tables these
// files create — Users, Sessions, Relays, Gateways, ConfigProfiles,
// ConfigVersions, CredentialSets and the rest — are read and written only by the
// console. A migration that does not run here is a console that does not work.
//
// # The contract with the frozen Bun runner
//
// `_migrations` records an applied migration by FILENAME. That is the whole
// upgrade story: this runner and logservice/src/dbmigrate.ts agree on the table
// shape and on the key, so a database migrated by one is seen as migrated by the
// other. Do not add a checksum column or an ordinal — either would make an
// existing production database, which has 26 rows in this table, look unmigrated
// or corrupt.
//
// There are no down migrations, exactly as before.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/ngmaibulat/mailgw/logservice-go/migrations"
)

// createTable is the tracking table, byte-compatible with the one the Bun runner
// creates (logservice/src/dbmigrate.ts).
//
// The UNIQUE on `name` is load-bearing and not decoration: it is what stops two
// runners started at the same moment — the `db-migrator` compose service and a
// `logservice` that now also migrates on start — from both recording the same
// file. The loser gets a duplicate-key error on the INSERT rather than a second
// application of the DDL. Keep it.
const createTable = `
CREATE TABLE IF NOT EXISTS _migrations (
    id        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name      VARCHAR(255) NOT NULL UNIQUE,
    appliedAt DATETIME     NOT NULL
)`

// Names lists the embedded migrations in apply order.
//
// Sorting is plain lexicographic, which is apply order only because every file
// carries a zero-padded three-digit prefix. That is the same rule the Bun runner
// relies on; keep the prefix.
func Names() ([]string, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Run applies every migration the database has not recorded, in order.
//
// `d` must be a connection opened with multi-statement support (see
// db.OpenForMigrations): a migration file is executed WHOLE, in one Exec, which
// is what the Bun runner did and therefore what these files were written
// against. Six of them hold several statements.
//
// A file is not applied atomically and cannot be: MariaDB auto-commits DDL, so
// BEGIN buys nothing here. A file that fails halfway leaves its earlier
// statements applied and its `_migrations` row unwritten, and re-running it will
// then fail on the already-applied part — because ALTER TABLE ADD COLUMN and
// CREATE INDEX are not idempotent and six files use them. That is the behaviour
// the Bun runner had. What is new is that the error SAYS so, because the
// alternative is an operator re-running a migrator that fails with a bare
// "duplicate column name" and no idea why.
func Run(ctx context.Context, d *sql.DB, log *slog.Logger) error {
	if _, err := d.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("create _migrations table: %w", err)
	}

	applied, err := appliedSet(ctx, d)
	if err != nil {
		return err
	}

	names, err := Names()
	if err != nil {
		return err
	}

	count := 0
	for _, name := range names {
		if _, ok := applied[name]; ok {
			continue
		}

		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		if _, err := d.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf(
				"migration %s failed: %w\n"+
					"the schema may be PARTIALLY migrated: MariaDB auto-commits DDL, so any "+
					"statements in this file before the failing one have been applied and %s "+
					"has not been recorded. Not every file is idempotent — inspect the schema "+
					"before re-running", name, err, name)
		}

		// Recorded only after the file succeeded, which is the same order the
		// Bun runner used. A crash between the two re-runs the file; see above.
		if _, err := d.ExecContext(ctx,
			`INSERT INTO _migrations (name, appliedAt) VALUES (?, NOW())`, name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}

		log.Info("migration applied", "migration", name)
		count++
	}

	if count == 0 {
		log.Info("schema up to date", "migrations", len(names))
	} else {
		log.Info("migrations applied", "applied", count, "migrations", len(names))
	}
	return nil
}

// appliedSet reads the filenames already recorded.
//
// A missing table is not tolerated here — Run creates it first — so an error is
// a real one and is returned rather than treated as "nothing applied", which
// would re-run all 26 files against a populated database.
func appliedSet(ctx context.Context, d *sql.DB) (map[string]struct{}, error) {
	rows, err := d.QueryContext(ctx, `SELECT name FROM _migrations`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read applied migrations: %w", err)
		}
		applied[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return applied, nil
}

// ErrNoMigrations reports an empty embedded set, which can only mean the
// go:embed directive stopped matching — a build that would silently serve an
// unmigrated database.
var ErrNoMigrations = errors.New("no migrations are embedded in this binary")

// Check validates the embedded set at startup, before anything touches the
// database. It exists because go:embed failing to match is a compile-time
// success and a runtime disaster: Run would find nothing to do and report
// "schema up to date" against an empty database.
func Check() error {
	names, err := Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return ErrNoMigrations
	}
	return nil
}
