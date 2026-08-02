package deliver

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// blackHole answers TCP and the greeting, then never says another word — a
// relay that has stopped responding without closing, which is the shape every
// unbounded wait in this package hangs on.
func blackHole(t *testing.T) (relay relays.Relay, dial func() (*smtp.Client, net.Conn)) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var mu sync.Mutex
	var accepted []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
			_, _ = c.Write([]byte("220 blackhole.test ESMTP\r\n"))
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		for _, c := range accepted {
			_ = c.Close()
		}
		mu.Unlock()
	})

	host, port, _ := net.SplitHostPort(l.Addr().String())
	p, _ := net.LookupPort("tcp", port)
	relay = relays.Relay{Name: "blackhole", Exchange: host, Port: relays.Port(p), TLS: relays.TLSNone}

	dial = func() (*smtp.Client, net.Conn) {
		t.Helper()
		conn, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return smtp.NewClient(conn), conn
	}
	return relay, dial
}

// The window M16 closed. context.AfterFunc's stop() returns false once the
// context has ended and closeConn is already running on its own goroutine — it
// cannot un-start it — so release() has to take the connection away as well.
// Without that, a cancellation landing between release and Pool.Put closes a
// socket that by then belongs to another delivery.
func TestConnGuard_ReleaseTakesTheConnectionAwayFromTheCloser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g := newConnGuard(ctx)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	g.use(server)
	g.release()

	// The racing goroutine, run explicitly: the attempt is over, so this must be
	// a no-op however late it arrives.
	cancel()
	g.closeConn()

	done := make(chan error, 1)
	go func() {
		_, err := server.Write([]byte("still open\r\n"))
		done <- err
	}()
	go func() {
		// net.Pipe is unbuffered and synchronous, so the write above only
		// completes once every byte has been read.
		buf := make([]byte, 64)
		for range 4 {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the guard closed a connection it had released: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write blocked; the connection state after release is not what it should be")
	}
}

// quietQuit sends QUIT and waits for the reply. Held across the pool mutex, one
// over-cap Put to a relay that has stopped answering stops every other Get, Put
// and Reap in the process for as long as that takes.
func TestPool_PutOverTheCapDoesNotBlockTheDeliveryPath(t *testing.T) {
	relay, dial := blackHole(t)
	opts := Options{LocalName: "devbook.local"}

	p := &Pool{MaxPerRelay: 1, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Close(ctx)
	})

	first, firstConn := dial()
	p.Put(relay, opts, first, firstConn, 0)

	// Over the cap: this Put has to dispose of its connection, and disposing of
	// it means waiting on a relay that will never answer.
	over, overConn := dial()
	putting := make(chan struct{})
	go func() {
		p.Put(relay, opts, over, overConn, 0)
		close(putting)
	}()

	// A checkout for a DIFFERENT relay, so it takes the mutex, finds nothing and
	// returns — it never probes, which keeps this about the lock rather than
	// about the black hole.
	other := relay
	other.Name, other.Exchange = "other", "other.invalid"

	got := make(chan bool, 1)
	go func() {
		c, _, _ := p.Get(other, opts, nil)
		got <- c != nil
	}()

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("Get blocked behind an over-cap Put's QUIT: the pool mutex is " +
			"being held across a network round trip")
	}

	// Let the parked Put finish so the test does not leak it.
	_ = overConn.Close()
	<-putting
}

// The checkout probe is a network round trip too, and until M16 it ran before
// the guard was armed and before the configured timeouts were applied — so a
// pooled connection to a relay that stopped answering could not be interrupted
// at all.
func TestPool_CancellationInterruptsTheCheckoutProbe(t *testing.T) {
	relay, dial := blackHole(t)
	opts := Options{LocalName: "devbook.local"}

	p := &Pool{MaxPerRelay: 2, MaxMessages: 10, IdleTimeout: time.Minute}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Close(ctx)
	})

	c, conn := dial()
	p.Put(relay, opts, c, conn, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	opts.Pool = p
	done := make(chan *Result, 1)
	go func() {
		done <- Deliver(ctx, relay, msg("me@ngm.dev", "you@ngm.dev"), opts)
	}()

	select {
	case res := <-done:
		if res.Err == nil {
			t.Fatal("delivery to a black hole reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the attempt could not be cancelled: the RSET probe on a pooled " +
			"connection is outside the guard")
	}
}
