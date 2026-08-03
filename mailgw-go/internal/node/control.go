package node

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// This file is the surface internal/testctl drives, and nothing else uses it.
//
// It lives here rather than in testctl because everything below it is
// unexported: the gateway, the agent and the store handle.
//
// It costs the shipped binary nothing. These are methods on a type cmd/mailgw-go
// already links, and the containment that matters runs the other way —
// cmd/mailgw-go must not import internal/testctl, which is what CI checks.
//
// Every method is a thin adapter over a path the production code already takes.
// ApplyBundle in particular does NOT reimplement applying: it writes a cache row
// and calls the same agent.applyCached that boot, the poll loop, the WebSocket
// and SIGHUP all call. A test that injected a configuration through a second
// apply path would be testing something no console can produce.

// Applied is the outcome of injecting a configuration.
type Applied struct {
	VersionID       int64    `json:"version_id"`
	Version         int      `json:"version"`
	SHA256          string   `json:"sha256"`
	RestartRequired []string `json:"restart_required"`
}

// Status is what a test needs to know about a running node.
type Status struct {
	Build            string   `json:"build"`
	Version          string   `json:"version"`
	Commit           string   `json:"commit"`
	Fingerprint      string   `json:"fingerprint"`
	GatewayUID       string   `json:"gateway_uid"`
	CentralURL       string   `json:"central_url"`
	Approval         string   `json:"approval"`
	Provisioned      bool     `json:"provisioned"`
	Serving          bool     `json:"serving"`
	AppliedVersion   *int     `json:"applied_version"`
	AppliedVersionID int64    `json:"applied_version_id"`
	ApplyError       string   `json:"apply_error"`
	LastError        string   `json:"last_error"`
	RestartRequired  []string `json:"restart_required"`
	// Listeners are the addresses SMTP is actually bound to, which is not what
	// the configuration asked for whenever it asked for port 0. This is the
	// field that lets a test bind an ephemeral port and still find it.
	Listeners []string `json:"listeners"`
	// AdminAddr is the same answer for the admin listener, and exists for the
	// same reason: /metrics, /readyz and /healthz live there, and a harness that
	// asked for :0 has no other way to reach them.
	AdminAddr string `json:"admin_addr"`
	SpoolDir  string `json:"spool_dir"`
}

// QueueEntry is one envelope in the spool.
//
// Deliberately not the same shape as `mailq -json`: that output is an operator
// contract with its own column choices, and coupling a test API to it would
// make a cosmetic change to one a breaking change to the other.
type QueueEntry struct {
	Queue     string   `json:"queue"`
	Name      string   `json:"name"`
	UUID      string   `json:"uuid"`
	Sender    string   `json:"sender"`
	Rcpts     []string `json:"rcpts"`
	RelayGrp  string   `json:"relay_group"`
	Attempts  int      `json:"attempts"`
	LastErr   string   `json:"last_error,omitempty"`
	Due       string   `json:"due,omitempty"`
	ParseErr  string   `json:"parse_error,omitempty"`
	BodyBytes int64    `json:"body_bytes,omitempty"`
}

// ApplyBundle makes raw the running configuration, synchronously.
//
// Synchronous is the whole point. Deploying through the console means waiting
// for a WebSocket notification or a 15s poll and then reading a log to find out
// whether it took; here the compiler's error is the HTTP response.
//
// ctx bounds THIS CALL. It is deliberately not the context the bring-up
// attaches its goroutines to: applyCached's context governs the lifetime of the
// delivery runner and the failed-events replayer, and every other caller — boot,
// the poll loop, the WebSocket, SIGHUP — passes one that lives as long as the
// process. This one comes from an HTTP handler and dies with the response, so
// passing it through cancelled the runner the instant the first configuration
// was injected: the gateway accepted mail and delivered none of it. n.serve()
// is the process-lifetime context instead.
//
// The version id is derived from the bundle's own digest and made negative.
// Negative because console version ids are a positive autoincrement, so the two
// spaces can never collide and bootConfig keeps working across a restart.
// Derived rather than counted because it makes re-injecting an unchanged bundle
// idempotent — the same row is upserted rather than a new one piled on — which
// is the behaviour the console already has for redeploying unchanged config.
func (n *Node) ApplyBundle(ctx context.Context, raw []byte) (Applied, error) {
	// The caller's context still decides whether this call happens at all — a
	// harness that gave up waiting should not have its configuration land a
	// second later.
	if err := ctx.Err(); err != nil {
		return Applied{}, err
	}
	if len(raw) == 0 {
		return Applied{}, errors.New("empty bundle")
	}
	// Rejected here as well as in ParseBundle so the caller gets "not JSON"
	// rather than a complaint about a missing format field.
	if !json.Valid(raw) {
		return Applied{}, errors.New("bundle is not valid JSON")
	}

	sum := sha256.Sum256(raw)
	cached := &store.CachedConfig{
		VersionID: injectedVersionID(sum),
		Version:   n.nextInjectedVersion(),
		SHA256:    hex.EncodeToString(sum[:]),
		Bundle:    raw,
		FetchedAt: time.Now(),
	}

	// Saved before it is applied, exactly as the pull loop does: a bundle that
	// fails to apply must still be on disk afterwards, or its apply_error has
	// nowhere to be recorded and the failure is unexplainable.
	if err := n.store.SaveConfig(cached); err != nil {
		return Applied{}, fmt.Errorf("cache the injected bundle: %w", err)
	}
	restart, err := n.agent.applyCached(n.serve(), cached)
	if err != nil {
		return Applied{}, err
	}

	// Recorded only after it applied, which is where this deliberately differs
	// from the pull loop. There, desired_version_id is the CONSOLE's intent and
	// is authoritative even for a bundle that fails — an operator has to be able
	// to see what they asked for. Here the caller is the test, it already has
	// the error in its hand, and leaving the pointer on a bundle that will never
	// apply would make the next restart come up on a failure and fall back.
	if err := n.store.SetDesiredVersionID(cached.VersionID); err != nil {
		return Applied{}, fmt.Errorf("record the desired version: %w", err)
	}

	if restart == nil {
		// An empty array rather than null: every caller in every language would
		// otherwise have to special-case it.
		restart = []string{}
	}
	return Applied{
		VersionID:       cached.VersionID,
		Version:         cached.Version,
		SHA256:          cached.SHA256,
		RestartRequired: restart,
	}, nil
}

// injectedVersionID maps a bundle digest onto a negative int64.
//
// The top bit is cleared before negating so the result is never math.MinInt64,
// which has no positive counterpart and would make any later arithmetic on it
// surprising. Zero is stepped past because 0 means "no desired version".
func injectedVersionID(sum [32]byte) int64 {
	v := int64(binary.BigEndian.Uint64(sum[:8]) &^ (1 << 63))
	if v == 0 {
		v = 1
	}
	return -v
}

// nextInjectedVersion is a display number for logs and the status page.
//
// In memory and therefore reset by a restart, which is acceptable because it is
// cosmetic: the version id above is the identity, and it is stable.
func (n *Node) nextInjectedVersion() int {
	n.injected++
	return n.injected
}

// ApplyProfiles composes a bundle from the profile texts an operator would type
// into the console, then applies it.
//
// A convenience over ApplyBundle and not a second format: the struct below is
// assembled into config.Bundle and handed to the same code. It exists because
// tests/provision.ts already carries these texts as inline constants, so a test
// can use the same strings rather than learning the bundle wire shape.
type Profiles struct {
	Server    string                    `json:"server"`
	Routing   string                    `json:"routing"`
	Allowlist json.RawMessage           `json:"allowlist"`
	Relays    map[string][]RelayProfile `json:"relays"`
	Logging   *config.Logging           `json:"logging"`
	Admin     *config.Admin             `json:"admin"`
	Auth      *config.Auth              `json:"auth"`
}

// RelayProfile mirrors relays.Relay's JSON, kept as a raw map so this file does
// not have to track every field that package grows.
type RelayProfile = json.RawMessage

// Bundle renders the profiles as the bundle document the console would send.
func (p Profiles) Bundle() ([]byte, error) {
	doc := map[string]any{"format": config.BundleFormat}
	if p.Server != "" {
		doc["server"] = p.Server
	}
	if p.Routing != "" {
		doc["routing"] = p.Routing
	}
	if len(p.Allowlist) > 0 {
		doc["allowlist"] = p.Allowlist
	}
	if p.Relays != nil {
		doc["relays"] = p.Relays
	}
	// Logging is not omitted when empty: a gateway with no logservice URLs is a
	// legal configuration and the zero value is what expresses it.
	if p.Logging != nil {
		doc["logging"] = *p.Logging
	} else {
		doc["logging"] = config.Logging{}
	}
	if p.Admin != nil {
		doc["admin"] = *p.Admin
	}
	if p.Auth != nil {
		doc["auth"] = *p.Auth
	}
	return json.Marshal(doc)
}

// AppliedBundle is the bundle currently cached, unredacted.
//
// Unredacted deliberately: this is a build whose whole purpose is to be
// inspected by a test, and a test asserting on a relay password it just
// injected cannot do so against "[redacted]". `config show` keeps redacting,
// because that is the command an operator pastes into an issue.
func (n *Node) AppliedBundle() (*store.CachedConfig, error) {
	c, err := n.store.AppliedConfig()
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("no configuration has been applied yet")
	}
	return c, nil
}

// Enroll points this gateway at a Central Management console and registers it.
//
// It calls the same agent.Register the wizard's POST /register calls, so what
// happens on the console is identical: the row lands pending and an operator
// (or a test) still has to approve the fingerprint, assign profiles and deploy.
//
// It does not present the claim code. M12's gate protects the admin UI, which
// is a browser surface; this is a different binary with no browser and no
// session. Nothing in the registration path is weakened — the console still
// verifies the Ed25519 signature and still refuses to reset an approval.
func (n *Node) Enroll(ctx context.Context, s adminui.Settings) error {
	return n.agent.Register(ctx, s)
}

// Status reports what this node is doing.
func (n *Node) Status() Status {
	st := n.agent.State()
	out := Status{
		Build:           "test-only",
		Version:         n.opts.Version,
		Commit:          n.opts.Commit,
		Fingerprint:     st.Fingerprint,
		GatewayUID:      st.GatewayUID,
		CentralURL:      st.CentralURL,
		Approval:        st.Approval,
		Provisioned:     st.CentralURL != "" && st.GatewayUID != "",
		Serving:         st.Serving,
		AppliedVersion:  st.AppliedVersion,
		ApplyError:      st.ApplyError,
		LastError:       st.LastError,
		RestartRequired: st.RestartRequired,
		Listeners:       n.gw.listeners.addrs(),
	}
	if n.ui != nil {
		out.AdminAddr = n.ui.Addr()
	}
	if c, err := n.store.AppliedConfig(); err == nil && c != nil {
		out.AppliedVersionID = c.VersionID
	}
	if sp := n.gw.Spool(); sp != nil {
		out.SpoolDir = sp.Root()
	}
	if out.RestartRequired == nil {
		out.RestartRequired = []string{}
	}
	if out.Listeners == nil {
		out.Listeners = []string{}
	}
	return out
}

// Queue lists every envelope in the spool.
func (n *Node) Queue() ([]QueueEntry, error) {
	sp := n.gw.Spool()
	if sp == nil {
		return nil, errors.New("no spool: no configuration has been applied yet")
	}
	entries, err := sp.ListAll()
	if err != nil {
		return nil, err
	}

	out := make([]QueueEntry, 0, len(entries))
	for _, e := range entries {
		q := QueueEntry{Queue: e.Queue, Name: e.Name, UUID: e.UUID()}
		if !e.Due.IsZero() {
			q.Due = e.Due.UTC().Format(time.RFC3339)
		}
		if e.Err != nil {
			// Reported rather than skipped: a torn or foreign file in the spool
			// is exactly what a test asserting on crash recovery is looking for.
			q.ParseErr = e.Err.Error()
			out = append(out, q)
			continue
		}
		if e.Env != nil {
			q.Sender = e.Env.MailFrom
			q.RelayGrp = e.Env.RelayGrp
			q.Attempts = e.Env.Attempts
			q.LastErr = e.Env.LastErr
			q.BodyBytes = e.Env.BodySize
			for _, r := range e.Env.Rcpts {
				q.Rcpts = append(q.Rcpts, r.Addr)
			}
		}
		out = append(out, q)
	}
	return out, nil
}

// Flush makes envelopes due now and wakes the delivery scheduler.
//
// With no uuids it flushes everything in the ready queue. This is what replaces
// sleeping through a retry backoff in an end-to-end test: the schedule is real
// and correct, and a test that waited it out would be asserting on the clock.
func (n *Node) Flush(uuids []string) (int, error) {
	sp := n.gw.Spool()
	if sp == nil {
		return 0, errors.New("no spool: no configuration has been applied yet")
	}

	if len(uuids) == 0 {
		entries, err := sp.ListAll()
		if err != nil {
			return 0, err
		}
		for _, e := range entries {
			if e.Queue == queue.QueueReady {
				uuids = append(uuids, e.UUID())
			}
		}
	}

	n_ok := 0
	var firstErr error
	for _, u := range uuids {
		if err := sp.Flush(u); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n_ok++
	}
	// Nudged even when some flushes failed: the ones that succeeded are due now
	// and there is no reason to make them wait for the poll ceiling.
	if n.gw.runner != nil {
		n.gw.runner.Nudge()
	}
	return n_ok, firstErr
}

// Release moves held envelopes out of quarantine and back into the ready queue.
//
// Quarantine is a decision the ruleset can make and `mailgw-go mailq release` is
// its only exit — a CLI this binary deliberately does not carry. Without this a
// test would have to reach into the spool directory itself, which would make the
// on-disk filename format a contract QueueEntry's own comment refuses to make.
func (n *Node) Release(uuids []string) (int, error) {
	return n.eachEnvelope(uuids, (*queue.Spool).Release, true)
}

// Hold moves ready envelopes into quarantine, the inverse of Release.
func (n *Node) Hold(uuids []string) (int, error) {
	return n.eachEnvelope(uuids, (*queue.Spool).Hold, false)
}

// Remove deletes envelopes and collects their bodies.
//
// This is how a test drops mail it deliberately left undeliverable so the next
// one starts from a known queue. The alternative is a restart per test, which is
// an order of magnitude more expensive for a call that already exists.
func (n *Node) Remove(uuids []string) (int, error) {
	return n.eachEnvelope(uuids, (*queue.Spool).Remove, false)
}

// eachEnvelope applies one spool operation to each uuid, reporting how many
// succeeded alongside the first failure.
//
// Partial success is real and is reported rather than collapsed: some uuids may
// have moved before one was found in flight, and a caller told only "error" would
// have no idea what state the queue is in.
//
// An empty list is an error here, where Flush treats it as "everything ready".
// The asymmetry is deliberate: "flush the queue" is a gesture an operator makes
// after an outage, and "release everything ever quarantined" is not a gesture
// anybody wants to make by accident.
func (n *Node) eachEnvelope(uuids []string, op func(*queue.Spool, string) error, nudge bool) (int, error) {
	sp := n.gw.Spool()
	if sp == nil {
		return 0, errors.New("no spool: no configuration has been applied yet")
	}
	if len(uuids) == 0 {
		return 0, errors.New("uuids is required and must not be empty")
	}

	ok := 0
	var firstErr error
	for _, u := range uuids {
		if err := op(sp, u); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
	}
	// Nudged even when some failed, for the same reason Flush is: what did move
	// is due now, and there is no reason to make it wait for the poll ceiling.
	if nudge && ok > 0 && n.gw.runner != nil {
		n.gw.runner.Nudge()
	}
	return ok, firstErr
}

// Reset returns this node to the state a fresh data volume would give it:
// no cached configuration and no console.
//
// What it CANNOT do is stop serving. Listeners, the relay table and the spool
// are bound for the life of the process — that is the same constraint
// restartRequired exists to report, and pretending otherwise here would give a
// test a false negative. The caller is told a restart is required; in practice a
// test resets and then injects, which swaps the rules and the allowlist and is
// what it wanted anyway.
func (n *Node) Reset(ctx context.Context) error {
	if err := n.store.ClearConfigCache(); err != nil {
		return err
	}
	if err := n.store.SetDesiredVersionID(0); err != nil {
		return err
	}
	return n.store.SetCentralURL("")
}
