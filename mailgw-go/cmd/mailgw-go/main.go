// Command mailgw-go is an SMTP relay gateway: it accepts inbound mail from
// allowlisted peers, applies a declarative rule set to every recipient, and
// forwards each to its relay group while posting audit events to the logservice
// API.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/node"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
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

The gateway is centrally managed and has no other configuration source: it
stores its identity and its configuration cache under -data, is provisioned
through the admin UI, and is told everything else by Central Management. There
is no way to hand it a configuration from this host — no directory, no file, no
environment. check, explain, mailq and events all read the same cache.

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

// mustParse handles the shared -data, -admin and -version flags.
func mustParse(name string, args []string, extra func(*flag.FlagSet)) node.Options {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	// The build stamps are set here rather than read from a package variable in
	// internal/node: two binaries link that package and each sets its own via
	// -ldflags, so a var there could only ever carry one of them.
	o := node.Options{Version: version, Commit: commit}
	fs.StringVar(&o.DataDir, "data", "/var/lib/mailgw-go", "data directory: SQLite store and gateway identity")
	fs.StringVar(&o.AdminAddr, "admin", "0.0.0.0:8080", `admin UI bind address ("" disables it)`)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if extra != nil {
		extra(fs)
	}
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Printf("mailgw-go %s (%s)\n", version, commit)
		os.Exit(0)
	}
	return o
}

// inboundTLS describes what a peer connecting to this gateway would be offered.
//
// Worth a line of its own because the answer is not readable from the keys: a
// node with no cert or key still ends up serving TLS, from a pair it generates
// itself, and an operator reading `check` should not have to know that to work
// out why STARTTLS is or is not being advertised.
func inboundTLS(cfg *config.Config) string {
	t := cfg.Server.TLS
	switch {
	case !cfg.Server.WantsTLS():
		return " disabled (starttls: false and no implicit_tls listener)"
	case t.ConfiguredPublic():
		return fmt.Sprintf(" %s", t.Cert)
	default:
		return " a self-signed pair generated under the data directory"
	}
}

// runCheck validates the configuration and exits non-zero if it is unusable.
func runCheck(o node.Options) int {
	l, cached, release, err := node.LoadFor(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	defer release()
	cfg := l.Config()

	fmt.Printf("config OK: %s\n", node.BundleSource(cached))
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
	fmt.Printf("  hostname:   %s\n", cfg.Server.Hostname)
	fmt.Printf("  listen:     %v\n", listenAddrs(cfg))
	fmt.Printf("  inbound tls:%s\n", inboundTLS(cfg))
	fmt.Printf("  allowlist:  %s\n", cfg.Allowlist)
	fmt.Printf("  relays:     %v\n", cfg.Relays.Names())
	fmt.Printf("  rules:      %s\n", l.RulesSource())
	fmt.Printf("  policy:     %d rule(s)\n", len(l.Ruleset().Policy))
	fmt.Printf("  routes:     %d rule(s) -> %v\n", len(l.Ruleset().Routes), l.Ruleset().Groups())
	fmt.Printf("  default:    %s\n", l.Ruleset().Default)

	for _, r := range l.Ruleset().Routes {
		scope := "message"
		if r.RcptScoped {
			scope = "recipient"
		}
		fmt.Printf("    %-32s prio=%-5d stage=%-7s %s\n", r.Name, r.Priority, r.Stage, scope)
	}

	for _, f := range l.Ruleset().FieldsUsed() {
		if why, dead := node.Unpopulated[f]; dead {
			fmt.Fprintf(os.Stderr, "  WARNING: a rule matches on %q, which is never populated yet (%s) — it cannot match\n", f, why)
		}
	}
	if cfg.Allowlist.AllowAll() {
		fmt.Fprintln(os.Stderr, "  WARNING: allow_all is set — this accepts mail from any peer")
	}
	// Deliberately no "prefer auth_pass_env" advice any more: this gateway has
	// no environment, so following it would produce an EMPTY password and a
	// relay rejection that looks like a wrong credential. The field is refused
	// at load time; see relays.EnvCredentials.
	if creds := cfg.Relays.PlaintextCredentials(); len(creds) > 0 {
		fmt.Fprintf(os.Stderr, "  NOTE: %v carry a relay password; it is encrypted at rest in the console "+
			"and travels only inside this gateway's signed bundle\n", creds)
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
	reportMsgAuth(cfg, l.Ruleset())
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
	fmt.Fprintf(os.Stderr, "  dkim:     signing %v\n", node.SigningDomains(cfg))
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
	l, _, release, err := node.LoadFor(o)
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
		Inline:   l.Config().Server.Attach.IncludeInline,
		AuthUser: authUser, AuthMech: authMech,
		SPF: spfRes, DKIM: dkimRes, DMARC: dmarcRes, DMARCPolicy: dmarcPolicy,
		Stage: at,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "explain: %v\n", err)
		return 2
	}

	l.Ruleset().Explain(env, at).Print(os.Stdout)
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
		if why, dead := node.Unpopulated[d.Name]; dead {
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

// runServe runs the gateway. There is one way to configure it.
//
// This used to pick between a file-mode path and a managed one. The second
// source is gone: the configuration comes from Central Management into the
// local SQLite cache, and nothing on this host can supply one.
func runServe(o node.Options) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return node.Serve(ctx, o)
}
