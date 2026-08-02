package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

// age backdates a file, standing in for one that was spilled long ago.
func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func rejectedFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, RejectedDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read rejected/: %v", err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// rename(2) preserves mtime, so a rejected file's mtime is when the event was
// SPILLED, not when it was rejected. An event spilled longer ago than the
// retention was therefore filed under rejected/ and deleted by the sweep at the
// end of the very same pass — the evidence destroyed before anyone could look
// at it, while events_replay_failed climbed and the gauge stayed at zero.
func TestReplay_ARejectedEventSurvivesTheSweepInTheSamePass(t *testing.T) {
	dir, m := spillDir(t, 1)

	// The event has been sitting in failed-events/ for a fortnight.
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("%d spilled files, want 1", len(names))
	}
	age(t, filepath.Join(dir, names[0].Name()), 14*24*time.Hour)

	// logservice refuses it today.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	r := replayer(t, dir, srv.URL, m)
	r.Retention = 7 * 24 * time.Hour

	// One tick of Run: a pass, then the sweep, in that order.
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := r.SweepRejected(); err != nil {
		t.Fatalf("SweepRejected: %v", err)
	}

	if got := rejectedFiles(t, dir); len(got) != 1 {
		t.Fatalf("rejected/ holds %v, want the one refused event: the sweep "+
			"deleted it on the pass that filed it", got)
	}
}

// A claim is the lock over one file, and a process killed between the claim and
// the outcome used to leave it named so that nothing would ever look at it
// again — invisible to Pending(), to `mailgw-go events` and to every counter,
// while Spool.LenAll went on counting it for ever.
func TestReclaim_ReturnsAnAbandonedClaimToThePendingSet(t *testing.T) {
	dir, _ := spillDir(t, 1)

	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read spill dir: %v", err)
	}
	orig := names[0].Name()

	// Exactly what a killed process leaves behind.
	claimed := filepath.Join(dir, ".claim-4242-"+
		strconv.FormatInt(time.Now().Add(-time.Hour).UnixNano(), 10)+"-"+orig)
	if err := os.Rename(filepath.Join(dir, orig), claimed); err != nil {
		t.Fatalf("rename: %v", err)
	}

	r := replayer(t, dir, "http://127.0.0.1:1", obs.New())
	if pending, _ := r.Pending(); len(pending) != 0 {
		t.Fatalf("Pending() = %v; a claimed file must not be listed as work", pending)
	}

	n, err := r.Reclaim()
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d files, want 1", n)
	}
	pending, err := r.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0] != orig {
		t.Errorf("Pending() = %v, want [%s]", pending, orig)
	}
}

// A claim taken moments ago belongs to a live pass. Reclaiming it would post
// the same audit row twice.
func TestReclaim_LeavesAFreshClaimAlone(t *testing.T) {
	dir, _ := spillDir(t, 1)

	names, _ := os.ReadDir(dir)
	orig := names[0].Name()
	claimed := filepath.Join(dir, ".claim-4242-"+strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+orig)
	if err := os.Rename(filepath.Join(dir, orig), claimed); err != nil {
		t.Fatalf("rename: %v", err)
	}

	r := replayer(t, dir, "http://127.0.0.1:1", obs.New())
	n, err := r.Reclaim()
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if n != 0 {
		t.Errorf("reclaimed %d files; a claim this new is in flight", n)
	}
}

// An event handed over after Close is as unrecoverable as a buffer-full drop —
// there is no file on disk for it — and it used to return silently, so the one
// counter that means "gone" missed the loss it exists to report. The send must
// also not panic: nothing may close the channel underneath a caller.
func TestSend_AfterCloseIsCountedAndDoesNotPanic(t *testing.T) {
	m := obs.New()
	c := New(Options{
		Timeout: 50 * time.Millisecond, Retries: 0, Senders: 1,
		SpillDir: t.TempDir(), Logger: quiet(), Metrics: m,
	})
	c.Close(context.Background())

	for range 3 {
		c.Send(Envelope{
			Kind: KindConnection,
			URL:  "http://127.0.0.1:1/api/connection",
			Body: Connection{UUID: "AAAA"},
		})
	}

	if got := m.EventsDropped.Load(); got != 3 {
		t.Errorf("events_dropped = %d, want 3", got)
	}
	if got := c.Stats.Dropped.Load(); got != 3 {
		t.Errorf("Stats.Dropped = %d, want 3", got)
	}
}

// Close is called from more than one place at shutdown, and a second one must
// be a no-op rather than a panic on a channel closed twice.
func TestClose_IsSafeToCallTwice(t *testing.T) {
	c := New(Options{
		Timeout: 50 * time.Millisecond, Retries: 0, Senders: 2,
		SpillDir: t.TempDir(), Logger: quiet(), Metrics: obs.New(),
	})
	c.Close(context.Background())
	c.Close(context.Background())
}
