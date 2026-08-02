package queue

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// The scheduler sleeps until the next envelope is actually owed rather than on
// a fixed tick, so a retry runs on time and an idle queue can be given a long
// rescan interval without delaying anything.

func TestReadyAndNext_ReportsTheEarliestFutureDueTime(t *testing.T) {
	s := newSpool(t)
	now := time.Now()

	due := []time.Duration{-time.Minute, 30 * time.Second, 5 * time.Second, time.Hour}
	for i, d := range due {
		e := sample(s, t, i+1)
		e.NextAt = now.Add(d).UnixMilli()
		if err := s.Enqueue(e); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	names, next, hasNext, err := s.ReadyAndNext(now)
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	if len(names) != 1 {
		t.Errorf("only the overdue envelope is ready, got %d", len(names))
	}
	if !hasNext {
		t.Fatal("three envelopes are still in the future")
	}
	// The filename carries a whole due-second, so a due time is truncated
	// downwards by up to 999ms: work can become ready slightly early, never
	// late. For a retry schedule that is the harmless direction.
	got := next.Sub(now)
	if got > 5*time.Second || got <= 4*time.Second {
		t.Errorf("next due in %s, want (4s, 5s] — the earliest of the three, truncated to the second", got)
	}
}

func TestReadyAndNext_NoFutureWork(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	e.NextAt = time.Now().Add(-time.Minute).UnixMilli()
	if err := s.Enqueue(e); err != nil {
		t.Fatal(err)
	}

	names, _, hasNext, err := s.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Errorf("got %d ready, want 1", len(names))
	}
	if hasNext {
		t.Error("nothing is scheduled for later, so there is no next due time")
	}
}

func TestReadyAndNext_EmptyQueue(t *testing.T) {
	s := newSpool(t)
	names, _, hasNext, err := s.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 || hasNext {
		t.Errorf("an empty queue has no work and no next due time, got %d/%v", len(names), hasNext)
	}
}

func testRunner(t *testing.T, poll time.Duration) *Runner {
	t.Helper()
	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Outbound": {{Name: "m", Exchange: "127.0.0.1", Port: 25}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewRunner(newSpool(t), RunnerConfig{
		Relays:       tbl,
		PollInterval: poll,
		Concurrency:  1,
		PerGroup:     1,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestSleepFor_UsesTheEarlierOfNextDueAndThePollCeiling(t *testing.T) {
	r := testRunner(t, 30*time.Second)

	// Work owed sooner than the ceiling wins: this is the whole point.
	if got := r.sleepFor(time.Now().Add(2*time.Second), true); got > 3*time.Second {
		t.Errorf("sleep %s, want about 2s", got)
	}
	// Work owed later than the ceiling does not extend the sleep, because
	// something may be dropped into q/ without going through Enqueue.
	if got := r.sleepFor(time.Now().Add(time.Hour), true); got != 30*time.Second {
		t.Errorf("sleep %s, want the 30s ceiling", got)
	}
	// Nothing queued: fall back to the ceiling.
	if got := r.sleepFor(time.Time{}, false); got != 30*time.Second {
		t.Errorf("sleep %s, want the 30s ceiling", got)
	}
}

// Overdue work must not spin the loop when no worker slot is free.
func TestSleepFor_HasAFloor(t *testing.T) {
	r := testRunner(t, 30*time.Second)
	if got := r.sleepFor(time.Now().Add(-time.Hour), true); got != minSleep {
		t.Errorf("sleep %s, want the %s floor", got, minSleep)
	}
}

func TestSleepFor_HandlesAnUnsetPollInterval(t *testing.T) {
	r := testRunner(t, 0)
	if got := r.sleepFor(time.Time{}, false); got <= 0 {
		t.Errorf("sleep %s must be positive even with no interval configured", got)
	}
}

// The behaviour that matters: an envelope deferred by a short delay is retried
// on time, even though the rescan ceiling is far longer than the delay. Before
// this change the retry waited for the next tick.
func TestRunner_RetriesWhenDueNotWhenTheCeilingExpires(t *testing.T) {
	r := testRunner(t, time.Hour) // a ceiling that must never be reached

	var attempts atomic.Int32
	r.cfg.Backoff = func(int) time.Duration { return 300 * time.Millisecond }
	r.cfg.Jitter = 0
	r.cfg.Deliver = func(_ context.Context, relay relays.Relay, _ deliver.Message, _ deliver.Options) *deliver.Result {
		attempts.Add(1)
		// A connection-level failure defers the envelope for one backoff.
		return &deliver.Result{Host: relay.Exchange, Port: relay.Port.String(), Err: errFake{}}
	}

	e := sample(r.spool, t, 1)
	if err := r.spool.Enqueue(e); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wait on Start itself, not on r.wg: the WaitGroup tracks delivery
	// attempts, so it can be empty while the scheduler loop is still reading
	// the spool. Letting it run past the end of the test races with the
	// t.TempDir cleanup that deletes that spool.
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx) }()

	// First attempt is immediate, the retry is owed 300ms later. Allowing 2s
	// is generous for the retry yet nowhere near the one-hour ceiling.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && attempts.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("expected the retry to fire on its due time; got %d attempts in 2s "+
			"with a 1h poll interval", got)
	}

	cancel()
	<-done
}

type errFake struct{}

func (errFake) Error() string { return "relay unreachable" }
