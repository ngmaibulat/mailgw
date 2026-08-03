package node

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// Serve runs a gateway whose configuration comes from Central Management, and
// returns the process exit code.
//
// It serves mail as soon as a cached bundle applies and not before. A gateway
// with no allowlist would deny every peer anyway (the allowlist zero value
// denies), and a listener that can only reject is worse than no listener: it
// looks healthy to a load balancer. Fail closed, and say so on the status page.
//
// Nothing here blocks on reaching the console. Booting from the cache with
// Central Management down is the entire reason the cache exists.
func Serve(ctx context.Context, o Options) int {
	// There is no server.yaml until a bundle applies, so logging takes its
	// defaults. The logger is NOT rebuilt at bring-up: the admin UI and the
	// agent already hold this one, and leaving them on a stale logger to honour
	// a log-level change is the worse trade. A bundle that changes it reports
	// "log" in restart_required instead.
	log := newLogger(config.LogConfig{})
	slog.SetDefault(log)

	if o.AdminAddr == "" {
		return fail(log, "managed mode has no way to be provisioned without an admin UI: give -admin an address")
	}

	st, err := store.Open(o.DataDir)
	if err != nil {
		log.Error("cannot open the data directory", "dir", o.DataDir, "err", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	id, err := st.Identity()
	if err != nil {
		log.Error("cannot establish a gateway identity", "err", err)
		return 1
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
		log.Error("cannot establish an admin claim code", "err", err)
		return 1
	}
	if !claim.Claimed {
		log.Warn("admin UI is unclaimed — enter this code in the wizard to take control",
			"code", claim.Code,
			"reset", fmt.Sprintf("mailgw-go claim reset -data %s", o.DataDir))
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
	g.tlsDir = filepath.Join(o.DataDir, "tls")

	a := newAgent(st, o.Version, log)
	a.gw = g
	a.metrics = g.Metrics()
	// A bundle whose server profile chooses no spool directory must not land on
	// the compiled-in /opt/mailgw-go/queue, which a managed node cannot write.
	a.bundleOpts = config.BundleOptions{SpoolDir: SpoolFallback(o.DataDir)}

	ui := &adminui.Server{
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
	// The UI is how an unprovisioned gateway gets provisioned, so it comes up
	// first and a bind failure is fatal — without it this process can never
	// become useful.
	adminErr := make(chan error, 1)
	go func() { adminErr <- ui.ListenAndServe(ctx, o.AdminAddr) }()

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
			log.Error("admin UI failed", "addr", o.AdminAddr, "err", err)
			return 1
		}
	}
	log.Info("shutting down")
	return 0
}

func fail(log *slog.Logger, msg string) int {
	log.Error(msg)
	return 2
}
