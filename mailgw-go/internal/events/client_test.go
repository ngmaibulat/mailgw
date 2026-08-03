package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Timeout:    2 * time.Second,
		Retries:    2,
		BufferSize: 16,
		Senders:    1,
		SpillDir:   filepath.Join(t.TempDir(), "failed-events"),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Keep retries instant so the suite stays fast.
		sleep: func(context.Context, time.Duration) {},
	}
}

func TestClient_PostsSuccessfully(t *testing.T) {
	var gotBody atomic.Value
	var gotKey atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		gotKey.Store(r.Header.Get("X-API-Key"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	}))
	defer srv.Close()

	opts := testOptions(t)
	opts.APIKey = "secret-key"
	c := New(opts)
	c.Send(Envelope{Kind: KindDelivery, URL: srv.URL, Body: Delivery{UUID: "X.1.1", Port: "25"}})
	c.Close(context.Background())

	if got := c.Stats.Sent.Load(); got != 1 {
		t.Errorf("sent: got %d, want 1", got)
	}
	if got, _ := gotKey.Load().(string); got != "secret-key" {
		t.Errorf("X-API-Key: got %q", got)
	}
	body, _ := gotBody.Load().(string)
	var d Delivery
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("server received unparseable body %q: %v", body, err)
	}
	if d.UUID != "X.1.1" {
		t.Errorf("uuid: got %q", d.UUID)
	}
}

// logservice accepts every request when its own API_KEY is unset, so an empty
// key must simply omit the header rather than send an empty one.
func TestClient_OmitsAPIKeyHeaderWhenUnset(t *testing.T) {
	seen := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present := r.Header["X-Api-Key"]
		seen <- present
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(testOptions(t))
	c.Send(Envelope{Kind: KindQueue, URL: srv.URL, Body: Queue{}})
	c.Close(context.Background())

	if present := <-seen; present {
		t.Error("X-API-Key should be absent when no key is configured")
	}
}

func TestClient_RetriesServerErrorsThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(testOptions(t))
	c.Send(Envelope{Kind: KindConnection, URL: srv.URL, Body: Connection{}})
	c.Close(context.Background())

	if got := calls.Load(); got != 3 {
		t.Errorf("attempts: got %d, want 3", got)
	}
	if got := c.Stats.Sent.Load(); got != 1 {
		t.Errorf("sent: got %d, want 1", got)
	}
	if got := c.Stats.Retried.Load(); got != 2 {
		t.Errorf("retried: got %d, want 2", got)
	}
}

// A 400 means the payload does not match the schema. Retrying an identical body
// cannot help, so it must spill immediately — this is the failure mode that
// silently loses multi-recipient deliveries in the Haraka plugin today.
func TestClient_DoesNotRetrySchemaRejectionAndSpills(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"Fail"}`))
	}))
	defer srv.Close()

	opts := testOptions(t)
	c := New(opts)
	c.Send(Envelope{Kind: KindDelivery, URL: srv.URL, Body: Delivery{UUID: "X.1.1"}})
	c.Close(context.Background())

	if got := calls.Load(); got != 1 {
		t.Errorf("attempts: got %d, want 1 (a 400 must not be retried)", got)
	}
	if got := c.Stats.Rejected.Load(); got != 1 {
		t.Errorf("rejected: got %d, want 1", got)
	}
	if got := c.Stats.Sent.Load(); got != 0 {
		t.Errorf("sent: got %d, want 0", got)
	}

	files := spillFiles(t, opts.SpillDir)
	if len(files) != 1 {
		t.Fatalf("spill files: got %d, want 1", len(files))
	}
	var rec SpillRecord
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("spill record is not valid JSON: %v", err)
	}
	if rec.Kind != string(KindDelivery) {
		t.Errorf("spill kind: got %q", rec.Kind)
	}
	if rec.Reason != "http 400" {
		t.Errorf("spill reason: got %q", rec.Reason)
	}
	// The body must survive intact so a replayer can resend it.
	var d Delivery
	if err := json.Unmarshal(rec.Body, &d); err != nil || d.UUID != "X.1.1" {
		t.Errorf("spilled body did not round-trip: %v (%s)", err, rec.Body)
	}
}

func TestClient_SpillsAfterExhaustingRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	opts := testOptions(t)
	c := New(opts)
	c.Send(Envelope{Kind: KindQueue, URL: srv.URL, Body: Queue{UUID: "X.1"}})
	c.Close(context.Background())

	if got := c.Stats.Spilled.Load(); got != 1 {
		t.Errorf("spilled: got %d, want 1", got)
	}
	if got := len(spillFiles(t, opts.SpillDir)); got != 1 {
		t.Errorf("spill files: got %d, want 1", got)
	}
}

// An unreachable logservice must not block or fail mail flow.
func TestClient_HandlesUnreachableServer(t *testing.T) {
	opts := testOptions(t)
	c := New(opts)
	// Port 1 on loopback refuses connections immediately.
	c.Send(Envelope{Kind: KindConnection, URL: "http://127.0.0.1:1/api/connection", Body: Connection{}})
	c.Close(context.Background())

	if got := c.Stats.Sent.Load(); got != 0 {
		t.Errorf("sent: got %d, want 0", got)
	}
	if got := c.Stats.Spilled.Load(); got != 1 {
		t.Errorf("spilled: got %d, want 1", got)
	}
}

// Send must never block; a full buffer drops with a counter instead.
func TestClient_DropsWhenBufferIsFull(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	opts := testOptions(t)
	opts.BufferSize = 2
	opts.Senders = 1
	m := obs.New()
	opts.Metrics = m
	c := New(opts)

	// The first goes in flight and blocks the sender; the buffer then fills.
	const n = 50
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			c.Send(Envelope{Kind: KindConnection, URL: srv.URL, Body: Connection{}})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked; it must never apply backpressure to the SMTP path")
	}

	if got := c.Stats.Dropped.Load(); got == 0 {
		t.Error("expected some events to be dropped once the buffer filled")
	}
	if got := c.Stats.Queued.Load(); got != n {
		t.Errorf("queued: got %d, want %d", got, n)
	}
	// Stats is per-client and unreachable from outside this package, so until
	// this counter existed the one UNRECOVERABLE form of audit loss was also the
	// only one invisible to /metrics and the console heartbeat.
	if got := m.EventsDropped.Load(); got != c.Stats.Dropped.Load() {
		t.Errorf("events_dropped = %d, want %d", got, c.Stats.Dropped.Load())
	}
}

// Send after Close must be a no-op rather than a panic on a closed channel.
func TestClient_SendAfterCloseIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(testOptions(t))
	c.Close(context.Background())
	c.Close(context.Background()) // idempotent

	c.Send(Envelope{Kind: KindConnection, URL: srv.URL, Body: Connection{}})
	if got := c.Stats.Queued.Load(); got != 0 {
		t.Errorf("queued after close: got %d, want 0", got)
	}
}

// An empty URL means the endpoint is not configured; skip silently.
func TestClient_IgnoresEmptyURL(t *testing.T) {
	c := New(testOptions(t))
	c.Send(Envelope{Kind: KindConnection, URL: "", Body: Connection{}})
	c.Close(context.Background())
	if got := c.Stats.Queued.Load(); got != 0 {
		t.Errorf("queued: got %d, want 0", got)
	}
}

func spillFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read spill dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// Close must be bounded by its context. Without that, a full buffer against an
// unreachable logservice holds the process open for Retries+1 HTTP attempts plus
// backoff per event — long enough for the container runtime to SIGKILL it, which
// loses the events entirely instead of spilling them.
func TestClose_IsBoundedByItsContext(t *testing.T) {
	// A server that accepts the connection and never answers, so every attempt
	// runs to its timeout. The handler parks on a channel the test controls
	// rather than on the request context: httptest.Server.Close waits for
	// outstanding handlers, and a request context does not necessarily fire when
	// the client gives up on a pooled connection.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-stop
	}))
	defer func() { close(stop); srv.Close() }()

	spill := t.TempDir()
	c := New(Options{
		Timeout:    time.Second,
		Retries:    5,
		Senders:    1,
		BufferSize: 64,
		SpillDir:   spill,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	for range 20 {
		c.Send(Envelope{Kind: KindDelivery, URL: srv.URL, Body: map[string]string{"a": "b"}})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	c.Close(ctx)
	elapsed := time.Since(start)

	// Generous: the point is that it is bounded at all, not the exact figure.
	// Unbounded would be 20 events x 6 attempts x 1s.
	if elapsed > 5*time.Second {
		t.Fatalf("Close took %v; it ignored its context", elapsed)
	}

	// What could not be delivered must be on disk, not lost with the process.
	entries, err := os.ReadDir(spill)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("Close abandoned undelivered events instead of spilling them to failed-events/")
	}
}

// The ordinary path must not change: with a reachable logservice and a context
// that is not about to expire, Close still delivers everything.
func TestClose_StillDrainsWhenThereIsTime(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spill := t.TempDir()
	c := New(Options{
		Timeout:    time.Second,
		Senders:    2,
		BufferSize: 64,
		SpillDir:   spill,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	for range 10 {
		c.Send(Envelope{Kind: KindDelivery, URL: srv.URL, Body: map[string]string{"a": "b"}})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Close(ctx)

	if n := got.Load(); n != 10 {
		t.Errorf("logservice received %d events, want 10", n)
	}
	entries, _ := os.ReadDir(spill)
	if len(entries) != 0 {
		t.Errorf("%d events spilled despite the pipeline being healthy", len(entries))
	}
}
