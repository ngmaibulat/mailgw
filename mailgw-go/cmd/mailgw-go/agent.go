package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/central"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// pollInterval is how often a provisioned gateway asks the console what it
// should be running. Jittered so a fleet restarted together does not stampede.
//
// It is the fallback, not the primary signal: the WebSocket in internal/central
// wakes the loop the moment a deploy lands. Everything still works if that
// socket never connects, which is what makes it safe to depend on neither.
const pollInterval = 15 * time.Second

// applyRetryBackoff is how long a version that failed to apply is left alone.
// Re-fetching and re-failing a broken bundle every tick would bury the real
// error under an identical one and hammer the console for nothing.
const applyRetryBackoff = 5 * time.Minute

// maxApplyError caps the message sent to the console. Its schema
// (webui-fastify/src/validation/agent.ts) rejects an apply_error longer than
// 4096, and a rejected report loses the heartbeat as well as the error.
const maxApplyError = 4000

// applier is what a pulled configuration is applied to. *gateway implements it;
// the pull-loop tests supply a fake so they never open a socket.
type applier interface {
	apply(ctx context.Context, l *loaded) (restart []string, err error)
}

// agent owns this gateway's relationship with Central Management: registering,
// polling for approval, pulling the configuration version the console wants,
// applying it, and reporting what actually happened.
type agent struct {
	store   *store.Store
	version string
	log     *slog.Logger

	// gw is what a pulled bundle is applied to. Nil in file mode, where the
	// configuration does not come from here.
	gw applier
	// bundleOpts carries the process-level knobs a bundle cannot: the spool
	// directory to fall back on.
	bundleOpts config.BundleOptions

	// metrics rides along on the heartbeat below. Nil in tests that do not
	// care; report simply omits the field then.
	metrics *obs.Metrics

	// state is read by the admin UI on every page render and written only by
	// Register and Run.
	state atomic.Pointer[adminui.State]

	// wake is how the WebSocket watcher asks for an immediate poll. Buffered by
	// one and sent to non-blockingly, so a burst of console changes collapses
	// into a single extra tick.
	wake chan struct{}

	// Pull-loop memory. Touched only from Run's goroutine and from the boot
	// path before it starts.
	lastApproval  string
	lastRestart   []string
	failedVersion int64
	failedUntil   time.Time
}

func newAgent(st *store.Store, version string, log *slog.Logger) *agent {
	a := &agent{store: st, version: version, log: log, wake: make(chan struct{}, 1)}
	a.refresh(nil)
	return a
}

// Wake asks the poll loop to run now rather than at its next tick. Safe to call
// from any goroutine and never blocks.
func (a *agent) Wake() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// Watch keeps a notification socket open to the console so a deploy lands in
// milliseconds instead of on the next poll.
//
// It is purely an accelerator: every failure here is a debug line, because the
// poll loop is doing the same job on a timer underneath. A gateway whose
// console does not support the socket, or cannot be reached over it, behaves
// exactly as it would without this.
func (a *agent) Watch(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		c, err := a.client()
		if err != nil {
			// Unprovisioned. The wizard resolves it; wait rather than spin.
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
			}
			continue
		}

		// Watch reconnects internally and returns only when ctx ends. The wait
		// below is belt and braces: it stops this loop spinning if that
		// contract ever changes.
		_ = c.Watch(ctx, a.Wake)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// State is the snapshot the admin UI renders.
func (a *agent) State() adminui.State {
	if s := a.state.Load(); s != nil {
		return *s
	}
	return adminui.State{}
}

// refresh rebuilds the cached state from the store, preserving whatever the
// last exchange with the console told us. A nil status leaves those fields as
// they were.
func (a *agent) refresh(status *central.StatusResponse) {
	next := adminui.State{}
	if prev := a.state.Load(); prev != nil {
		next = *prev
	}

	if id, err := a.store.Identity(); err == nil {
		next.Fingerprint = id.Fingerprint
		next.GatewayUID = id.GatewayUID
	} else {
		a.log.Error("cannot read gateway identity", "err", err)
	}
	if u, err := a.store.CentralURL(); err == nil {
		next.CentralURL = u
	}
	if v, err := a.store.BoolSetting(store.KeyCentralInsecureTLS); err == nil {
		next.InsecureTLS = v
	}
	if v, err := a.store.Setting(store.KeyCentralCAFile); err == nil {
		next.CAFile = v
	}
	if cfg, err := a.store.AppliedConfig(); err == nil && cfg != nil {
		v := cfg.Version
		next.AppliedVersion = &v
		next.ApplyError = cfg.ApplyError
	}
	// The failed version's error is the one an operator needs to read; the
	// applied row's is by definition empty.
	if a.failedVersion != 0 {
		if f, err := a.store.ConfigByVersionID(a.failedVersion); err == nil && f != nil && f.ApplyError != "" {
			next.ApplyError = f.ApplyError
		}
	}
	if status != nil {
		next.Approval = status.Approval
		next.DesiredVersion = status.DesiredVersion
	}
	if a.gw != nil {
		if g, ok := a.gw.(*gateway); ok {
			next.Serving = g.Serving()
			next.RestartRequired = g.RestartRequired()
		}
	}

	a.state.Store(&next)
}

// client builds a signing client from the stored settings. It returns an error
// while the gateway is unprovisioned.
func (a *agent) client() (*central.Client, error) {
	url, err := a.store.CentralURL()
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, errors.New("no Central Management URL is configured")
	}
	id, err := a.store.Identity()
	if err != nil {
		return nil, err
	}
	return a.clientFor(url, id)
}

func (a *agent) clientFor(url string, id *store.Identity) (*central.Client, error) {
	insecure, err := a.store.BoolSetting(store.KeyCentralInsecureTLS)
	if err != nil {
		return nil, err
	}
	caFile, err := a.store.Setting(store.KeyCentralCAFile)
	if err != nil {
		return nil, err
	}
	httpClient, err := central.NewHTTPClient(insecure, caFile, central.DefaultTimeout)
	if err != nil {
		return nil, err
	}
	return &central.Client{
		BaseURL: url,
		ID:      id.GatewayUID,
		Key:     id.PrivateKey,
		HTTP:    httpClient,
		Log:     a.log,
	}, nil
}

// Register points this gateway at a console and asks to join.
//
// The settings are stored BEFORE the call, so a failure leaves the wizard
// pre-filled and a restart retries automatically. That is also what makes
// "central URL set but no gateway id" mean "registration did not complete",
// which is the state the wizard renders with an error.
func (a *agent) Register(ctx context.Context, s adminui.Settings) error {
	if err := a.store.SetCentralURL(s.CentralURL); err != nil {
		return err
	}
	if err := a.store.SetBoolSetting(store.KeyCentralInsecureTLS, s.InsecureTLS); err != nil {
		return err
	}
	if err := a.store.SetSetting(store.KeyCentralCAFile, s.CAFile); err != nil {
		return err
	}

	id, err := a.store.Identity()
	if err != nil {
		return err
	}
	// Registration proves possession of the key rather than presenting an id,
	// so the client is built without one.
	c, err := a.clientFor(s.CentralURL, &store.Identity{
		PrivateKey: id.PrivateKey, PublicKey: id.PublicKey,
	})
	if err != nil {
		a.note(err)
		return err
	}

	resp, err := c.Register(ctx, id.PublicKey, central.Collect(a.version))
	if err != nil {
		a.note(err)
		return describeRegisterError(err)
	}

	// The fingerprint is the one string an operator eyeballs. If the console
	// computed a different one, something is very wrong and approving it would
	// approve the wrong thing.
	if resp.Fingerprint != id.Fingerprint {
		err := fmt.Errorf("the console reported fingerprint %s, but this gateway's key is %s",
			resp.Fingerprint, id.Fingerprint)
		a.note(err)
		return err
	}

	if err := a.store.SetGatewayUID(resp.GatewayUID); err != nil {
		return err
	}

	a.log.Info("registered with Central Management",
		"gateway_uid", resp.GatewayUID, "fingerprint", resp.Fingerprint,
		"approval", resp.Approval, "new", resp.New)

	a.clearNote()
	a.refresh(&central.StatusResponse{Approval: resp.Approval})
	return nil
}

// describeRegisterError turns the failure modes an operator can actually fix
// into advice, rather than leaving a bare transport error in the form.
func describeRegisterError(err error) error {
	switch {
	case errors.Is(err, central.ErrClockSkew):
		return fmt.Errorf("the console rejected our timestamp — check this gateway's clock (NTP): %w", err)
	case central.IsUnreachable(err):
		return fmt.Errorf("could not reach the console. If it serves a self-signed "+
			"certificate, tick the trust checkbox or supply a CA bundle: %w", err)
	default:
		return err
	}
}

// note records why the last exchange with the console failed, for the UI.
func (a *agent) note(err error) {
	cur := a.State()
	cur.LastError = err.Error()
	a.state.Store(&cur)
}

func (a *agent) clearNote() {
	cur := a.State()
	cur.LastError = ""
	a.state.Store(&cur)
}

// Run reconciles this gateway with the console until ctx is cancelled.
//
// Nothing here is allowed to be fatal: if the console is unreachable the
// gateway keeps running on whatever it already has. That is the whole promise
// of the local cache, and it is why every error path below logs and continues.
func (a *agent) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
			// The console said something changed; go and look now.
		case <-time.After(jittered(pollInterval)):
		}

		c, err := a.client()
		if err != nil {
			// Unprovisioned: entirely normal, and the wizard is how it gets
			// resolved. Nothing to say every 15 seconds.
			continue
		}

		status, err := c.Status(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.logPollError(err)
			a.note(err)
			continue
		}

		if status.Approval != a.lastApproval {
			a.log.Info("approval state", "approval", status.Approval)
			a.lastApproval = status.Approval
		}
		a.clearNote()

		// Pull before reporting, so the report carries the result of the apply
		// that just happened rather than describing the previous tick.
		if a.gw != nil && status.Approval == approvalApproved && status.DesiredVersionID != nil {
			a.pull(ctx, c, status)
		}

		a.refresh(status)
		a.report(ctx, c)
	}
}

// approvalApproved is the console's word for a gateway that may fetch a
// configuration. Every other value means "wait".
const approvalApproved = "approved"

// pull brings this gateway to the configuration version the console wants.
//
// It never returns an error: a failure is recorded in the cache and reported,
// and the previous configuration keeps running.
func (a *agent) pull(ctx context.Context, c *central.Client, st *central.StatusResponse) {
	want := *st.DesiredVersionID

	// Record what the console asked for before doing anything about it. This is
	// the only place that intent is written down, and boot depends on it — see
	// bootConfig.
	if err := a.store.SetDesiredVersionID(want); err != nil {
		a.log.Error("cannot record the desired version", "err", err)
	}

	// Already running it. Applied is the authority here, not fetched — a
	// version that arrived but did not compile must not count as done.
	applied, err := a.store.AppliedConfig()
	if err != nil {
		a.log.Error("cannot read the applied configuration", "err", err)
		return
	}
	if applied != nil && applied.VersionID == want {
		a.failedVersion = 0
		return
	}

	// Tried this one and it did not apply. Back off rather than re-fetching and
	// re-failing every tick; a different desired version clears this.
	if a.failedVersion == want && time.Now().Before(a.failedUntil) {
		return
	}

	// Do we already hold those exact bytes? This is the rollback path: the
	// console repoints at an older version we cached earlier, so there is
	// nothing to fetch and what runs afterwards is byte-identical to what ran
	// before. The digest is the console's, treated as an opaque change token
	// and compared — never recomputed, because the JSON on the wire is not the
	// serialisation that was hashed.
	cached, err := a.store.ConfigByVersionID(want)
	if err != nil {
		a.log.Error("cannot read the config cache", "err", err)
		return
	}

	if cached != nil && (st.BundleSHA256 == nil || cached.SHA256 == *st.BundleSHA256) {
		// Re-save to refresh fetched_at. LatestConfig orders by it, and boot
		// prefers the latest — so without this a rollback would be undone by
		// the next restart, which would boot the superseded higher version_id.
		// ApplyError is cleared deliberately: this attempt is a fresh one.
		cached.FetchedAt, cached.ApplyError = time.Now(), ""
		if err := a.store.SaveConfig(cached); err != nil {
			a.log.Error("cannot refresh the cached configuration", "err", err)
			return
		}
	} else {
		resp, err := c.Config(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.logPollError(err)
			a.note(err)
			return
		}

		cached = &store.CachedConfig{
			VersionID: resp.VersionID,
			Version:   resp.Version,
			SHA256:    resp.BundleSHA256,
			Bundle:    resp.Bundle,
			FetchedAt: time.Now(),
		}
		// Cache BEFORE attempting to apply. A bundle that breaks the apply path
		// must still be on disk afterwards, or the failure is unexplainable and
		// its reason never reaches the console.
		if err := a.store.SaveConfig(cached); err != nil {
			a.log.Error("cannot cache the pulled configuration", "err", err)
			// Applying something we could not record would leave the gateway
			// running a version it cannot name after a restart.
			return
		}
		a.log.Info("pulled a configuration version",
			"version", cached.Version, "version_id", cached.VersionID, "sha256", cached.SHA256)
	}

	_, _ = a.applyCached(ctx, cached)
}

// applyCached is the single place a cached bundle becomes running
// configuration — shared by boot, the pull loop, the WebSocket wake-up and
// SIGHUP, so all four have identical semantics.
func (a *agent) applyCached(ctx context.Context, c *store.CachedConfig) ([]string, error) {
	var restart []string

	l, err := loadBundle(c.Bundle, a.bundleOpts, bundleSource(c))
	if err == nil {
		restart, err = a.gw.apply(ctx, l)
	}

	if err != nil {
		msg := truncate(err.Error(), maxApplyError)
		if serr := a.store.MarkApplyError(c.VersionID, msg); serr != nil {
			a.log.Error("cannot record the apply failure", "err", serr)
		}
		a.log.Error("configuration version failed to apply; keeping the running configuration",
			"version", c.Version, "version_id", c.VersionID, "err", err)
		a.failedVersion, a.failedUntil = c.VersionID, time.Now().Add(applyRetryBackoff)
		a.refresh(nil)
		return restart, err
	}

	// Marked applied even when part of it needs a restart. Boot reads the
	// applied row, so a partial apply left unmarked would restart into the OLD
	// bundle — the operator would do exactly what the console asked and nothing
	// would change.
	if err := a.store.MarkApplied(c.VersionID, time.Now()); err != nil {
		a.log.Error("cannot record the applied version", "err", err)
	}
	a.failedVersion = 0
	a.lastRestart = restart

	if len(restart) > 0 {
		a.log.Warn("configuration applied, but some changes need a restart",
			"version", c.Version, "version_id", c.VersionID, "changed", restart)
	} else {
		a.log.Info("configuration version applied", "version", c.Version, "version_id", c.VersionID)
	}
	a.refresh(nil)
	return restart, nil
}

// applyFromCache re-applies the cached configuration. This is SIGHUP in managed
// mode, the counterpart of re-reading the directory in file mode.
//
// Note it cannot rebind listeners or swap the relay table — those are bound for
// the life of the process. A gateway told restart_required needs an actual
// restart; this is for picking up a bundle that arrived while something else
// was wrong.
func (a *agent) applyFromCache(ctx context.Context) error {
	c := bootConfig(a.store, a.log)
	if c == nil {
		return errors.New("no configuration is cached")
	}
	_, err := a.applyCached(ctx, c)
	return err
}

// report tells the console what this gateway is actually running, and why not,
// if not. It is a full state snapshot rather than a patch.
func (a *agent) report(ctx context.Context, c *central.Client) {
	r := central.Report{Version: a.version}

	// The heartbeat rides the poll rather than a ticker of its own: this loop
	// already contacts the console and already describes what is applied, so a
	// second goroutine would double the request rate to say the same thing.
	//
	// It is therefore NOT on a fixed cadence — the loop also fires on a.wake
	// when the console pushes a change, so a burst of deploys produces a burst
	// of reports. That is harmless: each one is a full snapshot the console
	// overwrites, so there is nothing here worth rate-limiting.
	if a.metrics != nil {
		r.Metrics = a.metrics.Snapshot()
	}

	if cfg, err := a.store.AppliedConfig(); err == nil && cfg != nil {
		r.AppliedVersionID = &cfg.VersionID
	}
	// The failed version's message is what an operator needs; the applied row's
	// is empty by construction. AppliedVersionID and ApplyError are sent as
	// explicit nulls when absent — the console merges on `!== undefined`, so a
	// missing field cannot clear a stale value.
	if a.failedVersion != 0 {
		if f, err := a.store.ConfigByVersionID(a.failedVersion); err == nil && f != nil && f.ApplyError != "" {
			msg := f.ApplyError
			r.ApplyError = &msg
		}
	}
	// Always non-nil, for the same reason: RestartRequired carries omitempty,
	// so a nil pointer would leave a stale restart_required: true on the
	// console for the life of the row.
	restart := len(a.lastRestart) > 0
	r.RestartRequired = &restart

	if err := c.Report(ctx, r); err != nil && ctx.Err() == nil {
		a.logPollError(err)
	}
}

// bootConfig picks the bundle a managed gateway starts from.
//
// It is the version the console last asked for, whenever that version's bytes
// are cached. Using the console's recorded intent rather than inferring it is
// what makes two cases come out right:
//
//   - a deploy that reported restart_required boots into the NEW bundle, so the
//     restart the operator was told to perform actually does something;
//   - a rollback boots into the OLDER version it repointed at, rather than the
//     one the operator just rejected.
//
// Neither "most recently fetched" nor "most recently applied" answers both, and
// both timestamps have one-second resolution, which a deploy and the rollback
// undoing it routinely share.
//
// Falls back to the last configuration that applied cleanly, so a gateway that
// has never heard from a console still comes up on what it was running.
func bootConfig(st *store.Store, log *slog.Logger) *store.CachedConfig {
	applied, err := st.AppliedConfig()
	if err != nil {
		log.Error("cannot read the applied configuration", "err", err)
		return nil
	}

	desired, err := st.DesiredVersionID()
	if err != nil {
		log.Error("cannot read the desired version", "err", err)
		return applied
	}
	if desired == 0 {
		return applied
	}

	wanted, err := st.ConfigByVersionID(desired)
	if err != nil {
		log.Error("cannot read the config cache", "err", err)
		return applied
	}
	if wanted != nil {
		return wanted
	}
	// The console wants a version we have never successfully fetched. Run what
	// we have; the pull loop will collect it.
	return applied
}

// bootFromCache brings SMTP up from the cache, if there is anything to bring it
// up from. Nothing here is fatal: a gateway that cannot apply its cache is
// still worth running, because the next poll may hand it a configuration that
// works — and refusing to start would also take the wizard down with it.
func bootFromCache(ctx context.Context, a *agent, st *store.Store, log *slog.Logger) {
	cached := bootConfig(st, log)
	if cached == nil {
		log.Warn("no cached configuration yet; SMTP is not listening",
			"hint", "approve this gateway in the console, then deploy a configuration to it")
		return
	}

	if _, err := a.applyCached(ctx, cached); err == nil {
		return
	}

	// bootConfig may have offered a newer bundle than the one that last ran.
	// Fall back to the known-good one rather than sitting idle because the
	// newest deploy happens to be broken.
	applied, aerr := st.AppliedConfig()
	if aerr != nil || applied == nil || applied.VersionID == cached.VersionID {
		log.Error("the cached configuration did not apply; SMTP is not listening")
		return
	}
	log.Warn("falling back to the last configuration that applied cleanly",
		"version", applied.Version, "version_id", applied.VersionID)
	if _, err := a.applyCached(ctx, applied); err != nil {
		log.Error("the last-good configuration did not apply either; SMTP is not listening", "err", err)
	}
}

// truncate keeps an error message inside the console's schema limit without
// silently dropping the tail.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "… (truncated)"
}

// logPollError says the useful thing for the failures an operator can act on,
// and stays quiet about the ones that are simply "not yet".
func (a *agent) logPollError(err error) {
	switch {
	case errors.Is(err, central.ErrNotApproved), errors.Is(err, central.ErrNoConfig):
		// Waiting for an operator, or nothing deployed yet. Both are normal.
	case errors.Is(err, central.ErrClockSkew):
		a.log.Error("the console rejected our timestamp; check this gateway's clock (NTP)", "err", err)
	case errors.Is(err, central.ErrUnknownGateway):
		a.log.Warn("the console has forgotten this gateway; it must be registered again", "err", err)
	case errors.Is(err, central.ErrUnauthorized):
		a.log.Error("the console rejected our signature", "err", err)
	default:
		a.log.Warn("cannot reach Central Management; continuing on the current configuration", "err", err)
	}
}

// jittered spreads a fleet's polls by ±10%.
func jittered(d time.Duration) time.Duration {
	delta := float64(d) * 0.1
	return d + time.Duration((rand.Float64()*2-1)*delta)
}
