package node

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/attach"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ratelimit"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/smtpsrv"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/tlsx"
)

// gateway owns everything that exists exactly once per process: the spool, the
// event pipeline, the delivery runner, the SMTP server and its listeners, and
// the two atomic pointers a session reads.
//
// It is brought up from a *Loaded exactly once, and every apply after that
// swaps the allowlist and the compiled rules and nothing else. The runner holds
// its relay table for the life of the process, and the listeners, the TLS
// keypair and the spool are bound at start; what changed but cannot be applied
// is reported rather than half-applied (see restartRequired).
//
// This is the same all-or-nothing contract SIGHUP reload has always had. The
// only new thing in M5 is that a second caller — the config pull loop — now
// uses it, from a different goroutine, which is what mu is for.
type gateway struct {
	log *slog.Logger

	// metrics outlives every apply, so a counter is not reset by a redeploy —
	// and it exists before the first bundle, which is when /metrics is first
	// scraped on a managed node.
	metrics *obs.Metrics
	// gatewayLabel is what the `gateway` column in the log tables holds,
	// resolved at bring-up from uid and the applied configuration.
	gatewayLabel string
	// uid is this gateway's Central Management identity, empty in file mode.
	// Set before the first apply so the label can prefer it.
	uid string
	// tlsDir is where a self-signed keypair is generated when the configuration
	// names none. Empty in file mode: a gateway run from a directory has an
	// operator who can put a certificate beside its configuration, and inventing
	// files next to somebody's config directory is not this program's business.
	tlsDir string

	// mu serialises apply against apply. The pull loop and the signal watcher
	// are two goroutines and both call it.
	mu sync.Mutex
	// live is the configuration this process actually started with. Nil until
	// the first apply, which is also how started() answers.
	live *config.Config
	// restart is what the most recent apply changed that a restart is needed
	// to pick up, for the status page.
	restart []string

	// Both are swapped atomically. A session reads each pointer once, so an
	// apply never takes effect halfway through a transaction.
	allowlist atomic.Pointer[config.Allowlist]
	rules     atomic.Pointer[ruleset.Ruleset]
	// spool is a pointer rather than a field because the admin UI reads it from
	// a request goroutine while a managed gateway's first apply creates it.
	spool atomic.Pointer[queue.Spool]
	// adminToken is the bearer token /metrics and /readyz require, read on
	// every scrape from the admin listener's goroutine.
	//
	// An atomic rather than a read of live: the admin listener is answering
	// requests before the first apply and throughout every later one, and
	// taking mu on a scrape would put /metrics behind whatever an apply is
	// doing — the same reasoning as bounceRouter's comment above.
	adminToken atomic.Pointer[string]
	// authUsers is the inbound SMTP AUTH credential set, read from a session
	// goroutine on every AUTH command.
	//
	// The third category alongside adminToken: neither hot-swapped through
	// swap() nor requiring a restart, so it is deliberately absent from
	// restartRequired. Deploying a bundle that adds or revokes a credential
	// takes effect on the next AUTH command, which is what an operator
	// revoking one expects and what a restart would make unnecessarily
	// disruptive. tls.allow_insecure_auth is the opposite — it is read once
	// when the go-smtp server is built — and restartRequired's `tls` entry
	// already covers it.
	authUsers atomic.Pointer[config.Auth]

	// limiter holds the rate-limit buckets. Created ONCE, at gateway
	// construction, and retuned in place by every apply.
	//
	// The third category again, and the reason is stated in M15's plan: a limit
	// an operator cannot adjust without restarting a mail server during an
	// incident is a limit they will not use. But note what is NOT done here —
	// the limiter is not replaced on apply, only its rules are. Replacing it
	// would hand every peer a fresh allowance whenever an unrelated
	// configuration change was deployed, so an attacker under pressure would be
	// released by a routine deploy.
	//
	// A plain field rather than an atomic.Pointer: the pointer never changes, so
	// there is nothing to publish. Limiter has its own mutex.
	limiter *ratelimit.Limiter

	events  *events.Client
	runner  *queue.Runner
	bouncer *queue.Bouncer
	// pool is nil unless outbound.reuse_connections is on; shutdown closes it.
	pool *deliver.Pool
	// runnerCancel stops the delivery loop and runnerDone reports that it has
	// actually stopped. Held explicitly rather than relying on the serve context
	// having already been cancelled: shutdown orders the teardown itself, and
	// "the signal handler probably did it" is not an ordering.
	runnerCancel context.CancelFunc
	runnerDone   chan struct{}
	// replayCancel stops the failed-events replay loop. It must be stopped
	// BEFORE the event pipeline closes: both write to failed-events/, and the
	// pipeline's final drain spills into the directory the replayer is draining.
	replayCancel context.CancelFunc
	replayDone   chan struct{}
	srv          *smtp.Server
	listeners    smtpListeners
}

// newBouncer builds the delivery-status-notification generator shared by the
// delivery runner and the SMTP session.
//
// The routing callback is the reason this lives here rather than in the queue
// package: it closes over g.rules, so a bounce is routed by the same rule engine
// as ordinary mail and picks up every hot-swapped configuration without being
// rebuilt. Nothing else in internal/queue knows the ruleset exists.
func (g *gateway) newBouncer(spool *queue.Spool, cfg *config.Config) *queue.Bouncer {
	return &queue.Bouncer{
		Spool:          spool,
		Enabled:        cfg.Server.DSN.Enabled,
		Full:           cfg.Server.DSN.FullReturn(),
		MaxReturnBytes: cfg.Server.DSN.MaxReturnBytes,
		Postmaster:     cfg.Server.DSN.PostmasterFor(cfg.Server.Hostname),
		Hostname:       cfg.Server.Hostname,
		RelayGroup:     cfg.Server.DSN.RelayGroup,
		Route:          g.bounceRouter(cfg.Server.Hostname),
		Metrics:        g.metrics,
		Log:            g.log,
	}
}

// bounceRouter asks the rule engine which relay group carries a notification
// addressed to `to`.
//
// It reads g.rules on every call rather than capturing a compiled ruleset, so a
// hot-swapped configuration routes bounces too — the same atomic.Pointer a
// session reads. Only the hostname is captured, because it cannot change without
// a restart anyway (see restartRequired) and reading g.live would mean taking
// g.mu on a path an apply can already be holding it from.
//
// Two things differ from routing ordinary mail, both deliberate:
//
// The env is fully populated — Conn and Helo present but zero, Msg carrying the
// single recipient — and evaluated at StageData. Route stops at the first rule
// whose stage has not been reached and reports "undecided", so evaluating a
// bounce at RCPT would let one data-stage route rule anywhere in the list send
// every bounce to the fallback instead.
//
// Only a relay decision is honoured. A route rule may also discard, reject or
// quarantine; those are decisions about ordinary mail, and letting one swallow a
// notification would silently stop senders hearing that theirs failed. Policy
// rules are not consulted at all: this gateway must not refuse its own bounces.
func (g *gateway) bounceRouter(hostname string) func(string) (string, string, bool) {
	return func(to string) (group, rule string, ok bool) {
		rules := g.rules.Load()
		if rules == nil {
			return "", "", false
		}

		env := &ruleset.Env{
			Stage: ruleset.StageData,
			Conn:  &ruleset.ConnEnv{},
			Helo:  &ruleset.HeloEnv{Name: hostname},
			Mail:  &ruleset.MailEnv{From: "", IsDSN: true},
			Rcpt:  &ruleset.RcptEnv{To: to, Index: 0, CountSoFar: 1},
			Msg: &ruleset.MsgEnv{
				Headers:     map[string][]string{},
				RcptCount:   1,
				RcptDomains: []string{ruleset.Domain(to)},
			},
		}

		d, decided := rules.Route(env, ruleset.StageData)
		if !decided {
			return "", "", false
		}
		relayGroup, isRelay := d.Relay()
		if !isRelay {
			return "", "", false
		}
		return relayGroup, d.Rule, true
	}
}

// sweepGrace is how recently a spooled body may have been written and still be
// spared by the boot-time orphan sweep.
//
// Generous on purpose. The sweep runs before the listeners bind, so nothing
// should be writing bodies at all; an hour is insurance against a second process
// sharing the spool, where being slow to reclaim disk is obviously preferable to
// deleting someone else's mail.
const sweepGrace = time.Hour

// newGateway builds an unstarted gateway. The logger is the boot logger; file
// mode replaces it from server.yaml at bring-up, managed mode keeps it.
func newGateway(log *slog.Logger) *gateway {
	return &gateway{
		log:     log,
		metrics: obs.New(),
		// Built with no rules, so it refuses nothing until an apply says
		// otherwise — and built HERE rather than at bring-up so its buckets
		// outlive every configuration this process runs. See the field comment.
		limiter: ratelimit.New(ratelimit.Rules{}, nil),
	}
}

// Limiter is the rate limiter, read live by the listener chain and by every
// session. Never nil.
func (g *gateway) Limiter() *ratelimit.Limiter { return g.limiter }

// setRateLimits retunes the limiter from a newly-applied configuration.
//
// Called from a successful apply only, on the same reasoning as setAuthUsers and
// setAdminToken: a bundle that was refused is not in force, so the last-good
// limits keep applying rather than a typo removing every ceiling at once.
func (g *gateway) setRateLimits(cfg *config.Config) {
	before := g.limiter.Rules()
	next := rateLimitRules(cfg.Server.RateLimit)
	g.limiter.SetRules(next)

	if reflect.DeepEqual(before, next) {
		return
	}
	// Said once, at the moment it changes. A limit that quietly came on is
	// otherwise diagnosed from the far end, by a sender that started seeing
	// 450s it never used to.
	switch enabled := cfg.Server.RateLimit.Enabled(); {
	case len(enabled) > 0:
		g.log.Info("rate limits in force", "limits", enabled)
	case before.Any():
		g.log.Warn("rate limits removed; nothing is throttled now")
	}
}

// rateLimitRules translates the configuration into the limiter's own vocabulary.
//
// A translation rather than a shared struct, because internal/ratelimit
// deliberately does not import internal/config — it is the same layering that
// keeps internal/dsn, internal/attach and internal/msgauth testable without a
// gateway.
func rateLimitRules(c config.RateLimits) ratelimit.Rules {
	rule := func(l config.RateLimit) ratelimit.Rule {
		return ratelimit.Rule{Rate: l.Rate, Per: l.Per.D(), Burst: l.Burst}
	}
	return ratelimit.Rules{
		ConnPerIP:     rule(c.ConnectPerIP),
		MsgPerSender:  rule(c.MessagesPerSender),
		MsgPerUser:    rule(c.MessagesPerUser),
		RcptPerDomain: rule(c.RcptsPerDomain),
		AuthFailPerIP: rule(c.AuthFailuresPerIP),
		MaxKeys:       c.MaxKeys,
	}
}

// Metrics is the process-wide counter set, for /metrics and the heartbeat.
func (g *gateway) Metrics() *obs.Metrics { return g.metrics }

// Serving reports whether the SMTP listeners are up.
func (g *gateway) Serving() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.live != nil
}

// RestartRequired is what the running configuration changed that needs a
// restart, for the status page.
func (g *gateway) RestartRequired() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.restart)
}

// Spool is the outbound spool, or nil before the first apply.
func (g *gateway) Spool() *queue.Spool { return g.spool.Load() }

// AdminToken is the bearer token /metrics and /readyz require, or "" for none.
//
// Empty before the first configuration applies, which is the one thing that
// cannot be otherwise: the token arrives in a bundle this process has not read
// yet. That leaves a few milliseconds at startup where the endpoints are open.
func (g *gateway) AdminToken() string {
	if t := g.adminToken.Load(); t != nil {
		return *t
	}
	return ""
}

// AuthUsers is the inbound credential set, or nil before the first apply.
//
// Nil is the safe answer for that window: smtpsrv treats it as "no credentials",
// so AUTH is simply not advertised until a configuration is in force — and a
// managed gateway is not serving SMTP at all before then.
func (g *gateway) AuthUsers() *config.Auth { return g.authUsers.Load() }

// setAuthUsers publishes a newly-applied credential set.
//
// Only ever called from a successful apply, on the same reasoning as
// setAdminToken: a bundle that was refused is not in force, so the last-good
// credentials keep working rather than everybody being locked out by a typo.
func (g *gateway) setAuthUsers(cfg *config.Config) {
	before := 0
	if live := g.AuthUsers(); live != nil {
		before = len(live.Users)
	}
	auth := cfg.Auth
	g.authUsers.Store(&auth)

	// Said once, at the moment it changes. "AUTH stopped being offered" is
	// otherwise diagnosed from the far end, by a client that suddenly cannot
	// find the capability it used yesterday.
	switch after := len(auth.Users); {
	case before == 0 && after > 0:
		g.log.Info("inbound AUTH credentials configured; AUTH is now advertised",
			"users", after, "insecure_auth_allowed", cfg.Server.TLS.AllowInsecureAuth)
	case before > 0 && after == 0:
		g.log.Warn("inbound AUTH credentials removed; AUTH is no longer advertised")
	case before != after:
		g.log.Info("inbound AUTH credentials changed", "users", after)
	}
}

// setAdminToken publishes a newly-applied token.
//
// Only ever called from a successful apply. A bundle that fails to apply keeps
// the last-good token, for the same reason it keeps the last-good everything
// else: a configuration that was refused is not in force.
func (g *gateway) setAdminToken(cfg *config.Config) {
	before := g.AdminToken()
	token := cfg.Admin.MetricsToken
	g.adminToken.Store(&token)

	// The one change here most likely to break somebody's existing probe, so it
	// says so once rather than leaving a 401 to be diagnosed from the other end.
	switch {
	case before == "" && token != "":
		g.log.Info("admin token configured; /metrics and /readyz now require it")
	case before != "" && token == "":
		g.log.Warn("admin token removed; /metrics and /readyz are open again")
	}
}

// apply installs l as the running configuration.
//
// The first call builds the process and starts serving; every later call swaps
// the allowlist and the rules. It returns what the incoming configuration
// changed that only a restart can pick up — nil on the first apply, because
// there is nothing yet for it to differ from.
//
// A failure at any step leaves both atomic pointers untouched. Half-applying a
// configuration change would be worse than not applying it.
func (g *gateway) apply(ctx context.Context, l *Loaded) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// On EVERY apply, not just the first. This used to live in bringUp, which
	// runs only when g.live == nil — and a gateway changes its configuration
	// exclusively by later applies, so the warnings that matter most could
	// never fire on the deploy that introduced them. Deploying allow_all: true
	// logged "configuration applied" and nothing else.
	g.warn(l)

	if g.live == nil {
		// Before bringUp, unlike the admin token: bringUp binds the listeners,
		// and a credential set published after that leaves a window in which a
		// client that connects immediately is not offered AUTH. Nothing reads
		// it if bringUp then fails.
		g.setAuthUsers(l.cfg)
		// Before bringUp too, and for the same reason: bringUp binds the
		// listeners, and the throttle listener consults the limiter on the very
		// first connection it accepts.
		g.setRateLimits(l.cfg)
		if err := g.bringUp(ctx, l); err != nil {
			return nil, err
		}
		g.setAdminToken(l.cfg)
		return nil, nil
	}

	restart := restartRequired(g.live, l.cfg)

	// The rules were validated against the relay table that arrived with them,
	// but the delivery runner still holds the one it started with. Recompile
	// against the live table so a rule can never name a group the runner cannot
	// dial. This is why Loaded keeps its *ruleset.File after compiling.
	if err := validateAgainstLiveRelays(l, g.live); err != nil {
		return restart, err
	}

	g.swap(l)
	g.setAdminToken(l.cfg)
	g.setAuthUsers(l.cfg)
	g.setRateLimits(l.cfg)
	g.restart = restart
	return restart, nil
}

// swap installs the hot half of a configuration: the allowlist and the compiled
// rules, the only two things that can change under a running process.
//
// Callers hold g.mu.
func (g *gateway) swap(l *Loaded) {
	g.allowlist.Store(l.cfg.Allowlist)
	g.rules.Store(l.rules)
	g.log.Info("configuration applied",
		"source", l.RulesSource(),
		"allowlist_entries", l.cfg.Allowlist.Len(),
		"policy_rules", len(l.rules.Policy),
		"route_rules", len(l.rules.Routes))
}

// bringUp is the first apply: it creates the spool, the event pipeline, the
// delivery runner and the SMTP server, and starts listening.
//
// Callers hold g.mu.
func (g *gateway) bringUp(ctx context.Context, l *Loaded) error {
	cfg := l.cfg

	// Resolved once, here, and never per event: os.Hostname is a syscall, and a
	// name that changed under a running process would put one box behind two
	// labels in the log tables.
	g.gatewayLabel = gatewayLabel(g.uid, cfg, g.log)

	spool, err := queue.NewSpool(cfg.Server.Outbound.SpoolDir)
	if err != nil {
		g.log.Error("cannot open spool", "dir", cfg.Server.Outbound.SpoolDir, "err", err)
		return err
	}
	g.spool.Store(spool)

	// Reclaim bodies a crash orphaned, here and not from the runner: a session
	// spends its whole data-stage policy pass between writing a body and
	// enqueueing the envelope that claims it, so a sweep racing live traffic
	// could delete a message that is about to be queued. Nothing is listening
	// yet, and the grace period is a second guard for anything left running.
	if n, err := spool.SweepBodies(sweepGrace); err != nil {
		g.log.Warn("cannot sweep orphaned message bodies", "err", err)
	} else if n > 0 {
		g.log.Info("reclaimed orphaned message bodies", "count", n)
	}

	g.events = events.New(events.Options{
		Timeout:    cfg.Server.Events.Timeout.D(),
		Retries:    cfg.Server.Events.Retries,
		BufferSize: cfg.Server.Events.BufferSize,
		Senders:    cfg.Server.Events.Senders,
		APIKey:     LogserviceAPIKey(cfg),
		SpillDir:   spool.FailedEventsDir(),
		Logger:     g.log,
		Metrics:    g.metrics,
	})

	// Built before the runner and the backend, because both send bounces.
	g.bouncer = g.newBouncer(spool, cfg)

	out := cfg.Server.Outbound
	mx := &deliver.Resolver{TTL: out.MXCacheTTL.D()}
	// Nil unless asked for: a nil pool is the "dial every time" path every
	// deployment has run until now, and leaving it nil is what guarantees the
	// default costs nothing rather than merely being configured off.
	var pool *deliver.Pool
	if out.ReuseConnections {
		pool = &deliver.Pool{
			MaxPerRelay: out.PerGroupConnections,
			MaxTotal:    out.MaxPooledConnections,
			MaxMessages: out.MaxMessagesPerConnection,
			IdleTimeout: out.ConnectionIdleTimeout.D(),
		}
		g.pool = pool
		// connection_idle_timeout is otherwise only enforced when a connection
		// is next borrowed, so a relay that stops receiving traffic holds its
		// sockets until process shutdown. Reaped on the idle timeout's own
		// period, on the serve context, so it stops with everything else.
		go pool.Reap(ctx, out.ConnectionIdleTimeout.D())
		g.log.Info("reusing relay connections between envelopes",
			"max_messages_per_connection", out.MaxMessagesPerConnection,
			"idle_timeout", out.ConnectionIdleTimeout.D(),
			"max_pooled_connections", out.MaxPooledConnections)
	}

	signer, err := dkimSigner(cfg)
	if err != nil {
		// validateDKIM already read and parsed every key on both load paths, so
		// reaching here means the filesystem changed underneath us between
		// validation and bring-up. Fatal rather than "carry on unsigned": at
		// bring-up an operator is watching, and mail from a DMARC domain going
		// out unsigned is refused at the far end while every log line here says
		// delivered.
		g.log.Error("cannot load DKIM signing keys", "err", err)
		return err
	}
	if signer != nil {
		g.log.Info("DKIM signing enabled", "domains", SigningDomains(cfg))
	}

	g.runner = queue.NewRunner(spool, queue.RunnerConfig{
		Relays:         cfg.Relays,
		Signer:         signer,
		PollInterval:   cfg.Server.Outbound.PollInterval.D(),
		Concurrency:    cfg.Server.Outbound.Concurrency,
		PerGroup:       cfg.Server.Outbound.PerGroupConnections,
		LocalName:      cfg.Server.Hostname,
		ConnectTimeout: cfg.Server.Outbound.ConnectTimeout.D(),
		DataTimeout:    cfg.Server.Outbound.DataTimeout.D(),
		Backoff:        cfg.Server.Outbound.BackoffFor,
		Jitter:         cfg.Server.Outbound.Jitter,
		MaxLifetime:    cfg.Server.Outbound.MaxLifetime.D(),
		DelayWarnAfter: cfg.Server.Outbound.DelayWarningAfter.D(),
		Bouncer:        g.bouncer,
		MX:             mx,
		Pool:           pool,
		DeliveryURL:    cfg.Logging.URLDelivery,
		Events:         g.events,
		Metrics:        g.metrics,
		Gateway:        g.gatewayLabel,
		Log:            g.log,
	})

	g.allowlist.Store(cfg.Allowlist)
	g.rules.Store(l.rules)

	backend := &smtpsrv.Backend{
		Cfg: cfg,
		// The serve context, so an attachment scan inside a DATA reply is
		// cancelled when shutdown starts instead of outliving it. Same context
		// the runner and the replayer derive from.
		Ctx:       ctx,
		Allowlist: g.allowlist.Load,
		Rules:     g.rules.Load,
		// Read per AUTH command rather than snapshotted per session: a revoked
		// credential should stop working on the next attempt, and there is no
		// transaction here to keep consistent the way there is for Rules.
		Auth:   g.AuthUsers,
		Spool:  spool,
		Events: g.events,
		Attach: attachScanner(cfg),
		// Read live, not snapshotted per session: a limit an operator has just
		// lowered should bite the connection that is already open, which is the
		// one they lowered it for.
		Limiter:  g.Limiter,
		MsgAuth:  msgAuthChecker(cfg, l.rules),
		Metrics:  g.metrics,
		Gateway:  g.gatewayLabel,
		Log:      g.log,
		OnQueued: func([]*queue.Envelope) { g.runner.Nudge() },
		Bounce:   g.bouncer.Bounce,
	}

	tlsCfg, err := g.serverTLS(cfg)
	if err != nil {
		g.log.Error("cannot configure inbound TLS", "err", err)
		return err
	}

	g.srv, err = smtpsrv.NewServer(backend, &cfg.Server, tlsCfg, g.log)
	if err != nil {
		g.log.Error("cannot build server", "err", err)
		return err
	}

	// Set before the listeners so a concurrent apply sees a started gateway and
	// takes the swap path rather than trying to bring it up a second time.
	g.live = cfg

	runnerCtx, cancelRunner := context.WithCancel(ctx)
	g.runnerCancel = cancelRunner
	g.runnerDone = make(chan struct{})
	go func() {
		defer close(g.runnerDone)
		g.runner.Start(runnerCtx)
	}()

	g.startReplayer(ctx, spool, cfg)

	// A bind failure here is terminal for SMTP in this process: smtpListeners
	// is guarded by a sync.Once that has now fired, so a later configuration
	// cannot retry it. Say so loudly rather than pretending a later apply might
	// fix it.
	if err := g.listeners.start(ctx, g.srv, cfg, g.allowlist.Load, g.Limiter, g.metrics, g.log); err != nil {
		g.log.Error("SMTP is not listening and will not retry; restart the gateway", "err", err)
		return err
	}
	return nil
}

// startReplayer runs the failed-events replay loop, unless it is turned off.
//
// A goroutine of its own rather than a case in Runner.Start's select: that loop's
// period is documented as a ceiling on queue latency, and a replay is an HTTP
// call to logservice, not a spool operation. Folding them together would mean
// tuning one to get the other.
func (g *gateway) startReplayer(ctx context.Context, spool *queue.Spool, cfg *config.Config) {
	interval := cfg.Server.Events.ReplayInterval.D()
	retention := cfg.Server.Events.RejectedRetention.D()
	if interval <= 0 {
		g.log.Debug("events.replay_interval is 0; spilled events wait for `mailgw-go events replay`")
		// The retention sweep rides the replay pass, so turning the pass off
		// turns it off too. Said out loud rather than left as a key that looks
		// like it does something and does not — the max.received_headers lesson.
		if retention > 0 {
			g.log.Warn("events.rejected_retention will not be applied while "+
				"events.replay_interval is 0; failed-events/rejected/ will only grow",
				"retention", retention)
		}
		return
	}

	r := &events.Replayer{
		Dir:    spool.FailedEventsDir(),
		Client: g.events,
		// The CURRENT endpoint, not the one recorded when the event failed: a
		// managed gateway's logservice URL arrives in a bundle and can change,
		// and replaying at a stale address forever helps nobody.
		URLFor:    LogserviceURLFor(cfg),
		Log:       g.log,
		Retention: retention,
		Metrics:   g.metrics,
	}

	replayCtx, cancel := context.WithCancel(ctx)
	g.replayCancel = cancel
	g.replayDone = make(chan struct{})
	go func() {
		defer close(g.replayDone)
		r.Run(replayCtx, interval)
	}()
}

// LogserviceURLFor maps an event kind back to the endpoint it belongs to.
func LogserviceURLFor(cfg *config.Config) func(events.Kind) string {
	return func(k events.Kind) string {
		switch k {
		case events.KindConnection:
			return cfg.Logging.URLConn
		case events.KindQueue:
			return cfg.Logging.URLQueue
		case events.KindDelivery:
			return cfg.Logging.URLDelivery
		}
		return ""
	}
}

// warn emits the startup warnings that make a working-but-wrong configuration
// visible: an open relay, credentials in the clear, and rules that can never
// match because this build does not populate the field they read.
func (g *gateway) warn(l *Loaded) {
	if l.cfg.Allowlist.AllowAll() {
		g.log.Warn("allow_all is set — accepting mail from any peer")
	}
	if creds := l.cfg.Relays.PlaintextCredentials(); len(creds) > 0 {
		g.log.Warn("plaintext relay credentials", "relays", creds)
	}
	if mx := l.cfg.Relays.AuthenticatedMX(); len(mx) > 0 {
		g.log.Warn("relay credentials are sent to MX-resolved hosts; the DNS decides who receives them",
			"relays", mx)
	}
	// AUTH is gated by go-smtp's authAllowed(): the session must be encrypted,
	// or allow_insecure_auth must be set. With neither, credentials are
	// configured, the console shows them deployed, and the capability is never
	// offered — a configuration that looks entirely correct and cannot work.
	//
	// The TLS test mirrors serverTLS: no TLS at all means either nothing wants
	// it, or no keypair is configured and this is file mode — a managed node
	// generates a self-signed pair instead, so it always has one.
	noTLS := !l.cfg.Server.WantsTLS() ||
		(!l.cfg.Server.TLS.ConfiguredPublic() && g.tlsDir == "")
	if l.cfg.Auth.Enabled() && noTLS && !l.cfg.Server.TLS.AllowInsecureAuth {
		g.log.Warn("inbound AUTH credentials are configured but AUTH can never be advertised: "+
			"the session cannot be encrypted and tls.allow_insecure_auth is false",
			"users", len(l.cfg.Auth.Users))
	}
	if l.cfg.Auth.Enabled() && l.cfg.Server.TLS.AllowInsecureAuth {
		g.log.Warn("tls.allow_insecure_auth is set — submission passwords may cross the network " +
			"in the clear unless something in front of this gateway terminates TLS")
	}
	if d := l.cfg.Server.DSN; d.Enabled && d.RelayGroup == "" {
		g.log.Warn("no dsn.relay_group; a bounce that no route rule claims cannot be sent")
	}
	for _, f := range l.rules.FieldsUsed() {
		if why, dead := Unpopulated[f]; dead {
			g.log.Warn("a rule matches on a field this build never populates; it cannot match",
				"field", f, "reason", why)
		}
	}
	if len(l.rules.Routes) == 0 {
		g.log.Warn("no route rules; every recipient gets the default action",
			"default", l.rules.Default.String())
	}
}

// shutdown tears the gateway down in an explicit order, bounded by one deadline
// shared across every step. It is safe on a gateway that never came up.
//
// The order is the point, and each step exists because the one before it has to
// have finished:
//
//  1. Close the listeners, so nothing new arrives while the rest drains.
//  2. Drain in-flight SMTP sessions, so a message already being accepted is
//     spooled rather than dropped after the client was told to send it.
//  3. Cancel the delivery runner and WAIT for it. Exiting with an attempt still
//     open is not lossy — Recover picks it up next boot — but it redelivers a
//     message the relay may already have taken, and this is the one cause of
//     that we can simply remove. Waiting only terminates because M9.2 bounded a
//     relay attempt; before that this would have hung instead of hurried.
//  4. Stop the failed-events replayer, because the next step writes into the
//     directory it is draining.
//  5. Close the event pipeline last, because steps 2 and 3 are still producing
//     audit events. Anything it cannot deliver in the time left spills to
//     failed-events/.
//
// Everything shares one deadline rather than getting its own, so the total is
// what server.shutdown_timeout says. Per-step timeouts would silently multiply,
// and the number that actually matters is the container's stop grace period.
func (g *gateway) shutdown() {
	deadline := g.shutdownTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	g.listeners.closeAll()

	if g.srv != nil {
		// Return as soon as sessions are done rather than always burning the
		// whole grace period — a gateway with nothing in flight, which is most
		// restarts, should stop immediately.
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = g.srv.Shutdown(ctx)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			g.log.Warn("grace period expired with SMTP sessions still open",
				"timeout", deadline)
		}
	}

	if g.runnerCancel != nil {
		g.runnerCancel()
		select {
		case <-g.runnerDone:
		case <-ctx.Done():
			g.log.Warn("grace period expired with a delivery attempt still in flight; "+
				"it will be recovered and retried on the next start", "timeout", deadline)
		}
	}

	if g.pool != nil {
		// Under the same deadline as everything else here: each QUIT waits for a
		// reply, and a relay that has stopped answering is a common reason to be
		// shutting down in the first place.
		g.pool.Close(ctx)
	}

	// Before Close, not after. Both write to failed-events/: the replayer is
	// draining it and Close spills whatever it cannot deliver back into it. A
	// replay pass still running during that drain would be reading a directory
	// being filled behind it, and could claim a file the pipeline is about to
	// replace.
	if g.replayCancel != nil {
		g.replayCancel()
		select {
		case <-g.replayDone:
		case <-ctx.Done():
			g.log.Warn("grace period expired with an audit-event replay in flight", "timeout", deadline)
		}
	}

	if g.events != nil {
		g.events.Close(ctx)
	}
}

// shutdownTimeout is the whole teardown's budget, from the running
// configuration. Falls back to the default on a gateway that never came up.
func (g *gateway) shutdownTimeout() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live != nil {
		if d := g.live.Server.ShutdownTimeout.D(); d > 0 {
			return d
		}
	}
	return config.DefaultShutdownTimeout
}

// watchSignals turns SIGHUP into a re-application of the configuration:
// "re-read the files" in file mode, "re-apply from the cache" in managed mode.
// The same operator gesture with the same all-or-nothing guarantee, which is
// what keeps the two modes feeling like one product.
//
// fsnotify is deliberately not used. A config file being written is observed
// mid-write, and reloading a truncated rule file — even with the keep-on-error
// guarantee — means logging a spurious error every time an operator saves.
// SIGHUP says "I am done editing", which is the signal that actually matters.
func (g *gateway) watchSignals(ctx context.Context, reload func(context.Context) error) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if err := reload(ctx); err != nil {
				g.log.Error("reload failed; keeping the running configuration", "err", err)
			}
		}
	}
}

// LogserviceAPIKey resolves the credential events are posted with.
//
// This gateway has no environment to read — that is the point of it — so
// Central Management delivers the key in the bundle, and there is nowhere else
// for it to come from. There used to be an events.api_key_env fallback here; it
// resolved to the empty string on every node this binary can run on, which meant
// a logservice that required a key rejected every audit event and the mail path
// never noticed.
func LogserviceAPIKey(cfg *config.Config) string {
	return cfg.Logging.APIKey
}

// serverTLS resolves the certificate the SMTP listener serves, or nil when this
// configuration wants no TLS at all.
//
// Three cases, in order:
//
//   - Neither starttls nor implicit_tls: nil, and nothing is generated. A
//     configuration that opted out gets no files written on its behalf.
//   - tls.cert and tls.key are set: use them. They are paths on this host, which
//     is why a private key never travels in a configuration bundle — the console
//     stores versions forever and serves them to every gateway on the profile.
//   - Neither is set, and this is a managed node: generate a self-signed pair
//     into the data directory and use that. A zero-configuration node has no
//     operator-supplied files by definition, and opportunistic STARTTLS with a
//     self-signed certificate is what nearly every sending MTA accepts. It is not
//     a substitute for a real certificate; it is a substitute for cleartext.
func (g *gateway) serverTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.Server.WantsTLS() {
		return nil, nil
	}

	certPath, keyPath := cfg.Server.TLS.Cert, cfg.Server.TLS.Key
	if certPath == "" || keyPath == "" {
		if g.tlsDir == "" {
			// File mode with no keypair: unchanged from every release so far.
			return nil, nil
		}
		var err error
		certPath, keyPath, err = tlsx.EnsureSelfSigned(g.tlsDir, cfg.Server.Hostname)
		if err != nil {
			return nil, err
		}
		g.log.Info("serving inbound TLS with a self-signed certificate; "+
			"replace it in place to use a real one, no restart needed",
			"cert", certPath, "hostname", cfg.Server.Hostname)
	}
	return smtpsrv.NewTLSConfig(certPath, keyPath)
}

// attachScanner builds the blocklist client, or nil when attachment scanning is
// off — which is what the session's "is it enabled?" check reads.
//
// Built once per bring-up rather than per message, so its transport pools
// connections to logservice instead of dialling for every attachment. The
// credential is the same one events are posted with: AttachChecker.js:21-23 read
// the same API_KEY, and a second key for the same server would be a second thing
// to rotate.
func attachScanner(cfg *config.Config) smtpsrv.AttachScanner {
	a := cfg.Server.Attach
	if !a.Enabled {
		return nil
	}
	return &attach.Scanner{
		URL:     a.URL,
		Timeout: a.Timeout.D(),
		APIKey:  LogserviceAPIKey(cfg),
	}
}

// msgAuthChecker builds the inbound message-authentication checker, or nil when
// nothing wants one — which is what the session's nil check reads.
//
// Two independent reasons to want one, exactly as internal/smtpsrv/attach.go
// has: a msgauth.* configuration key, or a rule that reads spf.*, dkim.* or
// dmarc.*. The rules are consulted here rather than only in the session because
// the resolver is built once per bring-up, and rules are re-read on every SIGHUP
// — so a rule added by a reload gets its check through the session's own
// Needs* calls against the live ruleset, while this decides whether a resolver
// exists at all. That is why the checker is created whenever the RULES ask, not
// only when the configuration does.
func msgAuthChecker(cfg *config.Config, rules *ruleset.Ruleset) *smtpsrv.MsgAuth {
	m := cfg.Server.MsgAuth
	if !m.SPF.Enabled && !m.DKIM.Enabled && !m.DMARC.Enabled &&
		!rules.NeedsSPF() && !rules.NeedsDKIM() && !rules.NeedsDMARC() {
		return nil
	}
	return &smtpsrv.MsgAuth{
		Resolver:          msgauth.NewResolver(m.DNSTimeout.D()),
		AuthservID:        m.AuthservIDFor(cfg.Server.Hostname),
		SPF:               m.SPF.Enabled,
		DKIM:              m.DKIM.Enabled,
		DMARC:             m.DMARC.Enabled,
		MaxDKIMSignatures: m.MaxDKIMSignatures,
	}
}

// dkimSigner builds the outbound signer, or nil when signing is off.
func dkimSigner(cfg *config.Config) (*queue.Signer, error) {
	s := cfg.Server.MsgAuth.Sign
	if !s.Enabled || len(s.Keys) == 0 {
		return nil, nil
	}
	keys := make([]msgauth.Key, 0, len(s.Keys))
	for _, k := range s.Keys {
		keys = append(keys, msgauth.Key{Domain: k.Domain, Selector: k.Selector, Path: k.Key})
	}
	Loaded, err := msgauth.NewKeys(keys)
	if err != nil {
		return nil, err
	}
	hdrCan, bodyCan, err := msgauth.ParseCanonicalization(s.Canonicalization)
	if err != nil {
		return nil, err
	}
	return queue.NewSigner(Loaded, hdrCan, bodyCan, s.Headers, s.Expiration.D()), nil
}

func SigningDomains(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Server.MsgAuth.Sign.Keys))
	for _, k := range cfg.Server.MsgAuth.Sign.Keys {
		out = append(out, k.Domain+" (s="+k.Selector+")")
	}
	return out
}

// restartRequired names what the incoming configuration changes that this
// process cannot pick up without restarting.
//
// A list rather than a bool, deliberately: "restart required" with no reason is
// an alarm an operator cannot act on, and the console renders the list.
//
// The membership is not arbitrary. The delivery runner holds its relay table
// for the life of the process; the listeners, the TLS keypair and the SIZE
// limit are bound when the server is built; and smtpsrv.Backend keeps the
// *config.Config it was constructed with and reads Hostname and the logservice
// URLs off it at session time. Anything on that list would otherwise be
// reported as applied while silently having no effect.
//
// Allowlist is not compared: it hot-swaps.
//
// Admin is not compared either, and that is a third category rather than an
// oversight: the metrics token is read from an atomic on every request, so a
// change to it is already in force by the time this function returns. Nothing
// reflects over this list to catch an omission, so the only guard is this
// sentence — anything added to config.Config belongs in exactly one of the
// three (restart here, hot-swapped in swap, or read live like this one).
func restartRequired(live, next *config.Config) []string {
	if live == nil || next == nil {
		return nil
	}

	var out []string
	add := func(changed bool, what string) {
		if changed {
			out = append(out, what)
		}
	}

	add(!live.Relays.Equal(next.Relays), "relays")
	add(!reflect.DeepEqual(live.Server.Listen, next.Server.Listen), "listen")
	add(live.Server.TLS != next.Server.TLS, "tls")
	add(live.Server.Hostname != next.Server.Hostname, "hostname")
	add(live.Server.Greeting != next.Server.Greeting, "greeting")
	add(!slices.Equal(live.Server.LocalDomains, next.Server.LocalDomains), "local_domains")
	add(live.Server.Max != next.Server.Max, "max")
	add(live.Server.SMTPUTF8 != next.Server.SMTPUTF8, "smtputf8")
	add(live.Server.Inactivity != next.Server.Inactivity, "inactivity_timeout")
	// On the list because shutdown reads g.live, and only bringUp ever sets it.
	// Without this a deployed bundle changing the key would report "applied"
	// while the process kept tearing down on the value it booted with.
	add(live.Server.ShutdownTimeout != next.Server.ShutdownTimeout, "shutdown_timeout")
	add(live.Server.Log != next.Server.Log, "log")
	add(!reflect.DeepEqual(live.Server.Outbound, next.Server.Outbound), "outbound")
	add(live.Server.DSN != next.Server.DSN, "dsn")
	add(live.Server.Attach != next.Server.Attach, "attach")
	// DeepEqual rather than !=, because MsgAuthConfig holds slices (the signing
	// keys and the header list) — the same reason Outbound and Listen use it.
	//
	// Squarely in the restart category, not the read-live one: the resolver and
	// the loaded signing keys are built once at bring-up and captured by
	// smtpsrv.Backend and queue.RunnerConfig respectively, so a deployed bundle
	// changing them would otherwise report "applied" while the process kept
	// checking and signing on what it booted with. (An individual key ROTATED IN
	// PLACE does not need a restart — msgauth.Keys watches its files — but a
	// change to which keys exist does.)
	add(!reflect.DeepEqual(live.Server.MsgAuth, next.Server.MsgAuth), "msgauth")
	add(live.Server.Events != next.Server.Events, "events")
	add(live.Logging != next.Logging, "logging")

	return out
}

// gatewayLabelMax is the width of the `gateway` column in the log tables
// (logservice/migrations/023). Anything longer has to be cut, and cutting
// silently would make two gateways indistinguishable in the log viewer — which
// is the precise thing the column was added to prevent.
const gatewayLabelMax = 64

// gatewayLabel is the value written to the `gateway` column of every audit row.
//
// A managed gateway uses its gateway_uid, so a log row joins the console's
// Gateways record directly. A file-mode gateway has no uid, and falls back to
// server.hostname — already its SMTP banner identity, already what the operator
// wrote down, and stable across container restarts in a way os.Hostname is not.
func gatewayLabel(uid string, cfg *config.Config, log *slog.Logger) string {
	label := uid
	if label == "" && cfg != nil {
		label = cfg.Server.Hostname
	}
	if label == "" {
		label, _ = os.Hostname()
	}

	if len(label) > gatewayLabelMax {
		log.Warn("the gateway label is too long for the log tables and has been truncated; "+
			"two gateways sharing a prefix will be indistinguishable in the log viewer",
			"label", label, "max", gatewayLabelMax)
		label = label[:gatewayLabelMax]
	}
	return label
}
