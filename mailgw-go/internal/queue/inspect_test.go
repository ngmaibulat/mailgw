package queue

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpen_RefusesToCreateAMissingSpool(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-spool")

	if _, err := Open(dir); err == nil {
		t.Fatal("Open created or accepted a spool that does not exist")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("Open created the directory; mailq run as the wrong user would " +
			"leave root-owned directories beside the gateway's own")
	}
}

func TestOpen_AttachesToAnExistingSpool(t *testing.T) {
	made := newSpool(t)
	got, err := Open(made.Root())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Root() != made.Root() {
		t.Errorf("Open root = %q, want %q", got.Root(), made.Root())
	}
}

func TestListAll_CoversEveryQueueAndCarriesFilenames(t *testing.T) {
	s := newSpool(t)

	ready := sample(s, t, 1)
	if err := s.Enqueue(ready); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	held := sample(s, t, 2)
	if err := s.Quarantine(held); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	entries, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListAll returned %d entries, want 2", len(entries))
	}

	byQueue := map[string]Entry{}
	for _, e := range entries {
		byQueue[e.Queue] = e
		if e.Name == "" {
			t.Errorf("entry %s has no filename; flush, rm and release act on it", e.UUID())
		}
	}
	if _, ok := byQueue[QueueReady]; !ok {
		t.Error("ListAll missed the ready queue")
	}
	// The reason ListAll exists: List() skips quarantine entirely, and quarantine
	// is what an operator runs mailq to find.
	if _, ok := byQueue[QueueQuarantine]; !ok {
		t.Error("ListAll missed quarantine")
	}
}

// A torn or foreign file is exactly what someone running mailq is hunting for,
// so it is reported rather than silently skipped the way List does.
func TestListAll_ReportsUnreadableEnvelopes(t *testing.T) {
	s := newSpool(t)
	bad := filepath.Join(s.Root(), dirReady, "000000000001.GARBAGE.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListAll returned %d entries, want 1", len(entries))
	}
	if entries[0].Err == nil {
		t.Error("an unreadable envelope was reported as fine")
	}
	if entries[0].Env != nil {
		t.Error("an unreadable envelope came back with a parsed envelope")
	}
}

// The due prefix and the claim prefix both lead with a numeric dot-component.
// Stripping a claim prefix from a ready-queue name would eat the due time.
func TestListAll_ReadsTheDueTimeFromBothQueues(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	due := time.Now().Add(2 * time.Hour)
	e.NextAt = due.UnixMilli()
	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	entries, _ := s.ListAll()
	if got := entries[0].Due.Unix(); got != due.Unix() {
		t.Errorf("ready-queue due = %d, want %d", got, due.Unix())
	}

	names, _ := s.Ready(time.Now().Add(3 * time.Hour))
	if _, _, err := s.Claim(names[0]); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	entries, _ = s.ListAll()
	if entries[0].Queue != QueueInflight {
		t.Fatalf("entry queue = %q, want inflight", entries[0].Queue)
	}
	if got := entries[0].Due.Unix(); got != due.Unix() {
		t.Errorf("inflight due = %d, want %d — the claim prefix was mis-stripped", got, due.Unix())
	}
}

func TestRelease_RebuildsTheReadyQueueFilename(t *testing.T) {
	s := newSpool(t)
	held := sample(s, t, 1)
	if err := s.Quarantine(held); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	if err := s.Release(held.UUID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The real assertion. A bare rename would leave "<uuid>.json" in q/, whose
	// name parseDue rejects — so ReadyAndNext skips it, no worker ever claims it,
	// and the message silently never moves.
	names, err := s.Ready(time.Now())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("Ready found %d envelopes after release, want 1 — the released "+
			"envelope is in q/ under a name the scheduler cannot parse", len(names))
	}
	if n := countDir(t, s, dirQuarantine); n != 0 {
		t.Errorf("quarantine still holds %d envelopes after release", n)
	}
}

// A held message was stopped by a person, not by a failing relay. Restarting it
// on the far end of a backoff ladder it never earned would mean waiting hours
// for mail an operator just decided to send.
func TestRelease_ResetsTheRetryClock(t *testing.T) {
	s := newSpool(t)
	held := sample(s, t, 1)
	held.Attempts = 7
	if err := s.Quarantine(held); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	if err := s.Release(held.UUID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	entries, _ := s.ListAll()
	if entries[0].Env.Attempts != 0 {
		t.Errorf("attempts after release = %d, want 0", entries[0].Env.Attempts)
	}
	if entries[0].Due.After(time.Now().Add(time.Second)) {
		t.Errorf("released envelope is due at %v, want now", entries[0].Due)
	}
}

func TestRelease_RefusesAnythingNotInQuarantine(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := s.Release(e.UUID); err == nil {
		t.Error("Release accepted an envelope that was already in the ready queue")
	}
}

func TestFlush_MakesAnEnvelopeDueNow(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	e.NextAt = time.Now().Add(8 * time.Hour).UnixMilli()
	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if names, _ := s.Ready(time.Now()); len(names) != 0 {
		t.Fatalf("envelope is due before it was flushed")
	}
	if err := s.Flush(e.UUID); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	names, _ := s.Ready(time.Now())
	if len(names) != 1 {
		t.Errorf("Ready found %d envelopes after flush, want 1", len(names))
	}
}

func TestRemove_DeletesTheEnvelopeAndCollectsItsBody(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := s.Remove(e.UUID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n := countDir(t, s, dirReady); n != 0 {
		t.Errorf("q/ still holds %d envelopes", n)
	}
	if _, err := os.Stat(s.BodyPath(e.Body)); !os.IsNotExist(err) {
		t.Errorf("Remove left the body behind: %v", err)
	}
}

// Removing an inflight envelope does not cancel its delivery — the worker still
// finishes and calls Reschedule, which writes the ready-queue copy BEFORE
// removing the inflight one. So a permitted `rm` would delete the file and then
// watch the envelope reappear. Refusing is the only honest answer.
func TestRemove_RefusesAnEnvelopeBeingDelivered(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _ := s.Ready(time.Now())
	claimed, inflight, err := s.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	err = s.Remove(e.UUID)
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("Remove of an inflight envelope = %v, want ErrInFlight", err)
	}

	// And the resurrection this prevents: the worker finishes normally.
	if err := s.Reschedule(inflight, claimed, time.Now()); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	if n := countDir(t, s, dirReady); n != 1 {
		t.Errorf("q/ holds %d envelopes after the attempt finished, want 1", n)
	}
}

func TestFlushAndHold_RefuseAnEnvelopeBeingDelivered(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _ := s.Ready(time.Now())
	if _, _, err := s.Claim(names[0]); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := s.Flush(e.UUID); !errors.Is(err, ErrInFlight) {
		t.Errorf("Flush of an inflight envelope = %v, want ErrInFlight", err)
	}
	if err := s.Hold(e.UUID); !errors.Is(err, ErrInFlight) {
		t.Errorf("Hold of an inflight envelope = %v, want ErrInFlight", err)
	}
}

func TestHold_IsTheInverseOfRelease(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := s.Hold(e.UUID); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if n := countDir(t, s, dirQuarantine); n != 1 {
		t.Fatalf("quarantine holds %d envelopes, want 1", n)
	}
	if n := countDir(t, s, dirReady); n != 0 {
		t.Fatalf("q/ holds %d envelopes, want 0", n)
	}

	if err := s.Release(e.UUID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n := countDir(t, s, dirReady); n != 1 {
		t.Errorf("q/ holds %d envelopes after release, want 1", n)
	}
}

func TestQueueOperations_ReportAnUnknownUUID(t *testing.T) {
	s := newSpool(t)
	for name, fn := range map[string]func(string) error{
		"flush":   s.Flush,
		"rm":      s.Remove,
		"release": s.Release,
		"hold":    s.Hold,
	} {
		if err := fn("NOSUCH-0000"); !errors.Is(err, ErrNotQueued) {
			t.Errorf("%s of an unknown uuid = %v, want ErrNotQueued", name, err)
		}
	}
}
