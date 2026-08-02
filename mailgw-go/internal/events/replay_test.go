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
	"sync"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// spillDir builds a spill directory by running real events through a client
// pointed at a dead endpoint, so the files under test are the ones the gateway
// actually writes rather than a test's idea of them.
func spillDir(t *testing.T, n int) (dir string, m *obs.Metrics) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "failed-events")
	m = obs.New()

	c := New(Options{
		Timeout: 50 * time.Millisecond, Retries: 0, Senders: 1,
		SpillDir: dir, Logger: quiet(), Metrics: m,
	})
	for i := 0; i < n; i++ {
		c.Send(Envelope{
			Kind: KindConnection,
			URL:  "http://127.0.0.1:1/api/connection",
			Body: Connection{UUID: "AAAA", DT: int64(i)},
		})
	}
	c.Close(context.Background())

	if got := int(m.EventsSpilled.Load()); got != n {
		t.Fatalf("spilled %d events, want %d", got, n)
	}
	return dir, m
}

func replayer(t *testing.T, dir, url string, m *obs.Metrics) *Replayer {
	t.Helper()
	return &Replayer{
		Dir:     dir,
		Client:  New(Options{Timeout: time.Second, Senders: 1, Logger: quiet()}),
		URLFor:  func(Kind) string { return url },
		Log:     quiet(),
		Metrics: m,
	}
}

func TestReplay_DeliversAndDrainsTheDirectory(t *testing.T) {
	dir, m := spillDir(t, 3)

	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
	}))
	defer srv.Close()

	res, err := replayer(t, dir, srv.URL, m).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Replayed != 3 || res.Rejected != 0 || res.Deferred != 0 {
		t.Fatalf("result = %+v, want 3 replayed", res)
	}
	if m.EventsReplayed.Load() != 3 {
		t.Errorf("events_replayed = %d, want 3", m.EventsReplayed.Load())
	}

	left, err := (&Replayer{Dir: dir}).Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d file(s) left behind: %v", len(left), left)
	}

	// The body must be the payload logservice originally refused, byte for byte.
	mu.Lock()
	defer mu.Unlock()
	for _, b := range bodies {
		var got Connection
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("replayed body is not a connection payload: %v (%s)", err, b)
		}
		if got.UUID != "AAAA" {
			t.Errorf("uuid = %q", got.UUID)
		}
	}
}

// A 4xx is a schema mismatch. Retrying it forever would mean a replay pass that
// never drains and a log line every interval about the same file.
func TestReplay_A4xxIsTerminalAndFiledUnderRejected(t *testing.T) {
	dir, m := spillDir(t, 2)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	res, err := replayer(t, dir, srv.URL, m).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Rejected != 2 {
		t.Fatalf("result = %+v, want 2 rejected", res)
	}
	if m.EventsReplayFailed.Load() != 2 {
		t.Errorf("events_replay_failed = %d, want 2", m.EventsReplayFailed.Load())
	}

	left, _ := (&Replayer{Dir: dir}).Pending()
	if len(left) != 0 {
		t.Errorf("a permanently rejected event is still pending: %v", left)
	}
	// Set aside, not deleted: it is the evidence of what was refused.
	rejected, err := os.ReadDir(filepath.Join(dir, RejectedDir))
	if err != nil {
		t.Fatalf("read rejected/: %v", err)
	}
	if len(rejected) != 2 {
		t.Errorf("rejected/ holds %d files, want 2", len(rejected))
	}
}

// The normal reason a spill directory is full is that logservice is down. A pass
// that kept going would post a thousand times into a closed socket.
func TestReplay_StopsAfterConsecutiveTransportFailures(t *testing.T) {
	dir, m := spillDir(t, 20)

	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	res, err := replayer(t, dir, srv.URL, m).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Deferred != maxConsecutiveFailures {
		t.Errorf("deferred = %d, want %d", res.Deferred, maxConsecutiveFailures)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != maxConsecutiveFailures {
		t.Errorf("posted %d times to a failing server, want %d", calls, maxConsecutiveFailures)
	}

	// Everything must still be pending: a 5xx is not a verdict.
	left, _ := (&Replayer{Dir: dir}).Pending()
	if len(left) != 20 {
		t.Errorf("%d events still pending, want 20 — a deferred event must stay claimable", len(left))
	}
}

// A managed gateway's logservice URL arrives in a bundle and can change. An
// event recorded weeks ago must not be replayed at the address it was refused by.
func TestReplay_PrefersTheCurrentURLOverTheRecordedOne(t *testing.T) {
	dir, m := spillDir(t, 1)

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
	}))
	defer srv.Close()

	r := replayer(t, dir, srv.URL+"/somewhere/else", m)
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got != "/somewhere/else" {
		t.Errorf("posted to %q, want the configured endpoint", got)
	}
}

func TestReplay_FallsBackToTheRecordedURL(t *testing.T) {
	dir, m := spillDir(t, 1)

	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
	}))
	defer srv.Close()

	// Rewrite the record so it points somewhere reachable, then replay with no
	// URLFor at all.
	names, _ := (&Replayer{Dir: dir}).Pending()
	path := filepath.Join(dir, names[0])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec SpillRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	rec.URL = srv.URL
	out, _ := json.Marshal(rec)
	if err := os.WriteFile(path, out, 0o640); err != nil {
		t.Fatal(err)
	}

	r := replayer(t, dir, "", m)
	r.URLFor = nil
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !hit {
		t.Error("a record with no configured endpoint was not replayed to its recorded one")
	}
}

// A file being written by something that does not commit through a rename must
// be waited for, not destroyed.
func TestReplay_RecentUnparseableFilesAreLeftAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "failed-events")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "0000000000000000001.connection.0001.json")
	if err := os.WriteFile(path, []byte(`{"kind":"conn`), 0o640); err != nil {
		t.Fatal(err)
	}

	r := replayer(t, dir, "http://127.0.0.1:1", obs.New())
	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Deferred != 1 || res.Rejected != 0 {
		t.Fatalf("result = %+v, want the torn file deferred", res)
	}
	if left, _ := r.Pending(); len(left) != 1 {
		t.Errorf("the torn file was not returned to the pending set: %v", left)
	}

	// Once it is old enough, "unparseable" stops meaning "wait".
	r.now = func() time.Time { return time.Now().Add(2 * tornGrace) }
	res, err = r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Rejected != 1 {
		t.Fatalf("result = %+v, want the stale torn file set aside", res)
	}
}

// Two replayers over one directory is the ordinary case: the gateway's own pass
// and an operator running `mailgw-go events replay`.
func TestReplay_ConcurrentPassesDoNotDoublePost(t *testing.T) {
	dir, m := spillDir(t, 25)

	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[string(b)]++
		mu.Unlock()
	}))
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = replayer(t, dir, srv.URL, m).RunOnce(context.Background())
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 25 {
		t.Errorf("logservice saw %d distinct events, want 25", len(seen))
	}
	for body, n := range seen {
		if n != 1 {
			t.Errorf("an event was posted %d times: %s", n, body)
		}
	}
}

func TestReplay_MissingDirectoryIsNotAnError(t *testing.T) {
	r := replayer(t, filepath.Join(t.TempDir(), "never-created"), "http://127.0.0.1:1", obs.New())
	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("a gateway that never failed to post an event is not a broken one: %v", err)
	}
	if res.Total() != 0 {
		t.Errorf("result = %+v", res)
	}
}

// The fixed-width nanosecond prefix is what makes a lexical sort chronological,
// so the log tables fill in the order the events happened.
func TestPending_IsOldestFirst(t *testing.T) {
	dir, _ := spillDir(t, 5)
	names, err := (&Replayer{Dir: dir}).Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 5 {
		t.Fatalf("got %d names", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// Close's deadline must bound the event ALREADY IN FLIGHT, not only the ones
// still queued behind it.
//
// A sender that picked an event up before Close was called used to hold
// context.Background() for that event's whole retry schedule — six attempts and
// five backoffs, ~37 seconds on the shipped defaults — while shutdown waited on
// it. This test starts the send first and closes second, which is exactly that
// ordering; it fails by timing out, not by asserting.
func TestClose_BoundsTheEventAlreadyInFlight(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-stop
	}))
	defer func() { close(stop); srv.Close() }()

	dir := filepath.Join(t.TempDir(), "failed-events")
	c := New(Options{
		Timeout: 100 * time.Millisecond, Retries: 5, Senders: 1,
		Backoff:  func(int) time.Duration { return 10 * time.Second },
		SpillDir: dir, Logger: quiet(),
	})

	c.Send(Envelope{Kind: KindDelivery, URL: srv.URL, Body: map[string]string{"a": "b"}})
	// Long enough that the sender is certainly inside deliver() with the
	// pre-Close context, which is the whole point.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	c.Close(ctx)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Close took %v; the in-flight event held a context Close could not cancel", elapsed)
	}

	left, err := (&Replayer{Dir: dir}).Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("%d events spilled, want 1 — an abandoned event must reach disk", len(left))
	}
}

// os.WriteFile cannot promise a whole file; the replayer reads this directory
// while the gateway is still writing into it.
func TestSpill_CommitsThroughARename(t *testing.T) {
	dir, _ := spillDir(t, 30)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 30 {
		t.Fatalf("%d files for 30 spills — names are colliding", len(ents))
	}
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var rec SpillRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Errorf("%s is not a complete record: %v", e.Name(), err)
		}
	}
}

// writeRejected puts a file directly into rejected/ with a chosen age, which is
// the only way to test retention without waiting for one.
func writeRejected(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	rej := filepath.Join(dir, RejectedDir)
	if err := os.MkdirAll(rej, 0o750); err != nil {
		t.Fatalf("mkdir rejected/: %v", err)
	}
	p := filepath.Join(rej, name)
	if err := os.WriteFile(p, []byte(`{"kind":"connection"}`), 0o640); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return p
}

func TestSweepRejected_RemovesOnlyWhatIsPastRetention(t *testing.T) {
	dir, _ := spillDir(t, 0)

	old := writeRejected(t, dir, "0001.connection.0001.json", 48*time.Hour)
	fresh := writeRejected(t, dir, "0002.connection.0001.json", time.Hour)

	r := replayer(t, dir, "http://127.0.0.1:1", nil)
	r.Retention = 24 * time.Hour

	n, err := r.SweepRejected()
	if err != nil {
		t.Fatalf("SweepRejected: %v", err)
	}
	if n != 1 {
		t.Errorf("removed %d files, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("a rejected event past its retention is still there")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a rejected event inside its retention was removed: %v", err)
	}
}

// Zero keeps everything. These files are the evidence of what logservice
// refused, so "no retention configured" has to mean "never delete", not
// "delete immediately".
func TestSweepRejected_ZeroRetentionDeletesNothing(t *testing.T) {
	dir, _ := spillDir(t, 0)
	old := writeRejected(t, dir, "0001.connection.0001.json", 365*24*time.Hour)

	r := replayer(t, dir, "http://127.0.0.1:1", nil)
	n, err := r.SweepRejected()
	if err != nil {
		t.Fatalf("SweepRejected: %v", err)
	}
	if n != 0 {
		t.Errorf("removed %d files with retention unset, want 0", n)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("zero retention deleted a file: %v", err)
	}
}

// rejected/ is created by the first rejection, so on a healthy gateway it never
// exists — an absent directory means zero, not broken.
func TestSweepRejected_ToleratesAnAbsentDirectory(t *testing.T) {
	r := replayer(t, filepath.Join(t.TempDir(), "failed-events"), "http://127.0.0.1:1", nil)
	r.Retention = time.Hour

	n, err := r.SweepRejected()
	if err != nil {
		t.Fatalf("SweepRejected on an absent directory: %v", err)
	}
	if n != 0 {
		t.Errorf("removed %d files, want 0", n)
	}
}

// The sweep is not a separate ticker: it rides the replay pass, which is what
// couples it to events.replay_interval.
func TestReplay_RunSweepsRejectedAsWell(t *testing.T) {
	dir, _ := spillDir(t, 0)
	old := writeRejected(t, dir, "0001.connection.0001.json", 48*time.Hour)

	r := replayer(t, dir, "http://127.0.0.1:1", nil)
	r.Retention = 24 * time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx, 50*time.Millisecond) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(old); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("Run never swept rejected/")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}
