package queue

import (
	"context"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// deferringDeliver answers every recipient 4xx from a relay that is otherwise
// perfectly healthy — greylisting, a rate limit, a full mailbox. Result.Err is
// nil, which is the combination the LastErr switch had no arm for.
func deferringDeliver(msg string) DeliverFunc {
	return func(_ context.Context, relay relays.Relay, m deliver.Message, _ deliver.Options) *deliver.Result {
		res := &deliver.Result{
			Relay: relay, Host: relay.Exchange, Port: relay.Port.String(),
		}
		for _, a := range m.Rcpts {
			res.Rcpts = append(res.Rcpts, deliver.RcptResult{
				Addr:    a,
				Outcome: deliver.OutcomeDeferred,
				Code:    451,
				Message: msg,
			})
		}
		return res
	}
}

// The relay answered and deferred everybody, so this attempt has its own reason
// and it must be the one recorded — mailq -json reads LastErr, and until M16 it
// reported the connection failure from the attempt before.
func TestLastErr_ADeferringRelayReplacesTheEarlierReason(t *testing.T) {
	r, _ := metricsRunner(t, 1)

	// Attempt one: nothing answers at all.
	r.cfg.Deliver = failingDeliver
	attemptOnce(t, r)

	env, inflight := requeued(t, r)
	if env.LastErr == "" {
		t.Fatal("the first attempt left no reason at all")
	}
	first := env.LastErr

	// Attempt two: the relay answers, and greylists.
	r.cfg.Deliver = deferringDeliver("4.7.1 greylisted, try again in 60s")
	r.attempt(context.Background(), env, inflight)

	env, _ = requeued(t, r)
	if env.LastErr == first {
		t.Errorf("LastErr = %q — the previous attempt's reason survived an "+
			"attempt that reached the relay", env.LastErr)
	}
	if !strings.Contains(env.LastErr, "greylisted") {
		t.Errorf("LastErr = %q, want the relay's own deferral message", env.LastErr)
	}
}

// The blank case: the same shape on a FIRST attempt left LastErr empty, so
// mailq showed a queued message with no explanation whatsoever.
func TestLastErr_ADeferringRelayIsRecordedOnTheFirstAttempt(t *testing.T) {
	r, _ := metricsRunner(t, 1)
	r.cfg.Deliver = deferringDeliver("4.2.2 mailbox full")

	attemptOnce(t, r)

	env, _ := requeued(t, r)
	if env.LastErr == "" {
		t.Fatal("a deferred envelope carries no reason on its first attempt")
	}
	if !strings.Contains(env.LastErr, "mailbox full") {
		t.Errorf("LastErr = %q, want the relay's own deferral message", env.LastErr)
	}
}

// A relay that says nothing useful still has to produce something an operator
// can act on.
func TestLastErr_ADeferralWithNoMessageStillNamesTheRelay(t *testing.T) {
	r, _ := metricsRunner(t, 1)
	r.cfg.Deliver = deferringDeliver("")

	attemptOnce(t, r)

	env, _ := requeued(t, r)
	if !strings.Contains(env.LastErr, "deferred by") {
		t.Errorf("LastErr = %q, want it to name the relay that deferred", env.LastErr)
	}
}
