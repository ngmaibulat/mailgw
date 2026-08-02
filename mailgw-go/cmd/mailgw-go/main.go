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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
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
  config show       print the cached configuration bundle, secrets redacted
  claim             show or reset the admin UI's claim code
  mailq             inspect and manage the outbound queue
  events            inspect and replay audit events logservice would not take
  convert-routing   transpile a Haraka routing.json into routing.yaml
  fields            list the fields rules can match on

With no -config, the gateway is centrally managed: it stores its identity and
its configuration cache under -data and is provisioned through the admin UI.
Pass -config <dir> to run from files instead. check and explain read whichever
of the two you name; with neither they read the default config directory.

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
	case "config":
		os.Exit(runConfig(args))
	case "claim":
		os.Exit(runClaim(args))
	case "mailq":
		os.Exit(runMailq(args))
	case "events":
		os.Exit(runEvents(args))
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

// opts is the shared flag set's result.
//
// Only `serve` reads more than configDir; `check` and `explain` still take a
// plain directory, so their behaviour is untouched. configSet/adminSet record
// whether the flag actually appeared on the command line, which is what
// distinguishes "file mode" from "managed mode" — a default value cannot say
// that, and `check`/`explain` must keep their /opt/mailgw-go/config default.
type opts struct {
	configDir string
	configSet bool
	dataDir   string
	dataSet   bool
	adminAddr string
	adminSet  bool
}

// mustParse handles the shared -config, -data, -admin and -version flags.
func mustParse(name string, args []string, extra func(*flag.FlagSet)) opts {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var o opts
	fs.StringVar(&o.configDir, "config", "/opt/mailgw-go/config", "configuration directory")
	fs.StringVar(&o.dataDir, "data", "/var/lib/mailgw-go", "data directory: SQLite store and gateway identity")
	fs.StringVar(&o.adminAddr, "admin", "0.0.0.0:8080", `admin UI bind address ("" disables it)`)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if extra != nil {
		extra(fs)
	}
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Printf("mailgw-go %s (%s)\n", version, commit)
		os.Exit(0)
	}

	// flag has no "was this set?"; Visit walks only the flags actually given.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "config":
			o.configSet = true
		case "data":
			o.dataSet = true
		case "admin":
			o.adminSet = true
		}
	})
	return o
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
//
// The auth.* pair left this map in M13, which is what "inbound AUTH is done"
// means in practice. mail.requiretls stays: advertising REQUIRETLS is a promise
// about outbound delivery this gateway does not keep, since relay TLS is
// per-relay policy and opportunistic is explicitly unauthenticated — so saying
// so here is the honest answer rather than an oversight.
var unpopulated = map[string]string{
	"mail.requiretls": "REQUIRETLS is not advertised, so a client cannot ask for it",
}

// inboundTLS describes what a peer connecting to this gateway would be offered.
//
// Worth a line of its own because the answer is not readable from the keys: a
// managed node with no cert or key still ends up serving TLS, from a pair it
// generates itself, and an operator reading `check` should not have to know that
// to work out why STARTTLS is or is not being advertised.
func inboundTLS(cfg *config.Config) string {
	t := cfg.Server.TLS
	switch {
	case !cfg.Server.WantsTLS():
		return " disabled (starttls: false and no implicit_tls listener)"
	case t.ConfiguredPublic():
		return fmt.Sprintf(" %s", t.Cert)
	default:
		return " a self-signed pair under the data directory (managed mode); none in file mode"
	}
}

// loadFor resolves the configuration the way serve does, so `check` and
// `explain` answer questions about what this gateway is actually running rather
// than about a directory it may not use.
//
// An explicit -config is file mode. An explicit -data reads the SQLite cache a
// managed gateway fills. Neither keeps the historical /opt/mailgw-go/config
// default, so a bare `check` is unchanged.
//
// The returned func releases the store handle and is never nil.
func loadFor(o opts) (*loaded, *store.CachedConfig, func(), error) {
	noop := func() {}

	if o.configSet || !o.dataSet {
		l, err := load(o.configDir)
		return l, nil, noop, err
	}

	// Note this is not a read-only open: it creates the directory and the
	// database and runs migrations. Running it as a different user than the
	// gateway can leave stray root-owned WAL files behind.
	st, err := store.Open(o.dataDir)
	if err != nil {
		return nil, nil, noop, err
	}
	release := func() { _ = st.Close() }

	cached := bootConfig(st, newLogger(config.LogConfig{}))
	if cached == nil {
		release()
		return nil, nil, noop, fmt.Errorf("%s: no configuration has been cached yet", o.dataDir)
	}

	l, err := loadBundle(cached.Bundle,
		config.BundleOptions{SpoolDir: filepath.Join(o.dataDir, "queue")},
		bundleSource(cached))
	if err != nil {
		release()
		return nil, nil, noop, err
	}
	return l, cached, release, nil
}

// runCheck validates the configuration and exits non-zero if it is unusable.
func runCheck(o opts) int {
	l, cached, release, err := loadFor(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	defer release()
	cfg := l.cfg

	if cached != nil {
		fmt.Printf("config OK: %s\n", bundleSource(cached))
		fmt.Printf("  digest:     %s\n", cached.SHA256)
		fmt.Printf("  fetched:    %s\n", cached.FetchedAt.Format(time.RFC3339))
		if cached.AppliedAt != nil {
			fmt.Printf("  applied:    %s\n", cached.AppliedAt.Format(time.RFC3339))
		} else {
			fmt.Printf("  applied:    never\n")
		}
		if cached.ApplyError != "" {
			fmt.Printf("  last error: %s\n", cached.ApplyError)
		}
		// The two paths deliberately differ here, so say which one ran: a
		// server profile with an unknown key is rejected from the console and
		// tolerated from disk.
		fmt.Printf("  parsed:     strictly (an unknown key in the server profile is rejected)\n")
	} else {
		fmt.Printf("config OK: %s\n", cfg.Dir)
	}
	fmt.Printf("  hostname:   %s\n", cfg.Server.Hostname)
	fmt.Printf("  listen:     %v\n", listenAddrs(cfg))
	fmt.Printf("  inbound tls:%s\n", inboundTLS(cfg))
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
	if mx := cfg.Relays.AuthenticatedMX(); len(mx) > 0 {
		fmt.Fprintf(os.Stderr, "  WARNING: %v send credentials to MX-resolved hosts; the DNS decides who receives them\n", mx)
	}
	if d := cfg.Server.DSN; d.Enabled {
		fmt.Fprintf(os.Stderr, "  bounces:  from %s, returning %s\n",
			d.PostmasterFor(cfg.Server.Hostname), returnMode(d))
		if d.RelayGroup == "" {
			fmt.Fprintln(os.Stderr, "  NOTE: no dsn.relay_group; a bounce no route rule claims cannot be sent")
		}
	}
	reportMsgAuth(cfg, l.rules)
	reportRateLimits(cfg)
	return 0
}

// reportRateLimits prints what is throttled, and says out loud that it is not.
//
// The "nothing is throttled" line is the point of this: every limit ships off,
// so an operator who believes they configured one and typed the key wrong would
// otherwise see no output at all and conclude it worked.
func reportRateLimits(cfg *config.Config) {
	on := cfg.Server.RateLimit.Enabled()
	if len(on) == 0 {
		fmt.Fprintln(os.Stderr, "  ratelimit: off — nothing is throttled")
		return
	}
	fmt.Fprintf(os.Stderr, "  ratelimit: %s\n", strings.Join(on, ", "))
	fmt.Fprintln(os.Stderr, "  NOTE: rate limits are per gateway and in memory, "+
		"so a fleet of N nodes admits N times these numbers; refusals are 4xx, "+
		"never permanent")
}

// reportMsgAuth prints what message authentication is doing, and why.
//
// It reports the checks the RULES turn on as well as the ones the configuration
// does, because those are the ones an operator is most likely to be surprised
// by: writing a rule on spf.result switches SPF on for every message with no
// msgauth: block anywhere in the file.
func reportMsgAuth(cfg *config.Config, rules *ruleset.Ruleset) {
	m := cfg.Server.MsgAuth

	var on []string
	name := func(enabled, needed bool, label string) {
		switch {
		case enabled:
			on = append(on, label)
		case needed:
			on = append(on, label+" (turned on by a rule)")
		}
	}
	name(m.SPF.Enabled, rules.NeedsSPF(), "spf")
	name(m.DKIM.Enabled, rules.NeedsDKIM(), "dkim")
	name(m.DMARC.Enabled, rules.NeedsDMARC(), "dmarc")

	if len(on) > 0 {
		fmt.Fprintf(os.Stderr, "  msgauth:  verifying %s as %q\n",
			strings.Join(on, ", "), m.AuthservIDFor(cfg.Server.Hostname))
	}

	if !m.Sign.Enabled {
		if len(on) == 0 {
			fmt.Fprintln(os.Stderr, "  msgauth:  off — nothing verified, nothing signed")
		}
		return
	}
	// Every key was already read and parsed by validateDKIM, so reaching here
	// means they are all usable; printing them is what tells an operator which
	// selectors have to exist in DNS.
	fmt.Fprintf(os.Stderr, "  dkim:     signing %v\n", signingDomains(cfg))
	fmt.Fprintln(os.Stderr, "  NOTE: the signing key is chosen by each message's From header domain; "+
		"mail from any other domain goes out unsigned")
}

// returnMode names how much of the original a bounce quotes, for `check`.
func returnMode(d config.DSNConfig) string {
	if d.FullReturn() {
		return "the full message"
	}
	return "headers only"
}

func rulesSource(l *loaded) string {
	if l.legacy {
		return l.source + " (Haraka format, transpiled)"
	}
	return l.source
}

// runExplain answers "why would this message go there?" without sending it.
func runExplain(args []string) int {
	var ip, helo, from, rcpt, stage, eml, authUser, authMech string
	var spfRes, dkimRes, dmarcRes, dmarcPolicy string
	var tlsOn bool

	o := mustParse("explain", args, func(fs *flag.FlagSet) {
		fs.StringVar(&ip, "ip", "127.0.0.1", "client IP address")
		fs.StringVar(&helo, "helo", "client.invalid", "EHLO name")
		fs.StringVar(&from, "from", "", "envelope sender (empty is a null sender)")
		fs.StringVar(&rcpt, "rcpt", "", "recipient to evaluate (required)")
		fs.StringVar(&stage, "stage", "data", "stage to evaluate at: connect|helo|mail|rcpt|data")
		fs.StringVar(&eml, "eml", "", "message file, to populate the data-stage fields")
		fs.BoolVar(&tlsOn, "tls", false, "treat the session as encrypted")
		fs.StringVar(&authUser, "auth-user", "", "treat the session as authenticated as this user")
		fs.StringVar(&authMech, "auth-mech", "PLAIN", "SASL mechanism to report, with -auth-user")
		// Flags rather than live lookups, exactly as -tls and -auth-user are:
		// the question worth answering is "what would my rules do if DMARC
		// failed", and resolving for real would make explain depend on the
		// network and on somebody else's DNS. Empty means the check did not run.
		fs.StringVar(&spfRes, "spf", "", "SPF result to assume: pass|fail|softfail|neutral|none|temperror|permerror")
		fs.StringVar(&dkimRes, "dkim", "", "DKIM result to assume: pass|fail|none|temperror|permerror")
		fs.StringVar(&dmarcRes, "dmarc", "", "DMARC result to assume: pass|fail|none|temperror|permerror")
		fs.StringVar(&dmarcPolicy, "dmarc-policy", "", "policy the From domain publishes, with -dmarc: none|quarantine|reject")
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

	// Answers against the cached bundle too: "why would this message go there?"
	// is the most useful question in the product, and a managed gateway has no
	// configuration directory to point it at.
	l, _, release, err := loadFor(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	defer release()

	// include_inline comes from the configuration being explained, not a flag:
	// the answer to "which parts count as attachments here?" is a property of
	// this gateway, and a flag would let explain disagree with it.
	// -auth-mech defaults to PLAIN so the common case is one flag, but it must
	// not describe an authenticated session when nobody said there was one.
	if authUser == "" {
		authMech = ""
	}
	env, err := buildEnv(explainOpts{
		IP: ip, Helo: helo, From: from, Rcpt: rcpt, TLS: tlsOn, EML: eml,
		Inline:   l.cfg.Server.Attach.IncludeInline,
		AuthUser: authUser, AuthMech: authMech,
		SPF: spfRes, DKIM: dkimRes, DMARC: dmarcRes, DMARCPolicy: dmarcPolicy,
		Stage: at,
	})
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

// runServe picks the configuration source.
//
// An explicitly-given -config means file mode: today's behaviour, byte for
// byte, which is what keeps check/explain/testdata/CI and the Bun SMTP suite
// working. Anything else is managed mode, where the configuration comes from
// Central Management into the local SQLite cache.
//
// Both paths now converge on one *gateway, brought up from a *loaded — the
// difference between them is only where those bytes came from, and when.
func runServe(o opts) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if o.configSet {
		return serveFile(ctx, o)
	}
	return serveManaged(ctx, o)
}

// serveManaged runs a gateway whose configuration comes from Central
// Management.
//
// It serves mail as soon as a cached bundle applies and not before. A gateway
// with no allowlist would deny every peer anyway (the allowlist zero value
// denies), and a listener that can only reject is worse than no listener: it
// looks healthy to a load balancer. Fail closed, and say so on the status page.
//
// Nothing here blocks on reaching the console. Booting from the cache with
// Central Management down is the entire reason the cache exists.
func serveManaged(ctx context.Context, o opts) int {
	// There is no server.yaml until a bundle applies, so logging takes its
	// defaults. Unlike file mode the logger is NOT rebuilt at bring-up: the
	// admin UI and the agent already hold this one, and leaving them on a
	// stale logger to honour a log-level change is the worse trade. A bundle
	// that changes it reports "log" in restart_required instead.
	log := newLogger(config.LogConfig{})
	slog.SetDefault(log)

	if o.adminAddr == "" {
		fmt.Fprintln(os.Stderr,
			"managed mode has no way to be provisioned without an admin UI:\n"+
				"  pass -config <dir> to run from files, or give -admin an address")
		return 2
	}

	st, err := store.Open(o.dataDir)
	if err != nil {
		log.Error("cannot open the data directory", "dir", o.dataDir, "err", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	id, err := st.Identity()
	if err != nil {
		log.Error("cannot establish a gateway identity", "err", err)
		return 1
	}

	log.Info("starting in managed mode", "version", version, "commit", commit,
		"data", o.dataDir, "fingerprint", id.Fingerprint, "admin", o.adminAddr)

	// The admin UI is the only way to provision this node, so it needs a
	// credential before it starts listening — not after.
	//
	// The code is logged on every boot until somebody uses it, because an
	// operator who restarts the container before claiming it would otherwise
	// have to go looking for it; and it stops being logged afterwards, because
	// by then it is a live credential rather than a first-boot notice. A node
	// upgraded from before M12 arrives here unclaimed and prints one once,
	// which is the deliberate one-time step in that upgrade.
	claim, err := st.EnsureClaimCode()
	if err != nil {
		log.Error("cannot establish an admin claim code", "err", err)
		return 1
	}
	if !claim.Claimed {
		log.Warn("admin UI is unclaimed — enter this code in the wizard to take control",
			"code", claim.Code,
			"reset", fmt.Sprintf("mailgw-go claim reset -data %s", o.dataDir))
	}

	g := newGateway(log)
	defer g.shutdown()
	// The uid is this node's identity in the console, and it is what labels the
	// audit rows it writes. Empty until registration completes, which is why
	// the label is resolved at bring-up rather than here.
	g.uid = id.GatewayUID
	// A managed node has no operator-supplied files, so a keypair it generates
	// itself is the only way inbound TLS can exist here. Beside the identity key
	// on the same durable volume, and the place to drop a real certificate.
	g.tlsDir = filepath.Join(o.dataDir, "tls")

	a := newAgent(st, version, log)
	a.gw = g
	a.metrics = g.Metrics()
	// A bundle whose server profile chooses no spool directory must not land on
	// the compiled-in /opt/mailgw-go/queue, which a managed node cannot write.
	a.bundleOpts = config.BundleOptions{SpoolDir: filepath.Join(o.dataDir, "queue")}

	ui := &adminui.Server{
		Store:        st,
		Version:      version,
		Commit:       commit,
		Log:          log,
		SpoolFn:      g.Spool,
		Register:     a.Register,
		State:        a.State,
		Metrics:      g.Metrics(),
		MetricsToken: g.AdminToken,
		Gauges: func() obs.Gauges {
			st := a.State()
			gg := obs.Gauges{
				Managed:  true,
				Approved: st.Approval == approvalApproved,
				Serving:  st.Serving,
			}
			if st.AppliedVersion != nil {
				gg.ConfigVersion = *st.AppliedVersion
			}
			// Nil until the first configuration applies, which leaves QueueOK
			// false and omits the queue series rather than reporting zero.
			adminui.SpoolGauges(&gg, g.Spool())
			return gg
		},
	}
	// The UI is how an unprovisioned gateway gets provisioned, so it comes up
	// first and a bind failure is fatal — without it this process can never
	// become useful.
	adminErr := make(chan error, 1)
	go func() { adminErr <- ui.ListenAndServe(ctx, o.adminAddr) }()

	// Boot from the cache. A failure is logged rather than fatal: the pull loop
	// may be handed a working configuration within the next poll.
	bootFromCache(ctx, a, st, log)

	go a.Run(ctx)
	go a.Watch(ctx)
	go g.watchSignals(ctx, a.applyFromCache)

	select {
	case <-ctx.Done():
	case err := <-adminErr:
		if err != nil {
			log.Error("admin UI failed", "addr", o.adminAddr, "err", err)
			return 1
		}
	}
	log.Info("shutting down")
	return 0
}

func serveFile(ctx context.Context, o opts) int {
	dir := o.configDir
	l, err := load(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}

	log := newLogger(l.cfg.Server.Log)
	slog.SetDefault(log)
	log.Info("starting", "version", version, "commit", commit, "config", dir,
		"rules", rulesSource(l), "policy_rules", len(l.rules.Policy), "route_rules", len(l.rules.Routes))

	g := newGateway(log)
	if _, err := g.apply(ctx, l); err != nil {
		return 1
	}
	defer g.shutdown()

	go g.watchSignals(ctx, func(ctx context.Context) error { return g.reloadDir(ctx, dir) })

	// The admin UI is opt-in in file mode: this gateway already has its
	// configuration, so the UI is a read-only status page. A failed admin
	// listener is worth an error but must not stop a gateway whose actual job
	// is relaying mail.
	if o.adminSet && o.adminAddr != "" {
		ui := &adminui.Server{
			Version:      version,
			Commit:       commit,
			ConfigDir:    dir,
			SpoolFn:      g.Spool,
			Log:          log,
			Metrics:      g.Metrics(),
			MetricsToken: g.AdminToken,
			// A file-mode gateway owns its configuration, so readiness is only
			// "are the listeners up?" — but it is wired to the real source
			// rather than hardcoded, so the answer stays honest.
			State: func() adminui.State { return adminui.State{Serving: g.Serving()} },
			Gauges: func() obs.Gauges {
				gg := obs.Gauges{Serving: g.Serving()}
				adminui.SpoolGauges(&gg, g.Spool())
				return gg
			},
		}
		go func() {
			if err := ui.ListenAndServe(ctx, o.adminAddr); err != nil {
				log.Error("admin UI failed", "addr", o.adminAddr, "err", err)
			}
		}()
	}

	<-ctx.Done()
	log.Info("shutting down")
	return 0
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
