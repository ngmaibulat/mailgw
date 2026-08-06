package migrate

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// These tests need a real MariaDB, so they are opt-in and skipped by default —
// `go test ./...` stays offline in both modules and in CI, which is the same
// rule the rest of this repo's Go suites follow.
//
//	MAILGW_MIGRATE_DB_TEST=1 DB_HOST=127.0.0.1 DB_USER=root DB_PASS=... go test ./migrate/ -run DB -v
//
// The driver is imported here and only here in this package: importing it in
// non-test code would make `migrate` a second package that pulls in the driver,
// where today db.go is deliberately the only one.

// scratchDSN builds a DSN for a throwaway database on the configured server.
// An empty dbName connects to the server itself, which is what CREATE DATABASE
// needs.
func scratchDSN(t *testing.T, dbName string, multi bool) string {
	t.Helper()
	c := mysql.NewConfig()
	c.Net = "tcp"
	c.Addr = envOr("DB_HOST", "127.0.0.1") + ":" + envOr("DB_PORT", "3306")
	c.User = envOr("DB_USER", "root")
	c.Passwd = envOr("DB_PASS", "")
	c.DBName = dbName
	c.Timeout = 10 * time.Second
	c.MultiStatements = multi
	c.Params = map[string]string{"charset": "utf8mb4"}
	return c.FormatDSN()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// scratchDB creates an empty database and returns its name, dropping it when
// the test ends. Named per test so a parallel run cannot collide.
func scratchDB(t *testing.T) string {
	t.Helper()
	if os.Getenv("MAILGW_MIGRATE_DB_TEST") == "" {
		t.Skip("set MAILGW_MIGRATE_DB_TEST=1 (and DB_*) to run the migration tests against a real MariaDB")
	}

	name := "mailgw_migrate_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	admin, err := sql.Open("mysql", scratchDSN(t, "", false))
	if err != nil {
		t.Fatalf("open server: %v", err)
	}
	defer func() { _ = admin.Close() }()

	if err := admin.Ping(); err != nil {
		t.Skipf("no MariaDB at %s: %v", envOr("DB_HOST", "127.0.0.1"), err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		d, err := sql.Open("mysql", scratchDSN(t, "", false))
		if err != nil {
			return
		}
		defer func() { _ = d.Close() }()
		_, _ = d.Exec("DROP DATABASE IF EXISTS `" + name + "`")
	})
	return name
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// This is the test the advisory lock exists for.
//
// Without the lock both runners read an empty applied set, both reach the first
// non-idempotent file, and the loser fails with `duplicate column name` — which
// under `restart: unless-stopped` is a restart loop, and which is what
// `docker compose up` on a fresh volume produces now that two services migrate.
func TestDBRun_TwoConcurrentRunnersBothSucceed(t *testing.T) {
	name := scratchDB(t)
	ctx := context.Background()

	// Two pools, not two connections from one: this is two processes in the
	// shipped stack, and a shared pool would hide a lock that only works
	// within one.
	open := func() *sql.DB {
		d, err := sql.Open("mysql", scratchDSN(t, name, true))
		if err != nil {
			t.Fatalf("open scratch database: %v", err)
		}
		d.SetMaxOpenConns(1) // as db.OpenForMigrations does
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	a, b := open(), open()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, d := range []*sql.DB{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // start both as close together as the runtime allows
			errs[i] = Run(ctx, d, discardLogger())
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			// Fatal rather than Error: without the lock this is where the test
			// stops, and the counts below would only report the wreckage.
			t.Fatalf("runner %d: %v", i, err)
		}
	}

	names, err := Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}

	var applied int
	if err := a.QueryRowContext(ctx, `SELECT COUNT(*) FROM _migrations`).Scan(&applied); err != nil {
		t.Fatalf("count applied: %v", err)
	}
	if applied != len(names) {
		t.Errorf("_migrations holds %d rows, want %d", applied, len(names))
	}

	// Every file recorded exactly once. The UNIQUE makes a duplicate row
	// impossible, so this is really asserting that nothing was skipped by a
	// runner that read the applied set at the wrong moment.
	var distinct int
	if err := a.QueryRowContext(ctx, `SELECT COUNT(DISTINCT name) FROM _migrations`).Scan(&distinct); err != nil {
		t.Fatalf("count distinct: %v", err)
	}
	if distinct != len(names) {
		t.Errorf("%d distinct migrations recorded, want %d", distinct, len(names))
	}
}

// A second Run against a migrated database applies nothing and answers quickly.
// This is the path every restart takes, and it is also what the loser of the
// race above does once it gets the lock.
func TestDBRun_IsIdempotent(t *testing.T) {
	name := scratchDB(t)
	ctx := context.Background()

	d, err := sql.Open("mysql", scratchDSN(t, name, true))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()

	if err := Run(ctx, d, discardLogger()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := Run(ctx, d, discardLogger()); err != nil {
		t.Fatalf("second run: %v", err)
	}

	names, _ := Names()
	var applied int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM _migrations`).Scan(&applied); err != nil {
		t.Fatalf("count: %v", err)
	}
	if applied != len(names) {
		t.Errorf("_migrations holds %d rows after two runs, want %d", applied, len(names))
	}
}

// The lock is only worth having if a second session actually cannot take it.
// Asserted with a zero timeout so the test costs a round trip rather than the
// 120s a real contended acquisition is allowed.
func TestDBLock_IsNotGrantedTwice(t *testing.T) {
	name := scratchDB(t)
	ctx := context.Background()

	d, err := sql.Open("mysql", scratchDSN(t, name, false))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()
	d.SetMaxOpenConns(2)

	held, err := d.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = held.Close() }()

	release, err := lock(ctx, held, discardLogger())
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	other, err := d.Conn(ctx)
	if err != nil {
		t.Fatalf("second conn: %v", err)
	}
	defer func() { _ = other.Close() }()

	got, err := getLock(ctx, other, 0)
	if err != nil {
		t.Fatalf("getLock: %v", err)
	}
	if got {
		t.Fatal("a second session took the lock while the first held it — Run is not serialised")
	}

	// And it is available again once released, or a crashed migrator would
	// lock the schema until somebody restarted MariaDB.
	release()
	got, err = getLock(ctx, other, 0)
	if err != nil {
		t.Fatalf("getLock after release: %v", err)
	}
	if !got {
		t.Fatal("the lock was not released")
	}
	if _, err := other.ExecContext(ctx, `SELECT RELEASE_LOCK(?)`, lockName); err != nil {
		t.Fatalf("release: %v", err)
	}
}
