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
	if opts.sleep == nil {
		opts.sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}

	c := &Client{
		opts: opts,
		http: &http.Client{Timeout: opts.Timeout},
		ch:   make(chan Envelope, opts.BufferSize),
		log:  opts.Logger,
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
	if c == nil || c.closed.Load() || env.URL == "" {
		return
	}
	c.Stats.Queued.Add(1)
	select {
	case c.ch <- env:
	default:
		c.Stats.Dropped.Add(1)
		c.log.Warn("event buffer full, dropping", "kind", env.Kind, "buffer", cap(c.ch))
	}
}

// Close stops accepting events and waits for the in-flight ones to finish.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.ch)
	})
	c.wg.Wait()
}

func (c *Client) run() {
	defer c.wg.Done()
	for env := range c.ch {
		c.deliver(context.Background(), env)
	}
}

// deliver posts one event, retrying transient failures.
func (c *Client) deliver(ctx context.Context, env Envelope) {
	body, err := json.Marshal(env.Body)
	if err != nil {
		// Unmarshalable payloads are a programming error; spilling them would
		// not help since the replayer could not read them either.
		c.log.Error("cannot encode event", "kind", env.Kind, "err", err)
		return
	}

	var lastErr error
	for attempt := 0; attempt <= c.opts.Retries; attempt++ {
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

	rec := spillRecord{
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

	name := fmt.Sprintf("%d.%s.json", c.opts.now().UnixNano(), env.Kind)
	path := filepath.Join(c.opts.SpillDir, name)
	if err := os.WriteFile(path, enc, 0o640); err != nil {
		c.Stats.SpillFail.Add(1)
		c.log.Error("cannot spill event", "path", path, "err", err)
		return
	}
	c.Stats.Spilled.Add(1)
}

type spillRecord struct {
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
