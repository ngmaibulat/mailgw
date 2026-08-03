package deliver

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// countingBackend records how many TCP connections the relay accepted, which is
// the only thing that actually proves reuse.
type countingBackend struct {
	fakeBackend
	sessions atomic.Int32
}

func (b *countingBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	b.sessions.Add(1)
	return &fakeSession{b: &b.fakeBackend}, nil
}

func startCountingRelay(t *testing.T) (relays.Relay, *countingBackend) {
	t.Helper()
	b := &countingBackend{}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := smtp.NewServer(b)
	srv.Domain = "relay.test"
	srv.MaxMessageBytes = 1 << 20
	srv.AllowInsecureAuth = true
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	host, port, _ := net.SplitHostPort(l.Addr().String())
	p, _ := net.LookupPort("tcp", port)
	return relays.Relay{Name: "r1", Exchange: host, Port: relays.Port(p), TLS: relays.TLSNone}, b
}

// The default. Every deployment until now dials per message, and that must not
// change because the code to do otherwise exists.
func TestPool_DisabledDialsEveryTime(t *testing.T) {
	relay, b := startCountingRelay(t)

	for range 3 {
		res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
			Options{LocalName: "devbook.local"})
		if res.Err != nil {
			t.Fatalf("Deliver: %v", res.Err)
		}
		if res.Reused {
			t.Error("a connection was reported reused with no pool configured")
		}
	}
	if n := b.sessions.Load(); n != 3 {
		t.Errorf("relay saw %d connections, want 3", n)
	}
}

func TestPool_ReusesOneConnectionAcrossMessages(t *testing.T) {
	relay, b := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })

	var reused int
	for range 3 {
		res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
			Options{LocalName: "devbook.local", Pool: p})
		if res.Err != nil {
			t.Fatalf("Deliver: %v", res.Err)
		}
		if res.Reused {
			reused++
		}
	}

	if n := b.sessions.Load(); n != 1 {
		t.Errorf("relay saw %d connections, want 1 — the connection was not reused", n)
	}
	if reused != 2 {
		t.Errorf("%d attempts reported reuse, want 2", reused)
	}
	if len(b.messages()) != 3 {
		t.Errorf("relay received %d messages, want 3", len(b.messages()))
	}
}

// Relays commonly cap messages per connection and answer the one after with a
// 421. Retiring first turns that from a delivery failure into a redial.
func TestPool_RetiresAConnectionAtTheMessageLimit(t *testing.T) {
	relay, b := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 2, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })

	for range 4 {
		res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
			Options{LocalName: "devbook.local", Pool: p})
		if res.Err != nil {
			t.Fatalf("Deliver: %v", res.Err)
		}
	}

	// Two messages per connection, four messages: two connections.
	if n := b.sessions.Load(); n != 2 {
		t.Errorf("relay saw %d connections, want 2", n)
	}
}

// A relay closes idle connections on its own schedule. The RSET probe is what
// stops that from surfacing as a delivery failure — the stale connection is
// dropped and a fresh one dialled, and the caller never sees it.
func TestPool_SurvivesARelayClosingTheConnection(t *testing.T) {
	relay, b := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p})
	if res.Err != nil {
		t.Fatalf("first Deliver: %v", res.Err)
	}

	// Close the pooled connection from underneath, exactly as a relay reaping
	// idle sessions would.
	c, conn, _ := p.Get(relay, Options{LocalName: "devbook.local"}, nil)
	if c == nil {
		t.Fatal("nothing was pooled after a successful delivery")
	}
	if conn == nil {
		t.Fatal("a pooled connection came back without its socket")
	}
	_ = c.Close()
	p.Put(relay, Options{LocalName: "devbook.local"}, c, conn, 1)

	res = Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p})
	if res.Err != nil {
		t.Fatalf("a dead pooled connection surfaced as a delivery failure: %v", res.Err)
	}
	if len(b.messages()) != 2 {
		t.Errorf("relay received %d messages, want 2", len(b.messages()))
	}
}

// An idle connection past its timeout is dropped without being probed: one we
// already know is stale is cheaper to discard than to discover.
func TestPool_DropsAnExpiredIdleConnection(t *testing.T) {
	relay, b := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: time.Nanosecond}
	t.Cleanup(func() { p.Close(context.Background()) })

	for range 2 {
		res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
			Options{LocalName: "devbook.local", Pool: p})
		if res.Err != nil {
			t.Fatalf("Deliver: %v", res.Err)
		}
	}
	if n := b.sessions.Load(); n != 2 {
		t.Errorf("relay saw %d connections, want 2 — an expired connection was reused", n)
	}
}

// The most damaging thing a pool can do is carry one message's failure into the
// next. A session that ended badly must never go back in.
func TestPool_DiscardsAConnectionAfterAFailure(t *testing.T) {
	b := &fakeBackend{dataErr: &smtp.SMTPError{Code: 552, Message: "too big"}}
	relay := startRelay(t, b, "r1")
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p})
	if res.Err != nil {
		t.Fatalf("unexpected connection error: %v", res.Err)
	}

	if c, _, _ := p.Get(relay, Options{LocalName: "devbook.local"}, nil); c != nil {
		t.Error("a connection whose DATA was refused was returned to the pool")
	}
}

// Two relays that share an address but authenticate as different users are not
// interchangeable. Handing one's session to the other would send mail as the
// wrong identity, and the relay would accept it — the session is already
// authenticated.
func TestPool_DoesNotShareConnectionsBetweenIdentities(t *testing.T) {
	relay, _ := startCountingRelay(t)

	alice := relay
	alice.AuthUser = "alice"
	bob := relay
	bob.AuthUser = "bob"

	if poolKey(alice, Options{}) == poolKey(bob, Options{}) {
		t.Error("two relays authenticating as different users share a pool key")
	}

	plain := relay
	secure := relay
	secure.TLS = relays.TLSRequired
	if poolKey(plain, Options{}) == poolKey(secure, Options{}) {
		t.Error("two relays with different TLS policies share a pool key")
	}
}

func TestPool_BoundsIdleConnectionsPerRelay(t *testing.T) {
	relay, _ := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 1, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })

	opts := Options{LocalName: "devbook.local", Pool: p}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), opts)
		}()
	}
	wg.Wait()

	p.mu.Lock()
	held := 0
	for _, cs := range p.idle {
		held += len(cs)
	}
	p.mu.Unlock()

	if held > 1 {
		t.Errorf("pool holds %d idle connections, want at most 1", held)
	}
}

func TestPool_CloseDropsEverything(t *testing.T) {
	relay, _ := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: time.Minute}

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p})
	if res.Err != nil {
		t.Fatalf("Deliver: %v", res.Err)
	}

	p.Close(context.Background())
	if c, _, _ := p.Get(relay, Options{LocalName: "devbook.local"}, nil); c != nil {
		t.Error("Close left a connection in the pool")
	}
}

// stallingBackend accepts the first message normally and then blocks forever
// inside DATA, so a second delivery over the SAME pooled connection has nothing
// but cancellation to end it.
type stallingBackend struct {
	fakeBackend
	stalled chan struct{}
	release chan struct{}
	n       atomic.Int32
	once    sync.Once
}

func (b *stallingBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &stallingSession{b: b}, nil
}

type stallingSession struct{ b *stallingBackend }

func (s *stallingSession) Mail(string, *smtp.MailOptions) error { return nil }
func (s *stallingSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (s *stallingSession) Reset()                               {}
func (s *stallingSession) Logout() error                        { return nil }

func (s *stallingSession) Data(r io.Reader) error {
	if s.b.n.Add(1) == 1 {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	s.b.once.Do(func() { close(s.b.stalled) })
	<-s.b.release
	return nil
}

func startStallingRelay(t *testing.T) (relays.Relay, *stallingBackend) {
	t.Helper()
	b := &stallingBackend{stalled: make(chan struct{}), release: make(chan struct{})}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := smtp.NewServer(b)
	srv.Domain = "relay.test"
	srv.MaxMessageBytes = 1 << 20
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		close(b.release)
		_ = srv.Close()
	})

	host, port, _ := net.SplitHostPort(l.Addr().String())
	p, _ := net.LookupPort("tcp", port)
	return relays.Relay{Name: "stall", Exchange: host, Port: relays.Port(p), TLS: relays.TLSNone}, b
}

// The M11.5 defect. The cancellation guard was armed only inside connect(), so
// a connection borrowed from the pool had no socket published to it: a
// cancelled context could not interrupt an in-flight DATA and the attempt ran to
// SubmissionTimeout — ten minutes on the defaults, long past
// server.shutdown_timeout and past everything the ordered teardown bounds.
func TestPool_ContextCancellationInterruptsAReusedConnection(t *testing.T) {
	relay, b := startStallingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10}
	t.Cleanup(func() { p.Close(context.Background()) })

	opts := Options{
		LocalName: "devbook.local", Pool: p,
		// Deliberately long: only the cancellation can end the second attempt.
		ConnectTimeout: 30 * time.Second,
		DataTimeout:    30 * time.Second,
	}

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), opts)
	if res.Err != nil {
		t.Fatalf("first Deliver: %v", res.Err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-b.stalled // the relay is now inside DATA and will not answer
		cancel()
	}()

	start := time.Now()
	res = Deliver(ctx, relay, msg("me@ngm.dev", "you@ngm.dev"), opts)
	elapsed := time.Since(start)

	if !res.Reused {
		t.Fatal("the second delivery dialled instead of reusing; this test proves nothing")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Deliver took %v — cancellation did not reach the pooled connection", elapsed)
	}
	if res.Err == nil && len(res.Accepted()) > 0 {
		t.Error("a cancelled attempt reported recipients as delivered")
	}
}

// A pooled connection used to keep whatever CommandTimeout/SubmissionTimeout its
// first dial set, because applyTimeouts only ever ran inside connect().
func TestPool_ReusedConnectionsGetTheCurrentTimeouts(t *testing.T) {
	relay, _ := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10}
	t.Cleanup(func() { p.Close(context.Background()) })

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p,
			ConnectTimeout: 30 * time.Second, DataTimeout: 30 * time.Second})
	if res.Err != nil {
		t.Fatalf("first Deliver: %v", res.Err)
	}

	c, conn, _ := p.Get(relay, Options{LocalName: "devbook.local"}, nil)
	if c == nil || conn == nil {
		t.Fatal("nothing usable was pooled")
	}
	p.Put(relay, Options{LocalName: "devbook.local"}, c, conn, 1)

	res = Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p,
			ConnectTimeout: 7 * time.Second, DataTimeout: 11 * time.Second})
	if res.Err != nil {
		t.Fatalf("second Deliver: %v", res.Err)
	}
	if !res.Reused {
		t.Fatal("the second delivery did not reuse the connection")
	}
	if c.CommandTimeout != 7*time.Second || c.SubmissionTimeout != 11*time.Second {
		t.Errorf("pooled client kept command=%v submission=%v, want 7s/11s",
			c.CommandTimeout, c.SubmissionTimeout)
	}
}

// The peer IP was left blank on the reused path, on the grounds that claiming an
// address the attempt never resolved would be a guess. With the socket carried
// through the pool it is not a guess.
func TestPool_ReusedConnectionsRecordThePeerAddress(t *testing.T) {
	relay, _ := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10}
	t.Cleanup(func() { p.Close(context.Background()) })

	opts := Options{LocalName: "devbook.local", Pool: p}
	if res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), opts); res.Err != nil {
		t.Fatalf("first Deliver: %v", res.Err)
	}

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), opts)
	if res.Err != nil {
		t.Fatalf("second Deliver: %v", res.Err)
	}
	if !res.Reused {
		t.Fatal("the second delivery did not reuse the connection")
	}
	if res.IP == "" {
		t.Error("a reused delivery recorded no peer address")
	}
}

// IdleTimeout was enforced lazily inside Get only, so a relay that stopped
// receiving traffic kept its sockets until process shutdown.
func TestPool_ReapDropsIdleConnections(t *testing.T) {
	relay, _ := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: 20 * time.Millisecond}
	t.Cleanup(func() { p.Close(context.Background()) })

	if res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p}); res.Err != nil {
		t.Fatalf("Deliver: %v", res.Err)
	}
	if idleCount(p) != 1 {
		t.Fatalf("%d idle connections after a delivery, want 1", idleCount(p))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Reap(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for idleCount(p) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the reaper never dropped an idle connection")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The key goes too, or the map keeps a zero-length slice for every relay the
	// gateway has ever spoken to — and use_mx mints one key per exchanger.
	p.mu.Lock()
	keys := len(p.idle)
	p.mu.Unlock()
	if keys != 0 {
		t.Errorf("%d empty keys left in the pool map, want 0", keys)
	}
}

// A reaper must not take connections that are still fresh.
func TestPool_ReapLeavesFreshConnectionsAlone(t *testing.T) {
	relay, _ := startCountingRelay(t)
	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: time.Hour}
	t.Cleanup(func() { p.Close(context.Background()) })

	if res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local", Pool: p}); res.Err != nil {
		t.Fatalf("Deliver: %v", res.Err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Reap(ctx, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)

	if idleCount(p) != 1 {
		t.Errorf("%d idle connections, want 1 — the reaper took a fresh one", idleCount(p))
	}
}

// dialRelay opens a greeted session against a real fake relay and hands back the
// socket underneath it.
//
// The global-cap tests drive Pool.Put directly rather than through Deliver,
// because what has to be asserted is what happens to the connection the pool
// turns away — and Deliver never lets go of it. A real relay rather than
// guard_test's blackHole, because quietQuit sends QUIT and waits for the 221.
func dialRelay(t *testing.T, relay relays.Relay) (*smtp.Client, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", relay.Addr())
	if err != nil {
		t.Fatalf("dial %s: %v", relay.Addr(), err)
	}
	c := smtp.NewClient(conn)
	if err := c.Hello("devbook.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	return c, conn
}

// closed reports whether the socket has been shut, which is how a refused
// connection is told apart from a leaked one.
func closed(t *testing.T, conn net.Conn) bool {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return true // already closed
	}
	_, err := conn.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// MaxPerRelay bounds one key; nothing bounded the number of KEYS, and poolKey is
// built from relay.Addr(), so with use_mx the key set follows DNS. Three relays
// under a global cap of two is that ceiling in miniature.
func TestPool_BoundsPooledConnectionsGlobally(t *testing.T) {
	p := &Pool{MaxPerRelay: 5, MaxTotal: 2, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })
	opts := Options{LocalName: "devbook.local"}

	var relayList []relays.Relay
	for range 3 {
		r, _ := startCountingRelay(t)
		relayList = append(relayList, r)
	}

	for i, r := range relayList[:2] {
		c, conn := dialRelay(t, r)
		if full := p.Put(r, opts, c, conn, 1); full {
			t.Fatalf("relay %d was refused below the global cap", i)
		}
	}
	if got := idleCount(p); got != 2 {
		t.Fatalf("%d idle connections after two puts, want 2", got)
	}

	third := relayList[2]
	c, conn := dialRelay(t, third)
	if full := p.Put(third, opts, c, conn, 1); !full {
		t.Error("Put did not report the global cap refusing the connection")
	}
	if got := idleCount(p); got != 2 {
		t.Errorf("%d idle connections, want 2 — the cap admitted a third", got)
	}
	// The policy is refuse-and-close, not evict: the connection the cap turned
	// away must be gone, or the cap would bound the map while leaking sockets.
	if !closed(t, conn) {
		t.Error("the refused connection was left open")
	}
	// And nothing was evicted to make room for it.
	if c, _, _ := p.Get(relayList[0], opts, nil); c == nil {
		t.Error("the cap evicted an incumbent instead of refusing the newcomer")
	}
}

// The argument the whole policy rests on: Get takes a connection OUT of the pool,
// which frees its slot, and Put returns it. A key already in the pool therefore
// recycles its own slot and never meets the cap — so refusing newcomers does not
// disable pooling for the busy relay it was turned on for.
func TestPool_ABusyKeyRecyclesItsOwnSlotAtTheCap(t *testing.T) {
	p := &Pool{MaxPerRelay: 5, MaxTotal: 1, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })
	opts := Options{LocalName: "devbook.local"}

	relay, _ := startCountingRelay(t)
	c, conn := dialRelay(t, relay)
	if full := p.Put(relay, opts, c, conn, 1); full {
		t.Fatalf("the first connection was refused at a cap of 1")
	}

	got, gotConn, msgs := p.Get(relay, opts, nil)
	if got == nil {
		t.Fatal("the pooled connection was not handed back")
	}
	if full := p.Put(relay, opts, got, gotConn, msgs+1); full {
		t.Error("a key returning its own connection was refused by the global cap")
	}
	if n := idleCount(p); n != 1 {
		t.Errorf("%d idle connections, want 1", n)
	}
}

// Being over per_group_connections is one relay behaving exactly as configured.
// It is not the global ceiling and must not be reported as one, or the counter
// would fire on healthy pooling.
func TestPool_PerRelayRefusalIsNotReportedAsGlobal(t *testing.T) {
	p := &Pool{MaxPerRelay: 1, MaxTotal: 10, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() { p.Close(context.Background()) })
	opts := Options{LocalName: "devbook.local"}

	relay, _ := startCountingRelay(t)
	for i := range 2 {
		c, conn := dialRelay(t, relay)
		if full := p.Put(relay, opts, c, conn, 1); full {
			t.Errorf("put %d reported the global cap for a per-relay refusal", i)
		}
	}
	if n := idleCount(p); n != 1 {
		t.Errorf("%d idle connections, want 1", n)
	}
}

// The reason a global cap is needed at all: without use_mx the key set is the
// relay table, and with it the key set is whatever DNS names.
func TestPool_TheKeySetFollowsDNS(t *testing.T) {
	r := &Resolver{Lookup: staticMX(
		&net.MX{Host: "mx1.partner.com.", Pref: 10},
		&net.MX{Host: "mx2.partner.com.", Pref: 20},
		&net.MX{Host: "mx3.partner.com.", Pref: 30},
	)}
	relay := relays.Relay{Name: "Outbound", Exchange: "partner.com", Port: 25, UseMX: true}

	expanded, err := Expand(context.Background(), r, relay)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(expanded) != 3 {
		t.Fatalf("Expand produced %d relays, want 3", len(expanded))
	}

	seen := map[string]bool{}
	for _, m := range expanded {
		seen[poolKey(m, Options{})] = true
	}
	if len(seen) != 3 {
		t.Errorf("three exchangers produced %d pool keys, want 3 — "+
			"if they collapsed, the ceiling this cap exists for would not exist", len(seen))
	}
	if k := poolKey(relay, Options{}); seen[k] {
		t.Error("a synthetic MX relay shares a pool key with the configured relay")
	}
}

func idleCount(p *Pool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, cs := range p.idle {
		n += len(cs)
	}
	return n
}
