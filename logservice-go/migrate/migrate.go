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
//
// # Exported, not internal, because logservice-fiber shares it
//
// M23 put a second implementation beside this one. The two differ in their HTTP
// layer and in nothing else, which is the only reason comparing them means
// anything — so everything below HTTP lives here and is imported by both.
// internal/api is what stays internal. Do not add an HTTP type to this package.
//
// Sharing this one keeps the 26 .sql files single-sourced, which matters more
// here than anywhere else: the CI step that byte-compares them against the
// frozen Bun originals only has two directories to compare because there are
// only two copies. It does mean two binaries now migrate — see Run.
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
// The UNIQUE on `name` stops two runners from both RECORDING the same file. It
// was once described here as also stopping the second APPLICATION of the DDL,
// and that is not what it does: Run reads the applied set once, up front, and
// records a file only after executing it, so two runners that start together
// both see an empty set and both reach the Exec.
//
// Which way the loser then dies depends on which file the race lands on, and
// only one of the two is survivable. On a file whose DDL happens to be
// idempotent (`CREATE TABLE IF NOT EXISTS`) the Exec succeeds twice and this
// constraint catches the second INSERT — an honest failure with the schema
// intact. On one of the six that are not (ALTER TABLE ADD COLUMN, CREATE INDEX)
// the Exec itself fails, and the file is left PARTIALLY applied and unrecorded,
// which is the state the error text below exists to explain.
//
// What actually serialises two runners is the advisory lock Run takes; see
// lockName. Keep the UNIQUE anyway — it is in the Bun runner's schema, an
// existing database has it, and it is a cheap backstop for the recording race
// the lock has already prevented.
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
//
// # Two runners are serialised, because since M23 there are two
//
// Everything below happens under a named advisory lock, on ONE connection taken
// out of the pool for the whole run. Without it, two migrators starting in the
// same second — which is what `docker compose up` on a fresh volume does — both
// read an empty applied set and both execute all 26 files, and the paragraph
// above says what happens next. The applied set is read AFTER the lock is held,
// so the loser of the race sees the winner's work and applies nothing.
func Run(ctx context.Context, d *sql.DB, log *slog.Logger) error {
	// One connection for the whole run. The lock is session-scoped, so it must
	// be taken and released on the same connection the migrations run on —
	// which is also why nothing below touches `d` directly. Note that
	// db.OpenForMigrations caps the pool at a single connection, so a stray
	// d.ExecContext here would deadlock against this one rather than merely
	// running unlocked.
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	release, err := lock(ctx, conn, log)
	if err != nil {
		return err
	}
	defer release()

	if _, err := conn.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("create _migrations table: %w", err)
	}

	applied, err := appliedSet(ctx, conn)
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

		if _, err := conn.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf(
				"migration %s failed: %w\n"+
					"the schema may be PARTIALLY migrated: MariaDB auto-commits DDL, so any "+
					"statements in this file before the failing one have been applied and %s "+
					"has not been recorded. Not every file is idempotent — inspect the schema "+
					"before re-running", name, err, name)
		}

		// Recorded only after the file succeeded, which is the same order the
		// Bun runner used. A crash between the two re-runs the file; see above.
		if _, err := conn.ExecContext(ctx,
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
//
// It takes the locked *sql.Conn rather than the pool, because reading this on a
// different connection would read it outside the lock — which is the whole
// defect the lock exists to close.
func appliedSet(ctx context.Context, d *sql.Conn) (map[string]struct{}, error) {
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

// lockName is the advisory lock every migrator takes for the length of its run.
//
// A MariaDB named lock rather than a row in a table, because it is released
// when the session ends: a migrator killed mid-run — `docker compose down`, an
// OOM, a lost network — takes its lock with it, where a row would need a
// timeout nobody can size and would eventually need a human to clear. It is
// also free to take when uncontended, which is every start after the first.
//
// The name is a bare string and not qualified by database, because MariaDB
// namespaces these per SERVER rather than per schema. That is deliberate on
// balance: two logservice stacks sharing one MariaDB server would serialise
// with each other unnecessarily, which costs a few seconds once, while the
// alternative — an unqualified name colliding with something else — costs a
// deadlock nobody would look for here.
const lockName = "logservice_migrate"

// lockWait bounds how long a migrator waits for one already running.
//
// 120s is sized against the work rather than picked round: a full 26-file run
// against an empty database is a few seconds, and this has to also cover the
// pathological case of a first boot on slow storage. Waiting is much better
// than failing here — the loser almost always has nothing to do once it gets
// in, so the wait is the whole cost.
const lockWait = 120

// lock takes the advisory lock and returns the function that releases it.
//
// The first attempt uses a zero timeout so the common case — nobody else
// migrating — costs one round trip and logs nothing. Only when that fails does
// it announce that it is waiting, which is exactly when an operator watching
// `docker compose logs` needs to be told why nothing is happening.
func lock(ctx context.Context, conn *sql.Conn, log *slog.Logger) (func(), error) {
	got, err := getLock(ctx, conn, 0)
	if err != nil {
		return nil, err
	}
	if !got {
		log.Info("another migrator holds the schema lock; waiting",
			"lock", lockName, "timeout_seconds", lockWait)
		if got, err = getLock(ctx, conn, lockWait); err != nil {
			return nil, err
		}
		if !got {
			return nil, fmt.Errorf(
				"timed out after %ds waiting for the %q lock: another process is "+
					"migrating this database and has not finished. If nothing else is "+
					"running, a migrator was killed while holding the lock and its "+
					"connection has not yet been reaped by the server",
				lockWait, lockName)
		}
		log.Info("schema lock acquired", "lock", lockName)
	}

	return func() {
		// Not ctx: a cancelled context is precisely the shutdown path, and the
		// release should still be sent. Closing the connection would release it
		// anyway — this is so the server reclaims it now rather than whenever
		// it notices the session is gone.
		rctx := context.WithoutCancel(ctx)
		if _, err := conn.ExecContext(rctx, `SELECT RELEASE_LOCK(?)`, lockName); err != nil {
			log.Warn("could not release the schema lock; it will be released when the connection closes",
				"lock", lockName, "err", err)
		}
	}, nil
}

// getLock returns whether the lock was taken within timeout seconds.
//
// GET_LOCK answers 1 for acquired, 0 for timed out and NULL for "an error
// occurred" — which is a different thing from a timeout and must not be read as
// one, or a broken server would look like a busy one and this would wait the
// full 120s to say so.
func getLock(ctx context.Context, conn *sql.Conn, timeout int) (bool, error) {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, lockName, timeout).Scan(&got); err != nil {
		return false, fmt.Errorf("acquire the %q schema lock: %w", lockName, err)
	}
	if !got.Valid {
		return false, fmt.Errorf(
			"acquire the %q schema lock: the server returned NULL, which means the "+
				"lock could not be evaluated at all rather than that it is held", lockName)
	}
	return got.Int64 == 1, nil
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
