// Command mailgw-go is an SMTP relay gateway: it accepts inbound mail from
// allowlisted peers, applies a declarative rule set to every recipient, and
// forwards each to its relay group while posting audit events to the logservice
// API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/smtpsrv"
)

// Set via -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
)

const usage = `usage: mailgw-go <command> [flags]

commands:
  serve             run the gateway (default)
  check             validate the configuration; non-zero exit on error
  explain           show which rule wins for a given envelope
  convert-routing   transpile a Haraka routing.json into routing.yaml
  fields            list the fields rules can match on

`

func main() {
	// A leading subcommand is optional; the default action is to serve.
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		os.Exit(runServe(mustParse("serve", args, nil)))
	case "check":
		os.Exit(runCheck(mustParse("check", args, nil)))
	case "explain":
		os.Exit(runExplain(args))
	case "convert-routing":
		os.Exit(runConvert(args))
	case "fields":
		os.Exit(runFields())
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

// mustParse handles the shared -config and -version flags.
func mustParse(name string, args []string, extra func(*flag.FlagSet)) string {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	configDir := fs.String("config", "/opt/mailgw-go/config", "configuration directory")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if extra != nil {
		extra(fs)
	}
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Printf("mailgw-go %s (%s)\n", version, commit)
		os.Exit(0)
	}
	return *configDir
}

// loaded is everything `check`, `explain` and `serve` need, loaded and
// validated identically so that a passing `check` genuinely predicts a
// successful start.
type loaded struct {
	cfg   *config.Config
	file  *ruleset.File
	rules *ruleset.Ruleset

	// source names the file the rules came from, and legacy reports whether
	// they were transpiled from the Haraka format.
	source string
	legacy bool
}

func load(dir string) (*loaded, error) {
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	l := &loaded{cfg: cfg}

	// routing.yaml wins when present. Without it the Haraka routing.json is
	// transpiled into exactly the same compiled ruleset, so an existing
	// deployment keeps working untouched and its behaviour stays diffable.
	yamlPath := filepath.Join(dir, config.FileRouting)
	if _, statErr := os.Stat(yamlPath); statErr == nil {
		if l.file, err = ruleset.LoadFile(yamlPath); err != nil {
			return nil, err
		}
		l.source = yamlPath
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("read %s: %w", yamlPath, statErr)
	} else {
		jsonPath := filepath.Join(dir, config.FileRoutingJS)
		table, err := ruleset.LoadLegacyRouting(jsonPath)
		if err != nil {
			return nil, err
		}
		if err := table.Validate(cfg.Relays); err != nil {
			return nil, err
		}
		l.file, l.source, l.legacy = table.Transpile(), jsonPath, true
	}

	if l.rules, err = ruleset.Compile(l.file, cfg.Relays, ruleset.DefaultSchema()); err != nil {
		return nil, err
	}
	return l, nil
}

// unpopulated lists fields the schema accepts but this build never fills in.
//
// A rule referencing one is not an error — the field is real and will work —
// but it cannot match yet, and a rule that silently never fires is exactly the
// failure mode the schema registry exists to prevent.
var unpopulated = map[string]string{
	"attachment.filename":     "attachment scanning lands in M6",
	"attachment.content_type": "attachment scanning lands in M6",
	"attachment.md5":          "attachment scanning lands in M6",
	"attachment.size":         "attachment scanning lands in M6",
	"attachment.disposition":  "attachment scanning lands in M6",
	"msg.has_attachment":      "attachment scanning lands in M6",
	"msg.mime_part_count":     "the MIME walk lands in M6",
	"auth.user":               "inbound AUTH is not implemented",
	"auth.mechanism":          "inbound AUTH is not implemented",
}

// runCheck validates the configuration and exits non-zero if it is unusable.
func runCheck(dir string) int {
	l, err := load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	cfg := l.cfg

	fmt.Printf("config OK: %s\n", dir)
	fmt.Printf("  hostname:   %s\n", cfg.Server.Hostname)
	fmt.Printf("  listen:     %v\n", listenAddrs(cfg))
	fmt.Printf("  allowlist:  %s\n", cfg.Allowlist)
	fmt.Printf("  relays:     %v\n", cfg.Relays.Names())
	fmt.Printf("  rules:      %s\n", rulesSource(l))
	fmt.Printf("  policy:     %d rule(s)\n", len(l.rules.Policy))
	fmt.Printf("  routes:     %d rule(s) -> %v\n", len(l.rules.Routes), l.rules.Groups())
	fmt.Printf("  default:    %s\n", l.rules.Default)

	for _, r := range l.rules.Routes {
		scope := "message"
		if r.RcptScoped {
			scope = "recipient"
		}
		fmt.Printf("    %-32s prio=%-5d stage=%-7s %s\n", r.Name, r.Priority, r.Stage, scope)
	}

	for _, f := range l.rules.FieldsUsed() {
		if why, dead := unpopulated[f]; dead {
			fmt.Fprintf(os.Stderr, "  WARNING: a rule matches on %q, which is never populated yet (%s) — it cannot match\n", f, why)
		}
	}
	if cfg.Allowlist.AllowAll() {
		fmt.Fprintln(os.Stderr, "  WARNING: allow_all is set — this accepts mail from any peer")
	}
	if creds := cfg.Relays.PlaintextCredentials(); len(creds) > 0 {
		fmt.Fprintf(os.Stderr, "  WARNING: plaintext auth_pass in relays.json for %v (prefer auth_pass_env)\n", creds)
	}
	return 0
}

func rulesSource(l *loaded) string {
	if l.legacy {
		return l.source + " (Haraka format, transpiled)"
	}
	return l.source
}

// runExplain answers "why would this message go there?" without sending it.
func runExplain(args []string) int {
	var ip, helo, from, rcpt, stage, eml string
	var tlsOn bool

	dir := mustParse("explain", args, func(fs *flag.FlagSet) {
		fs.StringVar(&ip, "ip", "127.0.0.1", "client IP address")
		fs.StringVar(&helo, "helo", "client.invalid", "EHLO name")
		fs.StringVar(&from, "from", "", "envelope sender (empty is a null sender)")
		fs.StringVar(&rcpt, "rcpt", "", "recipient to evaluate (required)")
		fs.StringVar(&stage, "stage", "data", "stage to evaluate at: connect|helo|mail|rcpt|data")
		fs.StringVar(&eml, "eml", "", "message file, to populate the data-stage fields")
		fs.BoolVar(&tlsOn, "tls", false, "treat the session as encrypted")
	})

	if rcpt == "" {
		fmt.Fprintln(os.Stderr, "explain: -rcpt is required")
		return 2
	}
	at, err := ruleset.ParseStage(stage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "explain: %v\n", err)
		return 2
	}

	l, err := load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}

	env, err := buildEnv(ip, helo, from, rcpt, tlsOn, eml, at)
	if err != nil {
		fmt.Fprintf(os.Stderr, "explain: %v\n", err)
		return 2
	}

	l.rules.Explain(env, at).Print(os.Stdout)
	return 0
}

// runConvert transpiles a Haraka routing.json to routing.yaml on stdout.
func runConvert(args []string) int {
	fs := flag.NewFlagSet("convert-routing", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mailgw-go convert-routing <routing.json>  > routing.yaml")
		return 2
	}

	table, err := ruleset.LoadLegacyRouting(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert-routing: %v\n", err)
		return 1
	}
	out, err := table.Transpile().Marshal()
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert-routing: %v\n", err)
		return 1
	}

	fmt.Print("# Generated by `mailgw-go convert-routing` from ", fs.Arg(0), "\n")
	fmt.Print("# Equivalent to the Haraka table it came from; edit freely.\n")
	os.Stdout.Write(out)
	return 0
}

// runFields prints the field registry, so a rule author does not have to read
// the source to find out what can be matched on.
func runFields() int {
	sc := ruleset.DefaultSchema()
	fmt.Printf("%-10s %-26s %-12s %s\n", "STAGE", "FIELD", "TYPE", "DESCRIPTION")
	for _, d := range sc.Describe() {
		kind := d.Kind.String()
		if d.List {
			kind += " list"
		}
		note := d.Desc
		if why, dead := unpopulated[d.Name]; dead {
			note += fmt.Sprintf("  [not populated yet: %s]", why)
		}
		fmt.Printf("%-10s %-26s %-12s %s\n", d.Stage, d.Name, kind, note)
	}
	return 0
}

func listenAddrs(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Server.Listen))
	for _, l := range cfg.Server.Listen {
		out = append(out, l.Addr)
	}
	return out
}

func runServe(dir string) int {
	l, err := load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	cfg := l.cfg

	log := newLogger(cfg.Server.Log)
	slog.SetDefault(log)
	log.Info("starting", "version", version, "commit", commit, "config", dir,
		"rules", rulesSource(l), "policy_rules", len(l.rules.Policy), "route_rules", len(l.rules.Routes))

	if cfg.Allowlist.AllowAll() {
		log.Warn("allow_all is set — accepting mail from any peer")
	}
	if creds := cfg.Relays.PlaintextCredentials(); len(creds) > 0 {
		log.Warn("plaintext relay credentials in relays.json", "relays", creds)
	}
	for _, f := range l.rules.FieldsUsed() {
		if why, dead := unpopulated[f]; dead {
			log.Warn("a rule matches on a field this build never populates; it cannot match",
				"field", f, "reason", why)
		}
	}

	spool, err := queue.NewSpool(cfg.Server.Outbound.SpoolDir)
	if err != nil {
		log.Error("cannot open spool", "dir", cfg.Server.Outbound.SpoolDir, "err", err)
		return 1
	}

	ev := events.New(events.Options{
		Timeout:    cfg.Server.Events.Timeout.D(),
		Retries:    cfg.Server.Events.Retries,
		BufferSize: cfg.Server.Events.BufferSize,
		Senders:    cfg.Server.Events.Senders,
		APIKey:     events.APIKeyFromEnv(cfg.Server.Events.APIKeyEnv),
		SpillDir:   spool.FailedEventsDir(),
		Logger:     log,
	})
	defer ev.Close()

	runner := queue.NewRunner(spool, queue.RunnerConfig{
		Relays:         cfg.Relays,
		PollInterval:   cfg.Server.Outbound.PollInterval.D(),
		Concurrency:    cfg.Server.Outbound.Concurrency,
		PerGroup:       cfg.Server.Outbound.PerGroupConnections,
		LocalName:      cfg.Server.Hostname,
		ConnectTimeout: cfg.Server.Outbound.ConnectTimeout.D(),
		DataTimeout:    cfg.Server.Outbound.DataTimeout.D(),
		Backoff:        cfg.Server.Outbound.BackoffFor,
		Jitter:         cfg.Server.Outbound.Jitter,
		MaxLifetime:    cfg.Server.Outbound.MaxLifetime.D(),
		DeliveryURL:    cfg.Logging.URLDelivery,
		Events:         ev,
		Log:            log,
	})

	// Both are swapped atomically on SIGHUP. A session reads each pointer once,
	// so a reload never takes effect halfway through a transaction.
	var allowlist atomic.Pointer[config.Allowlist]
	allowlist.Store(cfg.Allowlist)
	var rules atomic.Pointer[ruleset.Ruleset]
	rules.Store(l.rules)

	backend := &smtpsrv.Backend{
		Cfg:       cfg,
		Allowlist: allowlist.Load,
		Rules:     rules.Load,
		Spool:     spool,
		Events:    ev,
		Log:       log,
		OnQueued:  func([]*queue.Envelope) { runner.Nudge() },
	}

	srv, err := smtpsrv.NewServer(backend, &cfg.Server, log)
	if err != nil {
		log.Error("cannot build server", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runner.Start(ctx)
	go watchReload(ctx, dir, cfg, &allowlist, &rules, log)

	var listeners []net.Listener
	for _, lst := range cfg.Server.Listen {
		ln, err := net.Listen("tcp", lst.Addr)
		if err != nil {
			log.Error("cannot listen", "addr", lst.Addr, "err", err)
			return 1
		}
		guarded := smtpsrv.Guard(ln, allowlist.Load, log)
		listeners = append(listeners, guarded)

		go func(addr string, ln net.Listener) {
			log.Info("listening", "addr", addr)
			if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
				log.Error("serve failed", "addr", addr, "err", err)
			}
		}(lst.Addr, guarded)
	}

	<-ctx.Done()
	log.Info("shutting down")

	for _, ln := range listeners {
		_ = ln.Close()
	}
	// Give in-flight sessions a moment to finish before dropping them.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = srv.Shutdown(shutdownCtx) }()
	<-shutdownCtx.Done()

	return 0
}

// watchReload re-reads the allowlist and the rule set on SIGHUP.
//
// The reload is all-or-nothing: anything that fails to parse, resolve or
// type-check leaves the running configuration untouched. Half-applying a rule
// change would be worse than not applying it.
//
// fsnotify is deliberately not used. A config file being written is observed
// mid-write, and reloading a truncated rule file — even with the keep-on-error
// guarantee — means logging a spurious error every time an operator saves.
// SIGHUP says "I am done editing", which is the signal that actually matters.
func watchReload(
	ctx context.Context,
	dir string,
	live *config.Config,
	allowlist *atomic.Pointer[config.Allowlist],
	rules *atomic.Pointer[ruleset.Ruleset],
	log *slog.Logger,
) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			next, err := load(dir)
			if err != nil {
				log.Error("reload failed; keeping the running configuration", "err", err)
				continue
			}

			// Rules were validated against the relay table on disk, but the
			// delivery runner still holds the one it started with. Rather than
			// swap a relay table out from under in-flight deliveries, refuse
			// the reload and say so.
			if err := validateAgainstLiveRelays(next, live); err != nil {
				log.Error("reload rejected; restart to pick up relay changes", "err", err)
				continue
			}

			allowlist.Store(next.cfg.Allowlist)
			rules.Store(next.rules)
			log.Info("configuration reloaded",
				"allowlist_entries", next.cfg.Allowlist.Len(),
				"policy_rules", len(next.rules.Policy),
				"route_rules", len(next.rules.Routes))
		}
	}
}

// validateAgainstLiveRelays recompiles the new rules against the relay table
// currently in force, so a rule can never name a group the runner cannot dial.
func validateAgainstLiveRelays(next *loaded, live *config.Config) error {
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
