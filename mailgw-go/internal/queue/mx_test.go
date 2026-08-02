package queue

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// mxRunner builds a runner whose only relay resolves its exchange through DNS,
// with a resolver that always fails.
//
// No interface is needed: Resolver.Lookup is an exported function field, which
// is how internal/deliver's own tests fake DNS.
func mxRunner(t *testing.T, lookupErr error) *Runner {
	t.Helper()

	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Outbound": {{Name: "mx", Exchange: "partner.example", Port: 25, UseMX: true}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return NewRunner(newSpool(t), RunnerConfig{
		Relays:       tbl,
		PollInterval: time.Hour,
		Concurrency:  1,
		PerGroup:     1,
		MX: &deliver.Resolver{
			Lookup: func(context.Context, string) ([]*net.MX, error) { return nil, lookupErr },
		},
		Metrics: obs.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// requeued claims back the single envelope the attempt left in q/, looking past
// its backoff. The inflight name comes with it so a caller can run a second
// attempt on the same envelope.
func requeued(t *testing.T, r *Runner) (*Envelope, string) {
	t.Helper()
	names, _, _, err := r.spool.ReadyAndNext(time.Now().Add(365 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("%d envelopes requeued, want 1", len(names))
	}
	env, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return env, inflight
}

// When every member of a group is use_mx and DNS is down, targets() returns
// nothing, the relay loop never runs, and there is no *deliver.Result to explain
// the deferral. LastErr used to be left at the previous attempt's value — or, on
// a first attempt, blank — and it is the ONLY evidence in this case: no relay was
// contacted, so no recipient carries a LastMsg either. Both `mailq -json` and the
// eventual expiry DSN read it.
func TestTargets_ResolutionFailureReachesTheEnvelope(t *testing.T) {
	r := mxRunner(t, &net.DNSError{Err: "server misbehaving", Name: "partner.example",
		IsTemporary: true})

	attemptOnce(t, r)

	env, _ := requeued(t, r)
	if env.LastErr == "" {
		t.Fatal("the envelope was deferred with no reason at all")
	}
	if !strings.Contains(env.LastErr, "partner.example") {
		t.Errorf("LastErr = %q; it should name the domain that could not be resolved", env.LastErr)
	}
	if !strings.Contains(env.LastErr, "server misbehaving") {
		t.Errorf("LastErr = %q; it should carry the resolver's own message", env.LastErr)
	}
}

// The regression the switch has to survive: a SECOND failed attempt must report
// this attempt's reason, not keep whatever the first one left behind.
func TestTargets_ResolutionFailureIsNotStaleAcrossAttempts(t *testing.T) {
	r := mxRunner(t, &net.DNSError{Err: "first failure", Name: "partner.example"})
	attemptOnce(t, r)

	env, inflight := requeued(t, r)
	if !strings.Contains(env.LastErr, "first failure") {
		t.Fatalf("LastErr = %q after the first attempt", env.LastErr)
	}

	// Same envelope, a different DNS answer.
	r.cfg.MX = &deliver.Resolver{
		Lookup: func(context.Context, string) ([]*net.MX, error) {
			return nil, &net.DNSError{Err: "second failure", Name: "partner.example"}
		},
	}
	r.attempt(context.Background(), env, inflight)

	env, _ = requeued(t, r)
	if strings.Contains(env.LastErr, "first failure") {
		t.Errorf("LastErr = %q — the previous attempt's reason survived this one", env.LastErr)
	}
	if !strings.Contains(env.LastErr, "second failure") {
		t.Errorf("LastErr = %q, want this attempt's reason", env.LastErr)
	}
}

// A group whose members all resolve normally must be untouched: the error is
// only consulted when nothing could be built.
func TestTargets_SuccessfulResolutionReportsNoError(t *testing.T) {
	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Outbound": {{Name: "mx", Exchange: "partner.example", Port: 25, UseMX: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(newSpool(t), RunnerConfig{
		Relays:       tbl,
		PollInterval: time.Hour,
		Concurrency:  1,
		PerGroup:     1,
		MX: &deliver.Resolver{
			Lookup: func(context.Context, string) ([]*net.MX, error) {
				return []*net.MX{{Host: "mx1.partner.example.", Pref: 10}}, nil
			},
		},
		Metrics: obs.New(),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	group, _ := r.cfg.Relays.Lookup("Outbound")
	targets, terr := r.targets(context.Background(), group,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if terr != nil {
		t.Errorf("targets reported %v on a successful resolution", terr)
	}
	if len(targets) != 1 || targets[0].Exchange != "mx1.partner.example" {
		t.Errorf("targets = %+v, want the one exchanger", targets)
	}
}
