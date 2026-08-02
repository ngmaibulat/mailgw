package ratelimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is the injected time source every test here drives by hand.
//
// The plan named this: reading the wall clock inside the limiter is what makes
// window tests flaky, so the dependency is taken from the start. Nothing in this
// file sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// take runs n events against one key and returns how many were allowed.
func take(l *Limiter, dim Dimension, id string, n int) int {
	allowed := 0
	for range n {
		if l.Allow(dim, id) {
			allowed++
		}
	}
	return allowed
}

func TestRule_Enabled(t *testing.T) {
	cases := []struct {
		rule Rule
		want bool
	}{
		{Rule{}, false},
		{Rule{Rate: 10}, false},                   // no window
		{Rule{Per: time.Minute}, false},           // no rate
		{Rule{Rate: 10, Per: time.Minute}, true},  //
		{Rule{Rate: -1, Per: time.Minute}, false}, //
	}
	for _, c := range cases {
		if got := c.rule.Enabled(); got != c.want {
			t.Errorf("%+v.Enabled() = %v, want %v", c.rule, got, c.want)
		}
	}
}

// TestAllow_BurstThenSustainedRate is the shape of the whole package: a bucket
// admits a burst up to its capacity, then admits at the refill rate.
func TestAllow_BurstThenSustainedRate(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 60, Per: time.Minute}}, c.now)

	// Burst defaults to Rate — a full window's worth, which is what "60 per
	// minute" means to the operator who wrote it.
	if got := take(l, ConnPerIP, "10.0.0.1", 100); got != 60 {
		t.Fatalf("initial burst allowed %d, want 60", got)
	}
	if l.Allow(ConnPerIP, "10.0.0.1") {
		t.Fatal("the 61st event was allowed with an empty bucket")
	}

	// 60/min refills one per second.
	c.advance(time.Second)
	if !l.Allow(ConnPerIP, "10.0.0.1") {
		t.Error("one token should have refilled after a second")
	}
	if l.Allow(ConnPerIP, "10.0.0.1") {
		t.Error("only one token should have refilled")
	}

	c.advance(10 * time.Second)
	if got := take(l, ConnPerIP, "10.0.0.1", 20); got != 10 {
		t.Errorf("after 10s allowed %d, want 10", got)
	}
}

// TestAllow_RefillIsCappedAtBurst: an idle key does not accumulate an unbounded
// allowance it can spend all at once later.
func TestAllow_RefillIsCappedAtBurst(t *testing.T) {
	c := newClock()
	l := New(Rules{MsgPerSender: Rule{Rate: 10, Per: time.Minute}}, c.now)

	take(l, MsgPerSender, "a@ngm.dev", 10)
	c.advance(24 * time.Hour)

	if got := take(l, MsgPerSender, "a@ngm.dev", 100); got != 10 {
		t.Errorf("after a day idle allowed %d, want the burst of 10", got)
	}
}

func TestAllow_ExplicitBurst(t *testing.T) {
	c := newClock()
	// A low sustained rate with room for a spike: 60/hour, but 20 at once.
	l := New(Rules{MsgPerSender: Rule{Rate: 60, Per: time.Hour, Burst: 20}}, c.now)

	if got := take(l, MsgPerSender, "a@ngm.dev", 100); got != 20 {
		t.Fatalf("burst allowed %d, want 20", got)
	}
	c.advance(time.Minute) // 60/hour == 1/minute
	if got := take(l, MsgPerSender, "a@ngm.dev", 5); got != 1 {
		t.Errorf("after a minute allowed %d, want 1", got)
	}
}

// TestAllow_KeysAreIndependent: one noisy peer must not spend anybody else's
// allowance. This is the property that lets the limiter live INSIDE the
// allowlist while the concurrency cap has to live outside it.
func TestAllow_KeysAreIndependent(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 5, Per: time.Minute}}, c.now)

	take(l, ConnPerIP, "10.0.0.1", 10)
	if !l.Allow(ConnPerIP, "10.0.0.2") {
		t.Error("a second peer was refused because the first exhausted its bucket")
	}
}

// TestAllow_DimensionsAreIndependent: a sender address and an authenticated
// username are routinely the same string, and they must not share a bucket.
func TestAllow_DimensionsAreIndependent(t *testing.T) {
	c := newClock()
	l := New(Rules{
		MsgPerSender: Rule{Rate: 2, Per: time.Minute},
		MsgPerUser:   Rule{Rate: 2, Per: time.Minute},
	}, c.now)

	take(l, MsgPerSender, "app@ngm.dev", 2)
	if !l.Allow(MsgPerUser, "app@ngm.dev") {
		t.Error("the same id in a different dimension shared a bucket")
	}
}

// TestAllow_DisabledAndNil: every way of saying "not limited" answers true.
func TestAllow_DisabledAndNil(t *testing.T) {
	c := newClock()
	l := New(Rules{}, c.now)

	if got := take(l, ConnPerIP, "10.0.0.1", 1000); got != 1000 {
		t.Errorf("a disabled rule refused %d events", 1000-got)
	}
	if l.Len() != 0 {
		t.Errorf("a disabled rule allocated %d buckets", l.Len())
	}

	var nilLimiter *Limiter
	if !nilLimiter.Allow(ConnPerIP, "10.0.0.1") {
		t.Error("a nil limiter refused an event")
	}
	if nilLimiter.Len() != 0 || nilLimiter.Rules().Any() {
		t.Error("a nil limiter should be inert")
	}
}

// TestAllow_EmptyIDIsNeverLimited. An id the caller could not determine — a null
// sender, an address with no domain — must not fall into one shared bucket:
// that bucket would be the busiest key on the gateway and would start refusing
// unrelated mail.
func TestAllow_EmptyIDIsNeverLimited(t *testing.T) {
	c := newClock()
	l := New(Rules{MsgPerSender: Rule{Rate: 1, Per: time.Hour}}, c.now)

	if got := take(l, MsgPerSender, "", 100); got != 100 {
		t.Errorf("an empty id was limited (%d of 100 allowed)", got)
	}
	if l.Len() != 0 {
		t.Errorf("an empty id allocated %d buckets", l.Len())
	}
}

// ── eviction ─────────────────────────────────────────────────────────────────
//
// The plan is explicit that a test which only checks refusals passes against an
// unbounded map, so these assert the map itself.

// TestEviction_FullBucketsAreDropped is the argument for using buckets at all: a
// bucket refilled to capacity is byte-for-byte what Allow would create for that
// key, so forgetting it cannot admit an event that would otherwise be refused.
func TestEviction_FullBucketsAreDropped(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 10, Per: time.Minute}, MaxKeys: 4}, c.now)

	for i := range 4 {
		l.Allow(ConnPerIP, fmt.Sprintf("10.0.0.%d", i))
	}
	if l.Len() != 4 {
		t.Fatalf("tracked %d keys, want 4", l.Len())
	}

	// Long enough for every bucket to refill completely.
	c.advance(time.Minute)

	// The next new key finds the map full, sweeps, and fits.
	if !l.Allow(ConnPerIP, "10.0.0.99") {
		t.Fatal("a new key was refused after a sweep should have made room")
	}
	if l.Len() != 1 {
		t.Errorf("after the sweep %d keys are tracked, want 1", l.Len())
	}
}

// TestEviction_PartialBucketsSurvive: the sweep must not release a peer that is
// still being limited, which is the failure a naive "clear the map when it is
// full" would have.
func TestEviction_PartialBucketsSurvive(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 10, Per: time.Hour}, MaxKeys: 3}, c.now)

	// Exhaust one peer, and leave two others full.
	take(l, ConnPerIP, "10.0.0.1", 10)
	l.Allow(ConnPerIP, "10.0.0.2")
	l.Allow(ConnPerIP, "10.0.0.3")

	// Enough time for 9 of the drained peer's 10 tokens, but not all ten — so a
	// correct sweep keeps its bucket while dropping the two that are full.
	c.advance(time.Hour / 10 * 9)

	// A new key finds the map full and triggers the sweep.
	l.Allow(ConnPerIP, "10.0.0.99")

	// The drained peer must have 9, not the 10 a reset would have given it.
	if got := take(l, ConnPerIP, "10.0.0.1", 20); got != 9 {
		t.Errorf("the partially-refilled peer got %d events, want 9 — "+
			"10 would mean the sweep reset a bucket that was still limiting", got)
	}
}

// TestEviction_MapNeverExceedsMaxKeys is the bound itself. Without eviction this
// would grow to 500.
func TestEviction_MapNeverExceedsMaxKeys(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 1, Per: time.Second}, MaxKeys: 50}, c.now)

	for i := range 500 {
		l.Allow(ConnPerIP, fmt.Sprintf("10.0.%d.%d", i/256, i%256))
		if l.Len() > 50 {
			t.Fatalf("after %d keys the map holds %d, over MaxKeys=50", i+1, l.Len())
		}
	}
}

// TestEviction_FullMapAdmitsRatherThanRefuses. When every tracked bucket is
// genuinely in use there is nowhere to record a new one — and the answer has to
// be "allow". Refusing what cannot be tracked turns a memory ceiling into a mail
// outage, and hands an attacker a way to deny service to everyone by filling the
// map with keys of their own choosing.
func TestEviction_FullMapAdmitsRatherThanRefuses(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 5, Per: time.Hour}, MaxKeys: 2}, c.now)

	take(l, ConnPerIP, "10.0.0.1", 5) // drained
	take(l, ConnPerIP, "10.0.0.2", 5) // drained
	if l.Len() != 2 {
		t.Fatalf("tracked %d keys, want 2", l.Len())
	}

	if !l.Allow(ConnPerIP, "10.0.0.3") {
		t.Error("an untrackable peer was refused; the memory ceiling became a mail outage")
	}
	// The two peers that ARE tracked stay limited.
	if l.Allow(ConnPerIP, "10.0.0.1") {
		t.Error("a drained peer was released")
	}
}

// TestSetRules_BucketsSurvive: an operator adjusting a limit during an incident
// must not thereby release the peer they are adjusting it for. Rebuilding the
// limiter on apply — the simpler implementation — would do exactly that.
func TestSetRules_BucketsSurvive(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 5, Per: time.Hour}}, c.now)

	take(l, ConnPerIP, "10.0.0.1", 5)
	if l.Allow(ConnPerIP, "10.0.0.1") {
		t.Fatal("setup: the peer should be drained")
	}

	// An unrelated deploy retunes a different dimension.
	l.SetRules(Rules{
		ConnPerIP:    Rule{Rate: 5, Per: time.Hour},
		MsgPerSender: Rule{Rate: 10, Per: time.Minute},
	})
	if l.Allow(ConnPerIP, "10.0.0.1") {
		t.Error("a routine deploy handed a drained peer a fresh allowance")
	}
}

// TestSetRules_DisablingFreesMemory: a limit switched off should stop holding
// buckets, rather than keeping them until something happens to sweep.
func TestSetRules_DisablingFreesMemory(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 5, Per: time.Hour}}, c.now)

	take(l, ConnPerIP, "10.0.0.1", 5)
	if l.Len() == 0 {
		t.Fatal("setup: expected a tracked bucket")
	}

	l.SetRules(Rules{})
	if l.Len() != 0 {
		t.Errorf("%d buckets survived the rule being disabled", l.Len())
	}
	if got := take(l, ConnPerIP, "10.0.0.1", 100); got != 100 {
		t.Errorf("a disabled rule still refused %d events", 100-got)
	}
}

// TestSetRules_RaisingSpeedsRefillRatherThanRefilling pins the consequence an
// operator is most likely to meet in production: a limit raised during an
// incident gives faster relief, not instant relief.
//
// Crediting the difference immediately would be exactly the "released by a
// deploy" behaviour SetRules exists to avoid, only triggered on purpose.
func TestSetRules_RaisingSpeedsRefillRatherThanRefilling(t *testing.T) {
	c := newClock()
	l := New(Rules{MsgPerSender: Rule{Rate: 2, Per: time.Hour}}, c.now)

	take(l, MsgPerSender, "a@ngm.dev", 2)
	l.SetRules(Rules{MsgPerSender: Rule{Rate: 50, Per: time.Hour}})

	// Still empty a second later: 50/hour is one every 72 seconds.
	c.advance(time.Second)
	if l.Allow(MsgPerSender, "a@ngm.dev") {
		t.Error("raising the limit instantly refilled a drained bucket")
	}

	// But relief arrives in 72 seconds rather than the 30 minutes the old rate
	// would have needed.
	c.advance(72 * time.Second)
	if !l.Allow(MsgPerSender, "a@ngm.dev") {
		t.Error("the raised rate did not speed the refill")
	}
}

// TestSetRules_LoweringBurstTakesEffect: clamping happens after the refill, so a
// peer cannot keep spending an allowance the operator has just removed.
func TestSetRules_LoweringBurstTakesEffect(t *testing.T) {
	c := newClock()
	l := New(Rules{MsgPerSender: Rule{Rate: 100, Per: time.Minute}}, c.now)

	l.Allow(MsgPerSender, "a@ngm.dev") // 99 left
	l.SetRules(Rules{MsgPerSender: Rule{Rate: 100, Per: time.Minute, Burst: 5}})

	if got := take(l, MsgPerSender, "a@ngm.dev", 50); got != 5 {
		t.Errorf("after lowering burst to 5, allowed %d", got)
	}
}

func TestRules_Any(t *testing.T) {
	if (Rules{}).Any() {
		t.Error("empty rules report as limiting something")
	}
	if (Rules{MaxKeys: 10}).Any() {
		t.Error("MaxKeys alone is not a limit")
	}
	for name, r := range map[string]Rules{
		"conn":   {ConnPerIP: Rule{Rate: 1, Per: time.Second}},
		"sender": {MsgPerSender: Rule{Rate: 1, Per: time.Second}},
		"user":   {MsgPerUser: Rule{Rate: 1, Per: time.Second}},
		"rcpt":   {RcptPerDomain: Rule{Rate: 1, Per: time.Second}},
		"auth":   {AuthFailPerIP: Rule{Rate: 1, Per: time.Second}},
	} {
		if !r.Any() {
			t.Errorf("%s rule not reported by Any()", name)
		}
	}
}

// TestConcurrent_NeverExceedsTheLimit is why `go test -race` matters more here
// than anywhere else in the module: this is touched from every accept goroutine
// and every session goroutine at once.
//
// It asserts the arithmetic as well as the absence of a race — a limiter that
// lost updates under contention would let more through than it should, which is
// the failure a race detector alone would not catch.
func TestConcurrent_NeverExceedsTheLimit(t *testing.T) {
	c := newClock() // frozen: no refill can confuse the count
	l := New(Rules{ConnPerIP: Rule{Rate: 100, Per: time.Minute}}, c.now)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if l.Allow(ConnPerIP, "10.0.0.1") {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 100 {
		t.Errorf("1600 concurrent events on a limit of 100 allowed %d", got)
	}
}

// TestConcurrent_ManyKeys exercises the eviction path under contention, where a
// sweep runs while other goroutines are creating buckets.
func TestConcurrent_ManyKeys(t *testing.T) {
	c := newClock()
	l := New(Rules{ConnPerIP: Rule{Rate: 2, Per: time.Second}, MaxKeys: 64}, c.now)

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 200 {
				l.Allow(ConnPerIP, fmt.Sprintf("10.%d.0.%d", g, i%256))
			}
		}(g)
	}
	wg.Wait()

	if l.Len() > 64 {
		t.Errorf("the map holds %d keys under contention, over MaxKeys=64", l.Len())
	}
}

func TestNew_NilClockUsesWallTime(t *testing.T) {
	l := New(Rules{ConnPerIP: Rule{Rate: 1, Per: time.Hour}}, nil)
	if !l.Allow(ConnPerIP, "10.0.0.1") {
		t.Fatal("first event refused")
	}
	if l.Allow(ConnPerIP, "10.0.0.1") {
		t.Error("second event allowed against a limit of one per hour")
	}
}

func TestMaxKeys_ZeroMeansTheDefaultNotUnbounded(t *testing.T) {
	if got := (Rules{}).maxKeys(); got != DefaultMaxKeys {
		t.Errorf("maxKeys() = %d, want %d — unbounded is the state this bounds", got, DefaultMaxKeys)
	}
	if got := (Rules{MaxKeys: 7}).maxKeys(); got != 7 {
		t.Errorf("maxKeys() = %d, want 7", got)
	}
}
