package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// ErrNoAdminAddr is New refusing to build a node nobody could provision.
//
// Its own error so Serve can exit 2 for it: an empty -admin is a usage mistake,
// and a script distinguishing "you typed it wrong" from "it failed to start" is
// the reason exit codes are not all 1.
var ErrNoAdminAddr = errors.New(
	"managed mode has no way to be provisioned without an admin UI: give -admin an address")

// Node is one gateway process: its store, its identity, the gateway runtime and
// its relationship with Central Management.
//
// Construction is separate from running so that a caller can reach the thing it
// built before it starts blocking. cmd/mailgw-go does not need that and calls
// Serve; cmd/mailgw-go-test starts a control API against the same Node and then
// calls Run. Folding the two together would have forced the test binary to
// either duplicate this bring-up or receive it through a callback, and a second
// copy of the wiring is the one thing this package exists to prevent.
type Node struct {
	opts  Options
	log   *slog.Logger
	store *store.Store
	gw    *gateway
	agent *agent
	ui    *adminui.Server

	// injected counts configurations pushed in through the control API, for a
	// display version number. Touched only from ApplyBundle, which the test
	// control server serialises; see control.go.
	injected int
}

// New builds a gateway process without starting anything that listens.
//
// It opens the store, establishes the Ed25519 identity, mints the admin claim
// code and assembles the gateway, the agent and the admin UI. Nothing here
// binds a socket and nothing contacts the console: New must succeed on a node
// whose Central Management is down, because booting from the cache in exactly
// that situation is the reason the cache exists.
//
// The caller owns the returned Node and must Close it. Run does that itself.
func New(o Options) (*Node, error) {
	// There is no server.yaml until a bundle applies, so logging takes its
	// defaults. The logger is NOT rebuilt at bring-up: the admin UI and the
	// agent already hold this one, and leaving them on a stale logger to honour
	// a log-level change is the worse trade. A bundle that changes it reports
	// "log" in restart_required instead.
	log := newLogger(config.LogConfig{})
	slog.SetDefault(log)

	if o.AdminAddr == "" {
		return nil, ErrNoAdminAddr
	}

	st, err := store.Open(o.DataDir)
	if err != nil {
		return nil, fmt.Errorf("cannot open the data directory %s: %w", o.DataDir, err)
	}

	// From here on a failure must close the store, or a caller that ignores the
	// Node on error leaves a SQLite handle and its WAL files behind.
	id, err := st.Identity()
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("cannot establish a gateway identity: %w", err)
	}

	log.Info("starting in managed mode", "version", o.Version, "commit", o.Commit,
		"data", o.DataDir, "fingerprint", id.Fingerprint, "admin", o.AdminAddr)

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
		_ = st.Close()
		return nil, fmt.Errorf("cannot establish an admin claim code: %w", err)
	}
	if !claim.Claimed {
		log.Warn("admin UI is unclaimed — enter this code in the wizard to take control",
			"code", claim.Code,
			"reset", fmt.Sprintf("mailgw-go claim reset -data %s", o.DataDir))
	}

	g := newGateway(log)
	// The uid is this node's identity in the console, and it is what labels the
	// audit rows it writes. Empty until registration completes, which is why
	// the label is resolved at bring-up rather than here.
	g.uid = id.GatewayUID
	// A managed node has no operator-supplied files, so a keypair it generates
	// itself is the only way inbound TLS can exist here. Beside the identity key
	// on the same durable volume, and the place to drop a real certificate.
	g.tlsDir = filepath.Join(o.DataDir, "tls")

	a := newAgent(st, o.Version, log)
	a.gw = g
	a.metrics = g.Metrics()
	// A bundle whose server profile chooses no spool directory must not land on
	// the compiled-in /opt/mailgw-go/queue, which a managed node cannot write.
	a.bundleOpts = config.BundleOptions{SpoolDir: SpoolFallback(o.DataDir)}

	n := &Node{opts: o, log: log, store: st, gw: g, agent: a}
	n.ui = &adminui.Server{
		Store:        st,
		Version:      o.Version,
		Commit:       o.Commit,
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
	return n, nil
}

// Log is the process logger, so a caller can report on the same stream.
func (n *Node) Log() *slog.Logger { return n.log }

// Close releases everything New acquired: the gateway's ordered teardown first,
// then the store.
//
// The order matters and is the same one shutdown documents — listeners, SMTP
// drain, runner, pool, replayer, event pipeline — all under one
// server.shutdown_timeout. Closing the store first would pull the configuration
// cache out from under a teardown that still reads it.
func (n *Node) Close() {
	n.gw.shutdown()
	_ = n.store.Close()
}

// Run starts everything that listens and blocks until ctx is cancelled or the
// admin UI fails, then tears the process down. It returns the process exit code.
//
// It serves mail as soon as a cached bundle applies and not before. A gateway
// with no allowlist would deny every peer anyway (the allowlist zero value
// denies), and a listener that can only reject is worse than no listener: it
// looks healthy to a load balancer. Fail closed, and say so on the status page.
//
// Nothing here blocks on reaching the console. Booting from the cache with
// Central Management down is the entire reason the cache exists.
func (n *Node) Run(ctx context.Context) int {
	defer n.Close()

	// The UI is how an unprovisioned gateway gets provisioned, so it comes up
	// first and a bind failure is fatal — without it this process can never
	// become useful.
	adminErr := make(chan error, 1)
	go func() { adminErr <- n.ui.ListenAndServe(ctx, n.opts.AdminAddr) }()

	// Boot from the cache. A failure is logged rather than fatal: the pull loop
	// may be handed a working configuration within the next poll.
	bootFromCache(ctx, n.agent, n.store, n.log)

	go n.agent.Run(ctx)
	go n.agent.Watch(ctx)
	go n.gw.watchSignals(ctx, n.agent.applyFromCache)

	select {
	case <-ctx.Done():
	case err := <-adminErr:
		if err != nil {
			n.log.Error("admin UI failed", "addr", n.opts.AdminAddr, "err", err)
			return 1
		}
	}
	n.log.Info("shutting down")
	return 0
}

// Serve is New plus Run: the whole of what the shipped binary does.
func Serve(ctx context.Context, o Options) int {
	n, err := New(o)
	if err != nil {
		// New logs nothing on the failure paths — it has a logger but the
		// caller owns the exit code, and a message printed twice is worse than
		// one printed here.
		slog.Default().Error("cannot start", "err", err)
		if errors.Is(err, ErrNoAdminAddr) {
			return 2
		}
		return 1
	}
	return n.Run(ctx)
}
