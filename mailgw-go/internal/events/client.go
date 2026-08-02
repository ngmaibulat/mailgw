package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

// Stats counts what the pipeline did, for logging and /metrics.
type Stats struct {
	Queued    atomic.Int64
	Sent      atomic.Int64
	Dropped   atomic.Int64 // buffer full — never blocks the SMTP path
	Spilled   atomic.Int64 // written to disk after giving up
	Rejected  atomic.Int64 // 4xx: a schema mismatch, not worth retrying
	Retried   atomic.Int64
	SpillFail atomic.Int64
}

// Options configures a Client.
type Options struct {
	// Timeout bounds a single HTTP attempt.
	Timeout time.Duration
	// Retries is the number of retries after the first attempt.
	Retries int
	// BufferSize bounds the in-memory queue.
	BufferSize int
	// Senders is the number of concurrent sender goroutines.
	Senders int
	// APIKey is sent as X-API-Key when non-empty.
	APIKey string
	// SpillDir receives payloads that could not be delivered. Empty disables
	// spilling (they are counted and dropped instead).
	SpillDir string
	// Backoff returns the delay before retry n (1-based). Nil uses 1s/3s/9s.
	Backoff func(attempt int) time.Duration
	Logger  *slog.Logger
	// Metrics may be nil. It carries the counters an operator watches to know
	// whether the audit trail is keeping up — Stats is per-client and reachable
	// only from inside this package.
	Metrics *obs.Metrics
	// now and sleep exist so tests can run without real delays.
	now   func() time.Time
	sleep func(context.Context, time.Duration)
}

// Client is a bounded, asynchronous shipper for audit events.
//
// The SMTP path must never wait on it and must never fail because of it: mail
// delivery does not depend on the audit trail being reachable. Send is
// therefore non-blocking, and a full buffer drops with a counter rather than
// applying backpressure. This replaces the fire-and-forget POSTs in
// mailgw/plugins/functions.js:84, which had no timeout, no retry, and — because
// they never checked response.ok (functions.js:91) — no way to tell a dropped
// event from a delivered one.
type Client struct {
	opts Options
	http *http.Client
	ch   chan Envelope
	wg   sync.WaitGroup
	log  *slog.Logger

	closeOnce sync.Once
	closed    atomic.Bool
	// spillSeq disambiguates two spills that land in the same nanosecond.
	spillSeq atomic.Int64
	// shutdown is the context Close was given, published to the sender pool.
	// A pointer because atomic.Value refuses to store differing concrete types
	// and a context.Context is an interface.
	shutdown atomic.Pointer[context.Context]
	// done is closed by Close, so a backoff already sleeping wakes up. The
	// context alone is not enough: a sender that entered the sleep before Close
	// published its context is sleeping on Background.
	done chan struct{}

	Stats Stats
}

func defaultBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return time.Second
	case 2:
		return 3 * time.Second
	default:
		return 9 * time.Second
	}
}

// New starts the sender pool. Call Close to drain it.
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = 4096
	}
	if opts.Senders <= 0 {
		opts.Senders = 4
	}
	if opts.Backoff == nil {
		opts.Backoff = defaultBackoff
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	c := &Client{
		opts: opts,
		http: &http.Client{Timeout: opts.Timeout},
		ch:   make(chan Envelope, opts.BufferSize),
		log:  opts.Logger,
		done: make(chan struct{}),
	}

	// Defaulted after construction, not before, so it can select on c.done. A
	// backoff that had already started when Close was called would otherwise run
	// its full nine seconds before anything looked at the deadline again.
	if c.opts.sleep == nil {
		c.opts.sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-c.done:
			case <-t.C:
			}
		}
	}

	c.wg.Add(opts.Senders)
	for i := 0; i < opts.Senders; i++ {
		go c.run()
	}
	return c
}

// Send enqueues an event. It never blocks and never returns an error: a failure
// to audit must not become a failure to deliver mail.
func (c *Client) Send(env Envelope) {
	if c == nil || env.URL == "" {
		return
	}
	if c.closed.Load() {
		// Counted, not silently discarded. gateway.shutdown closes this pipeline
		// last, but it logs the case where the grace period expires with sessions
		// still open — and those sessions go on calling Send. The event is as
		// unrecoverable as a buffer-full drop, so it belongs in the counter whose
		// whole point is that those are the ones with no file on disk.
		c.Stats.Dropped.Add(1)
		if c.opts.Metrics != nil {
			c.opts.Metrics.EventsDropped.Add(1)
		}
		c.log.Warn("event arrived after the pipeline closed, dropping", "kind", env.Kind)
		return
	}
	c.Stats.Queued.Add(1)
	select {
	case c.ch <- env:
	default:
		c.Stats.Dropped.Add(1)
		// Stats is per-client and reachable only from inside this package, so
		// until this counter existed the one UNRECOVERABLE form of audit loss
		// was the only one invisible to /metrics and the console heartbeat —
		// EventsSpilled, EventsReplayed and EventsReplayFailed were all exposed,
		// and every one of those still has the row on disk.
		if c.opts.Metrics != nil {
			c.opts.Metrics.EventsDropped.Add(1)
		}
		c.log.Warn("event buffer full, dropping", "kind", env.Kind, "buffer", cap(c.ch))
	}
}

// Close stops accepting events and drains the buffered ones, bounded by ctx.
//
// The context is what makes shutdown bounded rather than "however long the audit
// trail takes". Each event costs up to Retries+1 HTTP attempts with backoff
// between them, so a full buffer against an unreachable logservice would
// otherwise hold the process open for minutes. When ctx expires, whatever is
// left spills to failed-events/ — which is exactly what that directory is for,
// and a far better outcome than being SIGKILLed with the events still in memory.
func (c *Client) Close(ctx context.Context) {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		// The context first, so a sender woken by done immediately afterwards
		// finds the deadline rather than Background.
		c.shutdown.Store(&ctx)
		close(c.done)
	})
	c.wg.Wait()

	// c.ch is deliberately NOT closed. Send checks c.closed and then sends, and
	// those two steps cannot be made atomic against a close from here — a
	// goroutine that passed the check a moment earlier would send on a closed
	// channel and panic the process, during shutdown, on the audit path. Senders
	// stop on c.done instead, and whatever raced in behind them is drained here.
	for {
		select {
		case env := <-c.ch:
			c.drop(env, "the pipeline closed while this event was in the buffer")
		default:
			return
		}
	}
}

// drop accounts for an event that will never be sent and has no file on disk.
func (c *Client) drop(env Envelope, why string) {
	c.Stats.Dropped.Add(1)
	if c.opts.Metrics != nil {
		c.opts.Metrics.EventsDropped.Add(1)
	}
	c.log.Warn("dropping an audit event", "kind", env.Kind, "reason", why)
}

// drainCtx is the context the sender pool delivers under: the one Close was
// given, or Background before Close is called.
func (c *Client) drainCtx() context.Context {
	if p := c.shutdown.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// run delivers events until Close, then drains what is buffered.
//
// Selecting on done rather than ranging over a closed channel is what lets Send
// stay a plain non-blocking send with no lock: nothing ever closes c.ch, so no
// send on it can panic. The drain terminates because the buffer is bounded and
// deliver is bounded by the shutdown context.
func (c *Client) run() {
	defer c.wg.Done()
	for {
		select {
		case env := <-c.ch:
			c.deliver(env)
		case <-c.done:
			for {
				select {
				case env := <-c.ch:
					c.deliver(env)
				default:
					return
				}
			}
		}
	}
}

// deliver posts one event, retrying transient failures.
func (c *Client) deliver(env Envelope) {
	body, err := json.Marshal(env.Body)
	if err != nil {
		// Unmarshalable payloads are a programming error; spilling them would
		// not help since the replayer could not read them either.
		c.log.Error("cannot encode event", "kind", env.Kind, "err", err)
		return
	}

	var lastErr error
	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
		// Re-read per attempt rather than captured once for the whole event.
		// A sender that had already picked an event up when Close was called
		// would otherwise hold context.Background() for its entire retry
		// schedule — five backoffs and six round trips, ~37s on the shipped
		// defaults — while shutdown waited on it. Re-reading bounds that window
		// to the single attempt already in flight.
		ctx := c.drainCtx()

		// Checked before every attempt, not only around the sleep. Cancelling the
		// backoff alone still leaves Retries+1 HTTP round trips per event, which
		// on a full buffer is the difference between a prompt shutdown and being
		// SIGKILLed part-way through one.
		if err := ctx.Err(); err != nil {
			c.spill(env, body, "shutdown: "+err.Error())
			return
		}
		if attempt > 0 {
			c.Stats.Retried.Add(1)
			c.opts.sleep(ctx, c.opts.Backoff(attempt))
		}

		status, err := c.post(ctx, env.URL, body)
		switch {
		case err == nil && status >= 200 && status < 300:
			c.Stats.Sent.Add(1)
			return

		case err == nil && status >= 400 && status < 500:
			// The payload does not match the server's schema. Retrying an
			// identical body cannot help, so record it and keep it for
			// inspection — this is exactly the failure that silently loses
			// multi-recipient deliveries today.
			c.Stats.Rejected.Add(1)
			c.log.Error("event rejected by logservice",
				"kind", env.Kind, "url", env.URL, "status", status)
			c.spill(env, body, fmt.Sprintf("http %d", status))
			return

		case err == nil:
			lastErr = fmt.Errorf("http %d", status)
		default:
			lastErr = err
		}
	}

	c.log.Error("event delivery failed", "kind", env.Kind, "url", env.URL, "err", lastErr)
	c.spill(env, body, errString(lastErr))
}

func (c *Client) post(ctx context.Context, url string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.opts.APIKey != "" {
		req.Header.Set("X-API-Key", c.opts.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// spill writes an undeliverable payload to disk for a later replay.
func (c *Client) spill(env Envelope, body []byte, reason string) {
	if c.opts.SpillDir == "" {
		return
	}
	if err := os.MkdirAll(c.opts.SpillDir, 0o750); err != nil {
		c.Stats.SpillFail.Add(1)
		c.log.Error("cannot create spill dir", "dir", c.opts.SpillDir, "err", err)
		return
	}

	rec := SpillRecord{
		Kind:   string(env.Kind),
		URL:    env.URL,
		Reason: reason,
		At:     c.opts.now().UTC().Format(time.RFC3339Nano),
		Body:   json.RawMessage(body),
	}
	enc, err := json.Marshal(rec)
	if err != nil {
		c.Stats.SpillFail.Add(1)
		return
	}

	path := filepath.Join(c.opts.SpillDir, c.spillName(env.Kind))
	if err := writeAtomic(c.opts.SpillDir, path, enc); err != nil {
		c.Stats.SpillFail.Add(1)
		c.log.Error("cannot spill event", "path", path, "err", err)
		return
	}
	c.Stats.Spilled.Add(1)
	if c.opts.Metrics != nil {
		c.opts.Metrics.EventsSpilled.Add(1)
	}
}

// spillName builds a filename that sorts by spill time and cannot collide.
//
// The nanosecond field is zero-padded to a fixed 19 digits, so a lexical sort of
// the directory is chronological — the same trick the spool's 12-digit due
// prefix plays, and what lets the replayer resend oldest-first without opening
// anything. The sequence suffix is because four sender goroutines can spill
// inside the same nanosecond on a coarse clock, and the second write would
// otherwise silently overwrite the first.
func (c *Client) spillName(kind Kind) string {
	return fmt.Sprintf("%019d.%s.%04d.json",
		c.opts.now().UnixNano(), kind, c.spillSeq.Add(1)%10000)
}

// writeAtomic commits through a rename, so a replayer scanning this directory
// while the gateway is still spilling into it can never read half a record.
// os.WriteFile cannot promise that.
func writeAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpName, 0o640); err != nil {
		return err
	}
	err = os.Rename(tmpName, path)
	return err
}

// SpillRecord is one undeliverable audit event, as it sits on disk.
//
// Exported so the replayer can be written against a documented shape rather than
// re-deriving it — and so a change to it is visibly a change to an on-disk
// format, not an internal struct edit.
type SpillRecord struct {
	Kind   string          `json:"kind"`
	URL    string          `json:"url"`
	Reason string          `json:"reason"`
	At     string          `json:"at"`
	Body   json.RawMessage `json:"body"`
}

// APIKeyFromEnv reads the API key from the named environment variable.
// logservice accepts every request when its own API_KEY is unset, so an empty
// value here is valid rather than an error.
func APIKeyFromEnv(name string) string {
	if name == "" {
		name = "API_KEY"
	}
	return os.Getenv(name)
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}
