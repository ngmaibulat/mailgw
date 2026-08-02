// Package ratelimit bounds how OFTEN something may happen, per key.
//
// It is the other half of the pair M11 started. `max.connections` bounds how
// many things happen at once; this bounds how often they happen at all. A peer
// opening one connection at a time and pushing a million messages through it
// never trips a concurrency cap.
//
// Like internal/msgauth, internal/dsn and internal/attach, it knows nothing
// about the spool, the session or the configuration: the clock is injected, the
// rules arrive as a struct, and every key is a string the caller chose. That is
// what makes the window arithmetic testable without a running gateway and
// without a real clock.
//
// # Token buckets, not sliding windows
//
// M15's plan said "in-memory sliding windows". A bucket is used instead, and the
// reason is the map bound. A sliding window has to keep the timestamps of the
// events still inside it, so its memory per key grows with the configured rate —
// a 10000/min limit keeps up to 10000 timestamps for one peer, and "bound the
// maps" then means bounding a quantity nobody can predict. A bucket is one
// float and one timestamp whatever the rate.
//
// It also makes eviction provably safe rather than a heuristic: a bucket at full
// capacity has no memory of the past at all, so it is byte-for-byte what a
// freshly created one would be, and dropping it cannot let anybody through who
// would otherwise have been refused. sweep() relies on exactly that.
//
// To an operator writing "100 per minute" the two behave the same.
package ratelimit

import (
	"sync"
	"time"
)

// Rule is one configured limit.
//
// The zero value is OFF, which is what every limit ships as: this gateway's
// defaults do not silently start refusing mail.
type Rule struct {
	// Rate is how many events are permitted per Per. Zero disables the rule.
	Rate int
	// Per is the window the rate is expressed over.
	Per time.Duration
	// Burst is how many events may arrive at once before the sustained rate
	// starts to bite. Zero means Rate — a full window's worth, which is what an
	// operator writing "100 per minute" almost always means.
	Burst int
}

// Enabled reports whether this rule limits anything.
func (r Rule) Enabled() bool { return r.Rate > 0 && r.Per > 0 }

// burst resolves the effective bucket capacity.
func (r Rule) burst() float64 {
	if r.Burst > 0 {
		return float64(r.Burst)
	}
	return float64(r.Rate)
}

// refillPerSecond is how fast the bucket recovers.
func (r Rule) refillPerSecond() float64 {
	return float64(r.Rate) / r.Per.Seconds()
}

// Dimension names what is being limited. It is part of every key, so two
// dimensions cannot collide even when they are keyed on the same string — a
// sender address and an authenticated username often are.
type Dimension string

const (
	// ConnPerIP is checked in the listener chain, once per accepted connection.
	ConnPerIP Dimension = "connect_per_ip"
	// MsgPerSender and MsgPerUser are checked at MAIL FROM, once per message.
	MsgPerSender Dimension = "messages_per_sender"
	MsgPerUser   Dimension = "messages_per_user"
	// RcptPerDomain is checked at RCPT TO, once per RECIPIENT — so a message to
	// fifty addresses at one domain costs fifty, which is the unit that matters
	// when the question is "how much mail is going there".
	RcptPerDomain Dimension = "rcpts_per_domain"
	// AuthFailPerIP is checked before the password comparison, so a refusal
	// costs no bcrypt.
	AuthFailPerIP Dimension = "auth_failures_per_ip"
)

// Rules is the full set, one per dimension.
type Rules struct {
	ConnPerIP     Rule
	MsgPerSender  Rule
	MsgPerUser    Rule
	RcptPerDomain Rule
	AuthFailPerIP Rule

	// MaxKeys bounds how many buckets are tracked across every dimension.
	//
	// A limiter keyed on a remote address is itself a memory-exhaustion vector
	// if nothing evicts — M11.4's MX cache is the cautionary example already in
	// this tree. Zero means DefaultMaxKeys rather than "unbounded", because
	// unbounded is the state this field exists to prevent.
	MaxKeys int
}

// DefaultMaxKeys is the bucket ceiling when none is configured.
//
// High enough that no real deployment meets it — a busy relay sees thousands of
// distinct senders an hour, not hundreds of thousands — and a bucket is about
// 100 bytes, so the ceiling costs roughly 10 MiB in the worst case. It is a
// memory bound, not a tuning knob.
const DefaultMaxKeys = 100_000

func (r Rules) maxKeys() int {
	if r.MaxKeys > 0 {
		return r.MaxKeys
	}
	return DefaultMaxKeys
}

// Any reports whether any dimension is limited, which is what decides whether a
// gateway builds a limiter at all.
func (r Rules) Any() bool {
	for _, x := range []Rule{r.ConnPerIP, r.MsgPerSender, r.MsgPerUser, r.RcptPerDomain, r.AuthFailPerIP} {
		if x.Enabled() {
			return true
		}
	}
	return false
}

// rule returns the rule for a dimension.
func (r Rules) rule(d Dimension) Rule {
	switch d {
	case ConnPerIP:
		return r.ConnPerIP
	case MsgPerSender:
		return r.MsgPerSender
	case MsgPerUser:
		return r.MsgPerUser
	case RcptPerDomain:
		return r.RcptPerDomain
	case AuthFailPerIP:
		return r.AuthFailPerIP
	}
	return Rule{}
}

// bucket is one key's remaining allowance.
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter answers "may this happen now?" for a set of keyed dimensions.
//
// Safe for concurrent use: it is touched from every accept goroutine and every
// session goroutine at once, which is why `go test -race` matters more here than
// anywhere else in the module.
type Limiter struct {
	// now is injected so the window arithmetic is testable. Reading the wall
	// clock directly inside the limiter is what makes this kind of test flaky,
	// so the dependency is taken from the start rather than retrofitted.
	now func() time.Time

	mu      sync.Mutex
	rules   Rules
	buckets map[key]*bucket
}

type key struct {
	dim Dimension
	id  string
}

// New builds a limiter. A nil clock means time.Now.
func New(rules Rules, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, rules: rules, buckets: map[key]*bucket{}}
}

// SetRules retunes the limiter in place.
//
// The buckets deliberately SURVIVE. Rebuilding the limiter on every apply would
// be simpler, but it would hand every peer a fresh allowance whenever an
// operator deployed an unrelated configuration change — so an attacker under
// pressure would be released by a routine deploy. Reinterpreting existing
// buckets under the new rate is both cheaper and the behaviour an operator
// adjusting a limit during an incident expects.
//
// One consequence is worth knowing before an operator meets it in production:
// RAISING a limit does not instantly refill a bucket that is already empty. It
// raises the ceiling and speeds the refill — going from 2/hour to 50/hour turns
// a 30-minute wait for the next event into a 72-second one — but the peer that
// was being throttled a moment ago is still, briefly, being throttled. Crediting
// the difference immediately would be the "release on deploy" behaviour this
// whole design exists to avoid, only triggered deliberately instead of by
// accident.
func (l *Limiter) SetRules(rules Rules) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rules = rules
	// A limit that has just been switched off should stop holding memory, and a
	// lowered MaxKeys should take effect now rather than at the next refusal.
	l.evictLocked()
}

// Rules returns the rules in force, for `check` and for tests.
func (l *Limiter) Rules() Rules {
	if l == nil {
		return Rules{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rules
}

// Allow reports whether one event on (dim, id) is permitted, and consumes an
// allowance when it is.
//
// A nil limiter, a disabled rule and an empty id all return true. The last is
// deliberate: an id the caller could not determine — a null sender, an
// unresolvable domain — must not be lumped into one shared bucket, because that
// bucket would then be the busiest key on the gateway and would refuse mail that
// has nothing to do with any one peer.
func (l *Limiter) Allow(dim Dimension, id string) bool { return l.take(dim, id, true) }

// Blocked reports whether the next event on (dim, id) would be refused, WITHOUT
// consuming an allowance.
//
// It exists for the failed-AUTH limiter, where the two halves have to be
// separated: the budget is spent by failures alone, so a client authenticating
// correctly a hundred times must never be affected however tight the limit is
// set. Allow would spend on every attempt and make the limit "AUTH commands per
// IP", which is a different and much less useful thing.
//
// Pair it with Spend on the path that should actually cost something.
func (l *Limiter) Blocked(dim Dimension, id string) bool {
	return !l.take(dim, id, false)
}

// Spend consumes one allowance on (dim, id) whatever the answer.
//
// A refusal is not reported because the caller has already acted: it is
// recording that something happened, not asking permission.
func (l *Limiter) Spend(dim Dimension, id string) { l.take(dim, id, true) }

// take is the one place the bucket arithmetic lives.
//
// spend=false makes it a query: the refill still happens (a bucket is only
// meaningful once it is current) but no token is removed.
func (l *Limiter) take(dim Dimension, id string, spend bool) bool {
	if l == nil || id == "" {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	rule := l.rules.rule(dim)
	if !rule.Enabled() {
		return true
	}

	now := l.now()
	capacity := rule.burst()
	k := key{dim: dim, id: id}

	b, ok := l.buckets[k]
	if !ok {
		if !spend {
			// A key with no bucket has its full allowance by definition. Do not
			// allocate one to answer a question — otherwise Blocked would be a
			// way to fill the map without ever sending anything.
			return true
		}
		if len(l.buckets) >= l.rules.maxKeys() {
			l.evictLocked()
		}
		if len(l.buckets) >= l.rules.maxKeys() {
			// Still full: every tracked bucket is genuinely in use. Admit rather
			// than refuse. The alternative — refusing what cannot be tracked —
			// turns a memory ceiling into a mail outage, and hands an attacker a
			// way to deny service to everybody by filling the map.
			return true
		}
		b = &bucket{tokens: capacity, last: now}
		l.buckets[k] = b
	}

	// Refill for the time that has passed, then spend.
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * rule.refillPerSecond()
		b.last = now
	}
	// Clamped AFTER refilling, so lowering burst takes effect on the next event
	// rather than leaving a peer holding an allowance the operator just removed.
	if b.tokens > capacity {
		b.tokens = capacity
	}

	if b.tokens < 1 {
		return false
	}
	if spend {
		b.tokens--
	}
	return true
}

// Len reports how many buckets are tracked. For tests and for `check`.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// evictLocked drops every bucket that has nothing left to say.
//
// A bucket is removable exactly when it has refilled to capacity: at that point
// it is identical to the one Allow would create for the same key, so forgetting
// it cannot admit an event that would otherwise have been refused. That is the
// whole reason this package uses buckets — the same argument cannot be made
// about a sliding window's timestamps, which have to be kept until they expire.
//
// Buckets for a dimension whose rule has been disabled go too, whatever their
// state: nothing will consult them again.
//
// Caller holds l.mu.
func (l *Limiter) evictLocked() {
	now := l.now()
	for k, b := range l.buckets {
		rule := l.rules.rule(k.dim)
		if !rule.Enabled() {
			delete(l.buckets, k)
			continue
		}
		tokens := b.tokens
		if elapsed := now.Sub(b.last); elapsed > 0 {
			tokens += elapsed.Seconds() * rule.refillPerSecond()
		}
		if tokens >= rule.burst() {
			delete(l.buckets, k)
		}
	}
}
