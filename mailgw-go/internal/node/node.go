// Package node is the gateway's composition root: it turns a data directory
// into a running gateway, and owns that gateway's relationship with Central
// Management.
//
// It exists as a package rather than as `package main` for two reasons, and the
// second is the one that mattered enough to move it here.
//
// The first is that there is now more than one binary. cmd/mailgw-go is the
// shipped node; cmd/mailgw-go-test is an engineering build that adds an
// unauthenticated control API for the end-to-end suites. Both must bring a
// gateway up through THE SAME code, or the thing the tests exercise is not the
// thing that ships.
//
// The second is that the bring-up wiring had no test that drove it. The listener
// chain (proxyproto -> Meter -> tls -> Guard -> Throttle -> Limit), the ordered
// shutdown and the apply path all lived in package main, so every test built its
// subject directly and none of them ran the wiring. docs/internal/dev/testing.md
// names that as a hazard, and M16 is the evidence: M11's connection cap passed
// every package test and still hid the *tls.Conn from go-smtp, costing an
// implicit_tls listener its TLS identity and its only pre-handshake read
// deadline. A defect reachable only through the wiring needs a test that goes
// through the wiring.
package node

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// Options is what a process knows that a configuration bundle cannot.
//
// There is deliberately no -config and no notion of "was this flag typed".
// Central Management is the only configuration source, so every command reads
// the same place and the defaults are the whole of a shipped node's command
// line: the image runs the binary with no arguments at all.
//
// What remains are properties of THIS PROCESS rather than of the configuration
// — where its state lives and where its provisioning UI listens — which is
// exactly why they cannot come from a bundle: DataDir is where the bundle would
// be cached, and AdminAddr is how a node with no bundle yet is provisioned.
//
// Version and Commit are build stamps. They are fields rather than package
// variables because two different binaries link this package and each sets its
// own via -ldflags; a package-level var here could only carry one of them.
type Options struct {
	DataDir   string
	AdminAddr string
	Version   string
	Commit    string
}

// Loaded is everything `check`, `explain` and `serve` need, loaded and
// validated identically so that a passing `check` genuinely predicts a
// successful start.
//
// Its fields stay unexported and are read through accessors. Nothing outside
// this package may assemble one: a Loaded that did not come from LoadBundle
// would not have been through ruleset.Compile, and every caller here assumes it
// has been.
type Loaded struct {
	cfg   *config.Config
	file  *ruleset.File
	rules *ruleset.Ruleset

	// source names where the rules came from, and legacy reports whether they
	// were transpiled from the Haraka format.
	source string
	legacy bool
}

// Config is the validated configuration.
func (l *Loaded) Config() *config.Config { return l.cfg }

// Ruleset is the compiled rule set.
func (l *Loaded) Ruleset() *ruleset.Ruleset { return l.rules }

// RulesSource names where the rules came from, for `check` output and log lines.
func (l *Loaded) RulesSource() string {
	if l.legacy {
		return l.source + " (Haraka format, transpiled)"
	}
	return l.source
}

// Unpopulated lists fields the schema accepts but this build never fills in.
//
// A rule referencing one is not an error — the field is real and will work —
// but it cannot match yet, and a rule that silently never fires is exactly the
// failure mode the schema registry exists to prevent.
//
// The auth.* pair left this map in M13, which is what "inbound AUTH is done"
// means in practice. mail.requiretls stays: advertising REQUIRETLS is a promise
// about outbound delivery this gateway does not keep, since relay TLS is
// per-relay policy and opportunistic is explicitly unauthenticated — so saying
// so here is the honest answer rather than an oversight.
var Unpopulated = map[string]string{
	"mail.requiretls": "REQUIRETLS is not advertised, so a client cannot ask for it",
}

// LoadFor resolves the configuration the way serve does, so `check` and
// `explain` answer questions about what this gateway is actually running.
//
// There is one source and therefore one branch. This used to fork on whether
// -config or -data had been typed, which meant a bare `check` read a directory
// the running gateway did not use — and on a node upgraded from file mode, a
// stale one that still existed, reporting a valid configuration and an empty
// queue while the live spool was elsewhere.
//
// The returned func releases the store handle and is never nil.
func LoadFor(o Options) (*Loaded, *store.CachedConfig, func(), error) {
	noop := func() {}

	// Note this is not a read-only open: it creates the directory and the
	// database and runs migrations. Running it as a different user than the
	// gateway can leave stray root-owned WAL files behind.
	st, err := store.Open(o.DataDir)
	if err != nil {
		return nil, nil, noop, err
	}
	release := func() { _ = st.Close() }

	cached := bootConfig(st, newLogger(config.LogConfig{}))
	if cached == nil {
		release()
		return nil, nil, noop, fmt.Errorf("%s: no configuration has been cached yet", o.DataDir)
	}

	l, err := loadBundle(cached.Bundle,
		config.BundleOptions{SpoolDir: SpoolFallback(o.DataDir)},
		BundleSource(cached))
	if err != nil {
		release()
		return nil, nil, noop, err
	}
	return l, cached, release, nil
}

// CachedBundle is the unparsed twin of LoadFor: the bundle this gateway would
// boot from, without compiling it.
//
// `config show` needs exactly this. It must print a bundle that does NOT parse
// — that is the case an operator runs it in — so it cannot go through LoadFor,
// which fails closed on a bundle the compiler rejects.
//
// The returned func releases the store handle and is never nil.
func CachedBundle(o Options) (*store.CachedConfig, func(), error) {
	noop := func() {}

	st, err := store.Open(o.DataDir)
	if err != nil {
		return nil, noop, err
	}
	release := func() { _ = st.Close() }

	cached := bootConfig(st, newLogger(config.LogConfig{}))
	if cached == nil {
		release()
		return nil, noop, fmt.Errorf("%s: no configuration has been cached yet", o.DataDir)
	}
	return cached, release, nil
}

// SpoolFallback is the spool directory a managed node uses when its server
// profile does not choose one.
//
// It is a function, and the only one, because M18 found the two configuration
// modes resolving this differently: every shipped edge node bind-mounted
// /opt/mailgw-go/queue while the real spool sat inside the 0700 identity
// directory. One expression, one answer, every caller.
func SpoolFallback(dataDir string) string { return filepath.Join(dataDir, "queue") }

// SpoolDir finds the queue the same way the running gateway would.
//
// The answer comes from the cached bundle rather than from <data>/queue,
// because an operator who set outbound.spool_dir in a server profile would
// otherwise be shown an empty queue and conclude their mail had vanished.
//
// A gateway with nothing deployed gets the fallback rather than an error: the
// gateway itself would use the same directory, and an empty queue there is the
// truthful answer.
func SpoolDir(o Options) (string, error) {
	st, err := store.Open(o.DataDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()

	fallback := SpoolFallback(o.DataDir)
	cached := bootConfig(st, newLogger(config.LogConfig{}))
	if cached == nil {
		return fallback, nil
	}

	l, err := loadBundle(cached.Bundle, config.BundleOptions{SpoolDir: fallback}, BundleSource(cached))
	if err != nil {
		return "", err
	}
	return l.cfg.Server.Outbound.SpoolDir, nil
}

// validateAgainstLiveRelays recompiles the new rules against the relay table
// currently in force, so a rule can never name a group the runner cannot dial.
func validateAgainstLiveRelays(next *Loaded, live *config.Config) error {
	_, err := ruleset.Compile(next.file, live.Relays, ruleset.DefaultSchema())
	return err
}

func newLogger(cfg config.LogConfig) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
