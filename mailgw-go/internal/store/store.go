// Package store is the gateway's local persistent state: its identity, its
// settings, and the configuration bundles it has pulled from Central
// Management. Nothing else in the module writes SQL.
//
// The database is a single SQLite file with exactly one writing process. That
// is why there is no migration library here and no connection pool worth the
// name — see migrate and Open.
//
// # Swapping the driver
//
// This file is the only one that imports a SQLite driver; everything else in
// the package is plain database/sql. If modernc.org/sqlite's dependency weight
// ever becomes a problem, zombiezen.com/go/sqlite is the intended replacement
// and this file is the only one that has to change.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// DBFile is the database file name within the data directory.
const DBFile = "mailgw.db"

// Store owns the connection to the local database.
type Store struct {
	db   *sql.DB
	path string

	// mu serialises Identity's read-or-create. The IMMEDIATE transaction and
	// the CHECK (id = 1) constraint both guard the cross-process case; this
	// guards goroutines cheaply enough not to matter.
	mu sync.Mutex
}

// dsn builds the connection string.
//
// It is assembled with net/url rather than concatenated: url.URL.String()
// percent-escapes the path, and the driver's url.Values decode undoes it, so a
// data directory containing a space or a '?' still works. Concatenating would
// silently drop every pragma after the first oddity in the path — a failure
// that looks like the pragmas simply not being honoured.
func dsn(path string) string {
	q := url.Values{}
	// The admin UI and the poll loop both touch this file.
	q.Add("_pragma", "journal_mode(WAL)")
	// Covers a second process (`mailgw-go check -data ...`) holding the lock.
	q.Add("_pragma", "busy_timeout(5000)")
	// Correct and durable under WAL, and much cheaper than FULL.
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	// Removes any dependency on a writable temp directory, which the
	// distroless runtime image does not guarantee.
	q.Add("_pragma", "temp_store(MEMORY)")
	// Identity() reads and then writes inside one transaction. Under WAL a
	// DEFERRED transaction that starts as a reader and later upgrades to a
	// writer gets SQLITE_BUSY immediately — busy_timeout does not retry a lock
	// upgrade, because retrying cannot resolve it. Taking the write lock up
	// front is what makes the read-or-create safe.
	q.Set("_txlock", "immediate")

	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// Open creates the data directory if needed, opens the database and applies any
// pending migrations.
//
// The directory is 0700 and the database 0600: this file holds an Ed25519
// private key.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	// MkdirAll honours the umask, and the directory may already exist (Docker
	// creates a volume mount point 0755). Tighten it. A container that does not
	// own its mount cannot chmod it; that is worth a warning, not a refusal —
	// the database file mode below is the control that actually matters.
	if err := os.Chmod(dir, 0o700); err != nil {
		slog.Warn("cannot tighten data dir permissions", "dir", dir, "err", err)
	}

	path := filepath.Join(dir, DBFile)
	// Create the file ourselves at 0600 before the driver can create it at
	// 0644, so the private key is never briefly world-readable. SQLite copies
	// the main database's mode onto the -wal and -shm files, so this covers
	// those too.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	_ = f.Close()

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One process, one writer, a database measured in kilobytes. Serialising
	// removes SQLITE_BUSY as a class of bug inside this process entirely, and
	// leaves busy_timeout to cover only a second process.
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	// sql.Open is lazy, so nothing above has actually touched the file yet.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, path: path}, nil
}

// Path is the database file's location, for logging.
func (s *Store) Path() string { return s.path }

// Close releases the connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// migrations[i] takes user_version from i-1 to i. Index 0 is unused.
//
// Never edit a shipped entry — append a new one. A single local file with one
// writer does not justify a migration library, but it does justify each step
// being atomic, which is why these are statement lists applied in a
// transaction rather than one semicolon-joined blob: a failure names the
// statement that failed, and nothing depends on the driver's multi-statement
// Exec behaviour.
var migrations = [][]string{
	0: nil,
	1: {
		`CREATE TABLE identity (
			id           INTEGER PRIMARY KEY CHECK (id = 1),
			gateway_uid  TEXT NOT NULL DEFAULT '',
			public_key   BLOB NOT NULL,
			private_key  BLOB NOT NULL,
			fingerprint  TEXT NOT NULL,
			created_at   INTEGER NOT NULL
		)`,
		`CREATE TABLE settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE config_cache (
			version_id  INTEGER PRIMARY KEY,
			version     INTEGER NOT NULL,
			sha256      TEXT    NOT NULL,
			bundle      BLOB    NOT NULL,
			fetched_at  INTEGER NOT NULL,
			applied_at  INTEGER,
			apply_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX idx_config_cache_fetched ON config_cache (fetched_at DESC)`,
	},
	2: {
		// "Most recently applied" is a sequence question, not a timestamp one.
		// applied_at has one-second resolution, so a rollback performed in the
		// same second as the deploy it undoes tie-broke on version_id — and the
		// higher version_id is exactly the bundle the operator just rejected.
		// Boot reads the applied row, so that tie would have undone a rollback
		// on the next restart.
		`ALTER TABLE config_cache ADD COLUMN applied_seq INTEGER NOT NULL DEFAULT 0`,
		// Existing rows: preserve the old ordering as the starting sequence, so
		// an upgrade does not reshuffle what a gateway thinks it is running.
		`UPDATE config_cache SET applied_seq = version_id WHERE applied_at IS NOT NULL`,
	},
	3: {
		// Admin UI sessions (M12). Deliberately in the database rather than in
		// the process: a restart is most likely to happen mid-provisioning —
		// a bundle applies with restart_required and the operator restarts —
		// and that is the worst possible moment to drop an operator back to the
		// claim page. The per-session CSRF token has to survive with it, or a
		// form rendered before the restart fails inscrutably when submitted
		// after it. And `mailgw-go claim reset` needs somewhere to revoke every
		// session at once, which is a DELETE here and nothing anywhere else.
		//
		// No index on expires_at: this table holds single digits of rows.
		`CREATE TABLE admin_sessions (
			id         TEXT PRIMARY KEY,
			csrf       TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
	},
}

func migrate(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if have >= len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this build understands (%d)",
			have, len(migrations)-1)
	}

	for v := have + 1; v < len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrate to %d: %w", v, err)
		}
		for _, stmt := range migrations[v] {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("migrate to %d: %w", v, err)
			}
		}
		// PRAGMA takes no bind parameters. v is a loop index over a package
		// literal, so this Sprintf cannot carry untrusted input.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate to %d: set version: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate to %d: commit: %w", v, err)
		}
	}
	return nil
}

// querier is satisfied by both *sql.DB and *sql.Tx, so a read can run either
// inside a transaction or on its own.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var errNotFound = errors.New("store: no such row")
