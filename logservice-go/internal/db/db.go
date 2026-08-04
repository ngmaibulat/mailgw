// Package db owns the connection to MariaDB: how the DSN is built, how the pool
// is configured, and how startup waits for a database that is still coming up.
//
// It is deliberately the only package that imports the MySQL driver, on the
// precedent of mailgw-go's internal/store — everything else is plain
// database/sql, so swapping the driver is a change to this file and nothing
// else.
//
// # Two pools, and why
//
// Open returns the pool that serves requests. It does NOT enable the driver's
// multi-statement support. OpenForMigrations returns a separate, short-lived
// connection that does. The split is the point: multi-statement support turns
// any SQL injection from a one-statement problem into an arbitrary-script one,
// and the search path (internal/query) is exactly where caller-supplied input
// reaches SQL. The migration runner needs it because a migration file is
// executed whole; a request handler never does.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Config is everything needed to reach the database. Every field comes from an
// environment variable — see cmd/logservice.
//
// Note that logservice, unlike the gateway, IS configured by its environment.
// mailgw-go's CI asserts that binary calls no os.Getenv, because a zero-config
// edge node must take everything from Central Management. This is a core-node
// service whose operator owns the host it runs on. Do not copy that assertion
// into this module.
type Config struct {
	Host string
	Port int
	User string
	Pass string
	Name string
}

// Addr renders host:port for log lines and error messages. A failure to reach
// the database is by far the most common startup failure, and an error that
// does not name the address it tried is one the reader cannot act on.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// dsn builds the driver connection string.
//
// It is assembled with the driver's own mysql.Config rather than concatenated,
// so a password containing '@', ':' or '/' — which is exactly the kind of
// password a generated secret is — cannot corrupt the DSN. FormatDSN escapes
// what needs escaping.
//
// parseTime is deliberately LEFT OFF. See internal/rows for the whole argument;
// the short version is that a MariaDB DATETIME carries no timezone, so
// parseTime would stamp one on and the API would start disagreeing with what
// the same operator sees in `mysql`. The audit trail is worth more when the two
// agree.
func dsn(c Config, multiStatements bool) string {
	m := mysql.NewConfig()
	m.Net = "tcp"
	m.Addr = c.Addr()
	m.User = c.User
	m.Passwd = c.Pass
	m.DBName = c.Name
	// The Bun service used a 10s connection timeout to "fail fast instead of
	// hanging". Same number, same reason.
	m.Timeout = 10 * time.Second
	// utf8mb4 so a subject line or a display name with an emoji in it does not
	// error the insert. The Bun driver negotiated this by default; the Go one
	// does not, and a mismatch here is invisible until a real message arrives.
	m.Params = map[string]string{"charset": "utf8mb4"}
	m.MultiStatements = multiStatements
	// InterpolateParams is left OFF: the driver then uses real prepared
	// statements, so a placeholder value can never be re-parsed as SQL. This
	// service builds WHERE clauses from caller-supplied JSON, so that guarantee
	// is the one it most depends on.
	return m.FormatDSN()
}

// Open returns the request-serving pool. It does not connect — database/sql is
// lazy — so a caller that needs to know the database is reachable calls Wait.
func Open(c Config) (*sql.DB, error) {
	d, err := sql.Open("mysql", dsn(c, false))
	if err != nil {
		return nil, fmt.Errorf("open database at %s: %w", c.Addr(), err)
	}
	// The Bun service left every pool bound at its driver's defaults, which for
	// database/sql means MaxOpenConns is UNLIMITED — a slow query storm would
	// open connections until MariaDB's max_connections refused them, and the
	// refusal would land on the gateway as a 5xx per audit event. These are
	// modest, explicit ceilings rather than a tuning exercise: this service does
	// small inserts and paginated reads.
	d.SetMaxOpenConns(25)
	d.SetMaxIdleConns(5)
	// Shorter than any reasonable proxy or MariaDB wait_timeout, so a connection
	// is retired by us rather than discovered dead by a request.
	d.SetConnMaxLifetime(5 * time.Minute)
	d.SetConnMaxIdleTime(2 * time.Minute)
	return d, nil
}

// OpenForMigrations returns a connection that accepts several statements in one
// Exec, which is what a migration file is.
//
// It is capped at a single connection: the runner is sequential and two
// concurrent migration connections have no use, while the cap makes it obvious
// that this pool is not for serving anything. Close it as soon as the run ends —
// see the comment on the package.
func OpenForMigrations(c Config) (*sql.DB, error) {
	d, err := sql.Open("mysql", dsn(c, true))
	if err != nil {
		return nil, fmt.Errorf("open database at %s for migrations: %w", c.Addr(), err)
	}
	d.SetMaxOpenConns(1)
	return d, nil
}

// Wait blocks until the database answers, or gives up.
//
// The schedule — 10 attempts, 2s apart — is carried over verbatim from the Bun
// runner (logservice/src/dbmigrate.ts), because it is tuned to the same thing:
// compose starting MariaDB and this service in the same second. Neither compose
// file has a working MariaDB healthcheck (the one in docker-compose.yaml is
// commented out), so this retry IS the readiness gate for the whole stack.
// The console's own boot wait (webui-fastify db/index.ts waitForSchema, 90s) is
// sized to sit outside this schedule plus the migrations that follow it: this
// can burn 20s before the first file is applied.
//
// It logs on each failed attempt rather than only at the end, because the
// interesting case is an operator watching `docker compose logs` wondering
// whether anything is happening.
func Wait(ctx context.Context, d *sql.DB, c Config, log *slog.Logger) error {
	const (
		attempts = 10
		delay    = 2 * time.Second
	)

	var last error
	for i := 1; i <= attempts; i++ {
		// Each ping gets its own deadline. Without one a ping inherits only the
		// driver's dial timeout, which does not cover a TCP connection that
		// establishes and then goes silent — the exact shape of a database that
		// is up but still recovering.
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := d.PingContext(pingCtx)
		cancel()
		if err == nil {
			if i > 1 {
				log.Info("database reachable", "addr", c.Addr(), "attempt", i)
			}
			return nil
		}
		last = err

		// A cancelled parent is a shutdown, not a slow database. Retrying would
		// hold the process open for another 20 seconds for no reason.
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for database at %s: %w", c.Addr(), ctx.Err())
		}
		if i == attempts {
			break
		}
		// delay.String(), not delay: slog's JSON handler renders a
		// time.Duration as an integer count of nanoseconds, so this line would
		// otherwise read "in": 2000000000 to the operator watching a stack come
		// up — which is the one audience it exists for.
		log.Warn("database not ready yet, retrying",
			"addr", c.Addr(), "attempt", i, "of", attempts,
			"retry_in", delay.String(), "err", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for database at %s: %w", c.Addr(), ctx.Err())
		case <-time.After(delay):
		}
	}

	return fmt.Errorf(
		"could not connect to the database at %s after %d attempts — is MariaDB running and reachable? (last error: %w)",
		c.Addr(), attempts, last)
}
