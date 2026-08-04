// Command logservice is the audit-trail API for the mail gateway fleet, and the
// owner of the shared database's schema.
//
// Usage:
//
//	logservice            serve (migrate first, then bind)
//	logservice serve      the same thing, said out loud
//	logservice migrate    apply pending migrations and exit
//	logservice version    print the version and exit
//
// # This binary IS configured by its environment
//
// mailgw-go is not, and its CI asserts as much: a zero-configuration edge node
// takes everything from Central Management, because it runs on a host its
// operator does not otherwise touch. This is the opposite kind of service — it
// runs on the core node, beside the database whose credentials it needs. It
// reads PORT, API_KEY, DB_HOST, DB_PORT, DB_USER, DB_PASS, DB_NAME, plus
// LOG_LEVEL and LOG_FORMAT. Do not copy mailgw-go's os.Getenv ban into this
// module.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ngmaibulat/mailgw/logservice-go/internal/db"
	"github.com/ngmaibulat/mailgw/logservice-go/internal/migrate"
)

// Set by -ldflags at build time; see Dockerfile.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	// Signals are handled here rather than in each subcommand so that a
	// docker-compose stop interrupts the database wait too. Without this, a
	// migrator pointed at a database that never comes up ignores SIGTERM for its
	// whole 20-second retry schedule and is then SIGKILLed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	log := newLogger()
	slog.SetDefault(log)

	var err error
	switch cmd {
	case "", "serve":
		err = runServe(ctx, log)
	case "migrate":
		err = runMigrate(ctx, log)
	case "version":
		fmt.Printf("logservice %s (%s)\n", version, commit)
		return
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		// A cancelled context is an operator stopping the process, not a
		// failure. Exiting non-zero would make `docker compose down` look like
		// a crash in every log aggregator watching exit codes.
		if errors.Is(err, context.Canceled) {
			log.Info("shutting down")
			return
		}
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `logservice — audit API and schema owner for the mail gateway fleet

  logservice           serve (applies pending migrations first)
  logservice serve     the same
  logservice migrate   apply pending migrations and exit
  logservice version   print the version

Configured entirely by the environment:
  PORT       listen port                       (default 3000)
  API_KEY    required in X-API-Key when set    (unset = every request accepted)
  DB_HOST    MariaDB host                      (default 127.0.0.1)
  DB_PORT    MariaDB port                      (default 3306)
  DB_USER    MariaDB user
  DB_PASS    MariaDB password
  DB_NAME    MariaDB database
  LOG_LEVEL  debug | info | warn | error       (default info)
  LOG_FORMAT json | text                       (default json)
`)
}

// runMigrate applies pending migrations and exits.
//
// This subcommand exists even though `serve` now migrates on start, and it is
// not redundant: both compose files gate the CONSOLE on
// `db-migrator: service_completed_successfully`, and the console never talks to
// logservice at boot. Without a one-shot migrator the console could start
// against a database with no Users table.
func runMigrate(ctx context.Context, log *slog.Logger) error {
	if err := migrate.Check(); err != nil {
		return err
	}
	cfg := dbConfigFromEnv()

	d, err := db.OpenForMigrations(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	if err := db.Wait(ctx, d, cfg, log); err != nil {
		return err
	}
	return migrate.Run(ctx, d, log)
}

// dbConfigFromEnv reads the DB_* variables.
//
// The defaults match logservice/example.env. DB_USER, DB_PASS and DB_NAME have
// none deliberately: a default database name is how you end up writing an audit
// trail into the wrong schema and only finding out when somebody goes looking
// for it.
func dbConfigFromEnv() db.Config {
	return db.Config{
		Host: envOr("DB_HOST", "127.0.0.1"),
		Port: envInt("DB_PORT", 3306),
		User: os.Getenv("DB_USER"),
		Pass: os.Getenv("DB_PASS"),
		Name: os.Getenv("DB_NAME"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt falls back on anything unparseable rather than failing.
//
// A malformed PORT should not stop an audit service from starting: the fallback
// is a working default, and the alternative is a fleet of gateways spilling
// events to disk because somebody typed `PORT=3000 ` with a trailing space.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("ignoring malformed environment value", "key", key, "value", v, "using", fallback)
		return fallback
	}
	return n
}

// newLogger mirrors mailgw-go's internal/node.newLogger: JSON to stderr by
// default, text opt-in, so the two services' output can be shipped by the same
// collector without a per-service parser.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if os.Getenv("LOG_FORMAT") == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
