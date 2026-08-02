package queue

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// metricsRunner builds a runner over a relay group of n relays, with a counter
// registry attached.
//
// Several relays is the interesting case: the delivery counters are the one
// place in this package where "per relay", "per envelope-attempt" and "per
// recipient" all meet, and failover is where they are easiest to conflate.
func metricsRunner(t *testing.T, n int) (*Runner, *obs.Metrics) {
	t.Helper()

	group := make([]relays.Relay, 0, n)
	for i := range n {
		group = append(group, relays.Relay{
			Name: string(rune('a' + i)), Exchange: "127.0.0.1", Port: 25, Priority: i,
		})
	}
	tbl, err := relays.NewTable(map[string][]relays.Relay{"Outbound": group})
	if err != nil {
		t.Fatal(err)
	}

	m := obs.New()
	r := NewRunner(newSpool(t), RunnerConfig{
		Relays:       tbl,
		PollInterval: time.Hour,
		Concurrency:  1,
		PerGroup:     1,
		Metrics:      m,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return r, m
}

// attemptOnce enqueues an envelope and runs exactly one delivery attempt
// against it, synchronously.
//
// Calling attempt directly rather than running Start keeps these tests free of
// timing: the counters are the subject, not the scheduler. attempt's deferred
// Nudge is non-blocking on a buffered channel, so it is safe outside Start.
func attemptOnce(t *testing.T, r *Runner) {
	t.Helper()

	if err := r.spool.Enqueue(sample(r.spool, t, 1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("ReadyAndNext returned %d names, want 1", len(names))
	}
	env, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), env, inflight)
}

func check(t *testing.T, m *obs.Metrics, want map[string]int64) {
	t.Helper()
	got := m.Snapshot()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
}

// The guard M6 names explicitly: failover within one attempt must not multiply
// the deferral. Only the connection-failure counter is per relay.
func TestMetrics_DeferredCountedOnceAcrossRelayFailover(t *testing.T) {
	r, m := metricsRunner(t, 3)
	r.cfg.Deliver = func(_ context.Context, relay relays.Relay, _ deliver.Message, _ deliver.Options) *deliver.Result {
		return &deliver.Result{Host: relay.Exchange, Port: relay.Port.String(), Err: errFake{}}
	}

	attemptOnce(t, r)

	check(t, m, map[string]int64{
		"deliver_attempts": 1,
		"deliver_connfail": 3, // one per relay tried
		"deliver_deferred": 1, // one per attempt, however many relays
		"deliver_ok":       0,
		"deliver_bounced":  0,
	})
}

// The other half of the guard: a relay answered, some recipients were accepted
// and others deferred. record() ran this time, and the deferral is still one.
func TestMetrics_DeferredCountedOnceWhenSomeRecipientsDefer(t *testing.T) {
	r, m := metricsRunner(t, 1)

	e := sample(r.spool, t, 1)
	e.Rcpts = []Recipient{
		{Addr: "a@ngm.dev", Status: StatusPending},
		{Addr: "b@ngm.dev", Status: StatusPending},
	}
	if err := r.spool.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	r.cfg.Deliver = func(_ context.Context, relay relays.Relay, _ deliver.Message, _ deliver.Options) *deliver.Result {
		return &deliver.Result{
			Host: relay.Exchange, Port: relay.Port.String(),
			Rcpts: []deliver.RcptResult{
				{Addr: "a@ngm.dev", Outcome: deliver.OutcomeDelivered, Code: 250},
				{Addr: "b@ngm.dev", Outcome: deliver.OutcomeDeferred, Code: 451},
			},
		}
	}

	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	env, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), env, inflight)

	check(t, m, map[string]int64{
		"deliver_attempts": 1,
		"deliver_ok":       1, // per recipient
		"deliver_deferred": 1, // per attempt, not per pending recipient
		"deliver_connfail": 0,
		"deliver_bounced":  0,
	})
}

// Bounces are per recipient, and an envelope every relay rejected is finished
// rather than deferred.
func TestMetrics_BouncedIsPerRecipient(t *testing.T) {
	r, m := metricsRunner(t, 1)

	e := sample(r.spool, t, 1)
	e.Rcpts = []Recipient{
		{Addr: "a@ngm.dev", Status: StatusPending},
		{Addr: "b@ngm.dev", Status: StatusPending},
	}
	if err := r.spool.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	r.cfg.Deliver = func(_ context.Context, relay relays.Relay, _ deliver.Message, _ deliver.Options) *deliver.Result {
		return &deliver.Result{
			Host: relay.Exchange, Port: relay.Port.String(),
			Rcpts: []deliver.RcptResult{
				{Addr: "a@ngm.dev", Outcome: deliver.OutcomeRejected, Code: 550},
				{Addr: "b@ngm.dev", Outcome: deliver.OutcomeRejected, Code: 550},
			},
		}
	}

	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	env, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), env, inflight)

	check(t, m, map[string]int64{
		"deliver_attempts": 1,
		"deliver_bounced":  2,
		"deliver_deferred": 0, // every recipient resolved, so nothing requeued
		"deliver_ok":       0,
	})
}

// An envelope naming a group that no longer exists never reaches a relay. It is
// still an attempt: counting it anywhere else would let the deferred/attempts
// ratio exceed 1.
func TestMetrics_UnknownRelayGroupStillCountsAnAttempt(t *testing.T) {
	r, m := metricsRunner(t, 1)

	e := sample(r.spool, t, 1)
	e.RelayGrp = "GroupThatWasDeleted"
	if err := r.spool.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	env, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), env, inflight)

	check(t, m, map[string]int64{
		"deliver_attempts": 1,
		"deliver_deferred": 1,
		"deliver_connfail": 0,
	})
}

// A runner built without a registry must not panic on the delivery path.
func TestMetrics_NilRegistryIsSafe(t *testing.T) {
	r, _ := metricsRunner(t, 1)
	r.cfg.Metrics = nil
	r.cfg.Deliver = func(_ context.Context, relay relays.Relay, _ deliver.Message, _ deliver.Options) *deliver.Result {
		return &deliver.Result{Host: relay.Exchange, Port: relay.Port.String(), Err: errFake{}}
	}
	attemptOnce(t, r)
}
