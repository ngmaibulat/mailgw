package deliver

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// Pool keeps authenticated relay connections open between envelopes.
//
// Off by default (outbound.reuse_connections), and that is a considered choice
// rather than caution for its own sake. Reuse changes what every relay in the
// field sees from this gateway: many smarthosts cap messages per connection,
// many rate-limit per connection rather than per message, and TLS session reuse
// alters what a receiving system records. None of that is visible from here, and
// nothing in the present volume profile shows a need. The code is tested and one
// configuration key away for a deployment that measures a reason.
//
// A pooled connection is only ever an optimisation. Every path that cannot prove
// a connection is still good throws it away and dials — never returns an error
// the caller would report as a delivery failure.
type Pool struct {
	// MaxPerRelay bounds idle connections held for one relay.
	MaxPerRelay int
	// MaxMessages is how many messages one connection may carry before it is
	// retired. Relays commonly enforce their own limit and answer the message
	// after it with a 421; retiring first keeps that from looking like a
	// delivery failure.
	MaxMessages int
	// IdleTimeout is how long an unused connection is kept. A relay will close
	// an idle connection on its own schedule, and a connection we know is stale
	// is cheaper to drop than to discover.
	IdleTimeout time.Duration

	mu   sync.Mutex
	idle map[string][]*pooled
}

// pooled is one idle connection.
//
// conn is the RAW net.Conn under the client, and carrying it is not
// bookkeeping: go-smtp's client API takes no context, so closing that socket is
// the only way to interrupt a command already blocked on a read. Without it a
// reused connection could not be cancelled at all, and an in-flight DATA would
// run to SubmissionTimeout — ten minutes by default — long past
// server.shutdown_timeout. Closing the raw conn still ends a STARTTLS session,
// because the *tls.Conn above it wraps the same descriptor.
type pooled struct {
	client   *smtp.Client
	conn     net.Conn
	messages int
	usedAt   time.Time
}

// key identifies connections that are interchangeable.
//
// Everything that changes what a connection IS belongs in it. Two relays sharing
// an address but authenticating as different users are not interchangeable, and
// handing one's connection to the other would send mail as the wrong identity —
// which the relay would accept, since the session is already authenticated.
func poolKey(relay relays.Relay, opts Options) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s",
		relay.Addr(), relay.AuthUser, relay.TLSPolicy(), opts.LocalName)
}

// Get returns an open connection for the relay, the socket underneath it, and
// how many messages it has already carried — or nils when none is available.
//
// The raw conn comes back so the caller can arm its cancellation guard on a
// reused connection exactly as it does on a freshly dialled one. See pooled.conn
// for why that is the only mechanism available.
//
// The count comes back because it lives with the connection, not with any one
// delivery: MaxMessages is a property of the session the relay is counting, and
// a per-attempt counter would reset every time and never reach the limit.
//
// The returned connection has been probed with RSET. A relay that closed it
// while it sat idle — which they all eventually do — fails that probe and the
// connection is dropped here, so the caller sees "no pooled connection" rather
// than a delivery error.
//
// prepare is called on each candidate BEFORE it is probed, and is where the
// caller arms its cancellation guard and applies its timeouts. Both have to
// happen here rather than on the way out: RSET is a network round trip, and one
// against a relay that accepts TCP and then says nothing would otherwise be
// bounded by neither the context nor outbound.connect_timeout — the very defect
// the guard exists to prevent, on the half of the path that skips connect().
func (p *Pool) Get(relay relays.Relay, opts Options, prepare func(*smtp.Client, net.Conn)) (*smtp.Client, net.Conn, int) {
	if p == nil {
		return nil, nil, 0
	}
	key := poolKey(relay, opts)

	for {
		c := p.take(key)
		if c == nil {
			return nil, nil, 0
		}
		if p.IdleTimeout > 0 && time.Since(c.usedAt) > p.IdleTimeout {
			c.close()
			continue
		}
		if prepare != nil {
			prepare(c.client, c.conn)
		}
		// The probe. Cheap, and the only way to tell a live connection from one
		// the relay closed without telling us.
		if err := c.client.Reset(); err != nil {
			// Hard close, not quietQuit: the connection just failed a command,
			// so a polite QUIT is one more round trip to wait for on a socket
			// that has already proved it may never answer.
			_ = c.client.Close()
			continue
		}
		return c.client, c.conn, c.messages
	}
}

// Put offers a finished connection back to the pool.
//
// messages is how many this connection has now carried. A connection is only
// accepted if it is under every limit; otherwise it is closed here, so a caller
// can always hand its connection over and forget about it.
func (p *Pool) Put(relay relays.Relay, opts Options, c *smtp.Client, conn net.Conn, messages int) {
	if p == nil || c == nil {
		_ = quietQuit(c)
		return
	}
	if p.MaxMessages > 0 && messages >= p.MaxMessages {
		_ = quietQuit(c)
		return
	}

	key := poolKey(relay, opts)

	p.mu.Lock()
	if p.idle == nil {
		p.idle = map[string][]*pooled{}
	}
	max := p.MaxPerRelay
	if max <= 0 {
		max = 2
	}
	over := len(p.idle[key]) >= max
	if !over {
		p.idle[key] = append(p.idle[key],
			&pooled{client: c, conn: conn, messages: messages, usedAt: time.Now()})
	}
	p.mu.Unlock()

	if over {
		// Outside the lock, and that is not tidiness: quietQuit sends QUIT and
		// waits for the reply, bounded only by CommandTimeout. Held across p.mu
		// it would stop every other Get, Put and Reap in the process for up to
		// that long — the rule expired() already states.
		_ = quietQuit(c)
	}
}

// Discard closes a connection that must not be reused.
//
// Called for every failure. A connection whose last command errored is in an
// unknown protocol state, and reusing it would apply this message's failure to
// the next one — the single most damaging thing a pool can do.
func (p *Pool) Discard(c *smtp.Client) {
	_ = quietQuit(c)
}

// Close drops every pooled connection. Used at shutdown.
//
// Bounded by ctx, because every other step of the ordered teardown is: closing
// serially, each QUIT waiting up to CommandTimeout for a relay that may be the
// reason we are shutting down, is how a handful of sockets push past
// server.shutdown_timeout. When the deadline passes the sockets are dropped
// outright — the descriptor matters, the courtesy does not.
func (p *Pool) Close(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()

	var all []*pooled
	for _, cs := range idle {
		all = append(all, cs...)
	}
	if len(all) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, c := range all {
		wg.Add(1)
		go func(c *pooled) {
			defer wg.Done()
			c.close()
		}(c)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	if ctx == nil {
		<-done
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
		// Drop the sockets. This also unblocks the QUITs above, which are
		// reading from them.
		for _, c := range all {
			if c.conn != nil {
				_ = c.conn.Close()
			}
		}
	}
}

// Reap drops idle connections that have outlived IdleTimeout, until ctx ends.
//
// IdleTimeout is otherwise enforced only lazily, inside Get — so a relay that
// stops receiving traffic keeps its sockets and file descriptors until Close()
// at process shutdown, which on a gateway whose traffic moved to another relay
// group is indefinitely. The lazy check stays: it is what guarantees a
// connection handed out is fresh, which a reaper alone cannot promise.
//
// Runs only when outbound.reuse_connections is on, because that is the only
// case in which a Pool exists at all.
func (p *Pool) Reap(ctx context.Context, interval time.Duration) {
	if p == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, c := range p.expired() {
				c.close()
			}
		}
	}
}

// expired removes and returns everything past IdleTimeout.
//
// Collected under the lock and closed outside it, the shape Close already uses:
// quietQuit sends QUIT and waits for the reply, which is a network round trip
// and must not be held across the mutex the delivery path needs.
func (p *Pool) expired() []*pooled {
	if p.IdleTimeout <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-p.IdleTimeout)

	p.mu.Lock()
	defer p.mu.Unlock()

	var out []*pooled
	for key, cs := range p.idle {
		kept := cs[:0]
		for _, c := range cs {
			if c.usedAt.Before(cutoff) {
				out = append(out, c)
				continue
			}
			kept = append(kept, c)
		}
		p.setIdle(key, kept)
	}
	return out
}

func (p *Pool) take(key string) *pooled {
	p.mu.Lock()
	defer p.mu.Unlock()
	cs := p.idle[key]
	if len(cs) == 0 {
		return nil
	}
	// Most recently used first: it is the one least likely to have been closed
	// at the far end while it sat here.
	c := cs[len(cs)-1]
	p.setIdle(key, cs[:len(cs)-1])
	return c
}

// setIdle stores a key's connections, dropping the key entirely when it empties.
//
// Without this the map keeps a zero-length slice under every relay it has ever
// spoken to. Bounded by the relay table for an ordinary configuration — but
// poolKey is built from relay.Addr(), and use_mx mints one synthetic relay per
// exchanger, so the key set follows DNS rather than the configuration.
//
// Callers must already hold p.mu.
func (p *Pool) setIdle(key string, cs []*pooled) {
	if len(cs) == 0 {
		delete(p.idle, key)
		return
	}
	p.idle[key] = cs
}

func (c *pooled) close() { _ = quietQuit(c.client) }

// quietQuit ends a session politely, falling back to closing the socket.
//
// QUIT can fail for the same reason the connection is being dropped, and there
// is nothing useful to do about it — but Close must still happen, or the file
// descriptor leaks.
func quietQuit(c *smtp.Client) error {
	if c == nil {
		return nil
	}
	if err := c.Quit(); err != nil {
		return c.Close()
	}
	return nil
}
