package queue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// failingDeliver is a relay that will not talk to us — a connection-level
// failure, so the runner tries the next relay and ultimately defers.
func failingDeliver(_ context.Context, relay relays.Relay, _ deliver.Message, _ deliver.Options) *deliver.Result {
	return &deliver.Result{Host: relay.Exchange, Port: relay.Port.String(), Err: errFake{}}
}

// enqueueAged puts an envelope in the queue that was accepted `age` ago, and
// runs one attempt against it.
func attemptAged(t *testing.T, r *Runner, age time.Duration) {
	t.Helper()

	env := sample(r.spool, t, 1)
	env.QueuedAt = time.Now().Add(-age).UnixMilli()
	if err := r.spool.Enqueue(env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil || len(names) != 1 {
		t.Fatalf("ReadyAndNext = %v, %v; want one envelope", names, err)
	}
	claimed, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), claimed, inflight)
}

func countDir(t *testing.T, s *Spool, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.Root(), dir))
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	return len(ents)
}

// An envelope whose relay group has been renamed out of the configuration
// returns from attempt long before the tail of the function. The lifetime check
// used to live at that tail, so such an envelope was never once compared against
// max_lifetime: it retried forever, never expired, never bounced, never reached
// dead/. Expiry now lives in deferEnvelope, which every non-terminal path uses.
func TestExpiry_UnknownRelayGroupStillExpires(t *testing.T) {
	r, m := metricsRunner(t, 1)
	r.cfg.MaxLifetime = time.Hour

	// Claim the envelope, then point it at a group the table does not have.
	env := sample(r.spool, t, 1)
	env.QueuedAt = time.Now().Add(-2 * time.Hour).UnixMilli()
	env.RelayGrp = "Outbound"
	if err := r.spool.Enqueue(env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil || len(names) != 1 {
		t.Fatalf("ReadyAndNext = %v, %v", names, err)
	}
	claimed, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed.RelayGrp = "GroupThatWasRenamed"

	r.attempt(context.Background(), claimed, inflight)

	if n := countDir(t, r.spool, dirDead); n != 1 {
		t.Errorf("dead/ holds %d envelopes, want 1 — the envelope never expired", n)
	}
	if n := countDir(t, r.spool, dirReady); n != 0 {
		t.Errorf("q/ holds %d envelopes, want 0 — it was requeued instead of given up on", n)
	}
	check(t, m, map[string]int64{"env_expired": 1})
}

// The ordinary path: every relay refused, and the message is past its lifetime.
func TestExpiry_BuriesAndMarksRecipientsExpired(t *testing.T) {
	r, m := metricsRunner(t, 1)
	r.cfg.MaxLifetime = time.Hour
	r.cfg.Deliver = failingDeliver

	attemptAged(t, r, 2*time.Hour)

	if n := countDir(t, r.spool, dirDead); n != 1 {
		t.Fatalf("dead/ holds %d envelopes, want 1", n)
	}
	ents, _ := os.ReadDir(filepath.Join(r.spool.Root(), dirDead))
	buried, err := readEnvelope(filepath.Join(r.spool.Root(), dirDead, ents[0].Name()))
	if err != nil {
		t.Fatalf("read buried envelope: %v", err)
	}
	for _, rc := range buried.Rcpts {
		if rc.Status != StatusExpired {
			t.Errorf("recipient %s status = %q, want %q", rc.Addr, rc.Status, StatusExpired)
		}
	}
	check(t, m, map[string]int64{"env_expired": 1, "deliver_deferred": 1})
}

// Inside its lifetime, the same failure defers rather than expiring.
func TestExpiry_YoungEnvelopeIsRequeued(t *testing.T) {
	r, m := metricsRunner(t, 1)
	r.cfg.MaxLifetime = time.Hour
	r.cfg.Deliver = failingDeliver

	attemptAged(t, r, time.Minute)

	if n := countDir(t, r.spool, dirDead); n != 0 {
		t.Errorf("dead/ holds %d envelopes, want 0", n)
	}
	if n := countDir(t, r.spool, dirReady); n != 1 {
		t.Errorf("q/ holds %d envelopes, want 1", n)
	}
	check(t, m, map[string]int64{"env_expired": 0, "deliver_deferred": 1})
}

// max_lifetime unset means retry indefinitely, which must keep working.
func TestExpiry_ZeroLifetimeNeverExpires(t *testing.T) {
	r, m := metricsRunner(t, 1)
	r.cfg.MaxLifetime = 0
	r.cfg.Deliver = failingDeliver

	attemptAged(t, r, 10000*time.Hour)

	if n := countDir(t, r.spool, dirDead); n != 0 {
		t.Errorf("dead/ holds %d envelopes, want 0", n)
	}
	check(t, m, map[string]int64{"env_expired": 0})
}
