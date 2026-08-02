package queue

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
)

func newSpool(t *testing.T) *Spool {
	t.Helper()
	s, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}
	return s
}

// sample builds a valid envelope with the documented uuid hierarchy.
func sample(s *Spool, t *testing.T, uuidSuffix int) *Envelope {
	t.Helper()
	const conn = "ABCDEF01-2345-6789-ABCD-EF0123456789"
	txn := conn + ".1"

	body, size, err := s.WriteBody(txn, strings.NewReader("Subject: hi\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatalf("WriteBody: %v", err)
	}

	return &Envelope{
		Version:  EnvelopeVersion,
		UUID:     txn + "." + strconv.Itoa(uuidSuffix),
		TxnUUID:  txn,
		ConnUUID: conn,
		Body:     body,
		BodySize: size,
		MailFrom: "me@ngm.dev",
		Rcpts:    []Recipient{{Addr: "you@ngm.dev", Status: StatusPending}},
		RelayGrp: "Outbound",
		QueuedAt: time.Now().UnixMilli(),
	}
}

func TestSpool_RoundTrip(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)

	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ready, err := s.Ready(time.Now())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("ready: got %d, want 1", len(ready))
	}

	got, inflight, err := s.Claim(ready[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.UUID != e.UUID || got.MailFrom != e.MailFrom || got.RelayGrp != e.RelayGrp {
		t.Errorf("envelope did not round-trip: %+v", got)
	}
	if len(got.Rcpts) != 1 || got.Rcpts[0].Addr != "you@ngm.dev" {
		t.Errorf("recipients did not round-trip: %+v", got.Rcpts)
	}

	// Claimed envelopes leave the ready queue.
	if r, _ := s.Ready(time.Now()); len(r) != 0 {
		t.Errorf("claimed envelope still ready: %v", r)
	}

	if err := s.Complete(inflight, got); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rdy, infl, err := s.Len()
	if err != nil {
		t.Fatal(err)
	}
	if rdy != 0 || infl != 0 {
		t.Errorf("after Complete: ready=%d inflight=%d, want 0/0", rdy, infl)
	}
	// The body is collected once nothing references it.
	if _, err := os.Stat(s.BodyPath(e.Body)); !os.IsNotExist(err) {
		t.Error("body should have been removed with the last envelope")
	}
}

// Envelopes become due in order, and one not yet due is withheld.
func TestSpool_ReadyRespectsDueTime(t *testing.T) {
	s := newSpool(t)

	soon := sample(s, t, 1)
	soon.NextAt = time.Now().Add(-time.Minute).UnixMilli()
	later := sample(s, t, 2)
	later.NextAt = time.Now().Add(time.Hour).UnixMilli()

	for _, e := range []*Envelope{later, soon} {
		if err := s.Enqueue(e); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	ready, err := s.Ready(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 {
		t.Fatalf("ready: got %d, want 1 (the future one must be withheld)", len(ready))
	}
	e, _, err := s.Claim(ready[0])
	if err != nil {
		t.Fatal(err)
	}
	if e.UUID != soon.UUID {
		t.Errorf("got %q, want the due envelope %q", e.UUID, soon.UUID)
	}

	// Once time passes, the second becomes available.
	all, err := s.Ready(time.Now().Add(2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("later: got %d ready, want 1", len(all))
	}
}

// Filenames sort by due time, so a lexical sort is due order.
func TestSpool_ReadyIsSortedByDueTime(t *testing.T) {
	s := newSpool(t)
	base := time.Now().Add(-time.Hour)

	for i, offset := range []time.Duration{30 * time.Minute, 0, 15 * time.Minute} {
		e := sample(s, t, i+1)
		e.NextAt = base.Add(offset).UnixMilli()
		if err := s.Enqueue(e); err != nil {
			t.Fatal(err)
		}
	}

	ready, err := s.Ready(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 3 {
		t.Fatalf("got %d ready, want 3", len(ready))
	}
	var last int64
	for _, n := range ready {
		due, ok := parseDue(n)
		if !ok {
			t.Fatalf("cannot parse due time from %q", n)
		}
		if due < last {
			t.Errorf("ready list is not in due order: %v", ready)
		}
		last = due
	}
}

// The rename is the lock: only one claimer can win.
func TestSpool_ClaimIsExclusive(t *testing.T) {
	s := newSpool(t)
	if err := s.Enqueue(sample(s, t, 1)); err != nil {
		t.Fatal(err)
	}

	ready, _ := s.Ready(time.Now())
	if _, _, err := s.Claim(ready[0]); err != nil {
		t.Fatalf("first claim should succeed: %v", err)
	}
	if _, _, err := s.Claim(ready[0]); err == nil {
		t.Fatal("a second claim of the same envelope must fail")
	}
}

func TestSpool_Reschedule(t *testing.T) {
	s := newSpool(t)
	if err := s.Enqueue(sample(s, t, 1)); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.Ready(time.Now())
	e, inflight, err := s.Claim(ready[0])
	if err != nil {
		t.Fatal(err)
	}

	e.Attempts++
	e.LastErr = "451 temporary"
	future := time.Now().Add(time.Hour)
	if err := s.Reschedule(inflight, e, future); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}

	// Not due now...
	if r, _ := s.Ready(time.Now()); len(r) != 0 {
		t.Errorf("rescheduled envelope should not be due yet: %v", r)
	}
	// ...but due later, with its state preserved.
	r, _ := s.Ready(future.Add(time.Minute))
	if len(r) != 1 {
		t.Fatalf("got %d ready, want 1", len(r))
	}
	got, _, err := s.Claim(r[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 1 || got.LastErr != "451 temporary" {
		t.Errorf("retry state lost: attempts=%d lastErr=%q", got.Attempts, got.LastErr)
	}

	_, infl, _ := s.Len()
	if infl != 1 {
		t.Errorf("inflight: got %d, want 1", infl)
	}
}

// Startup recovery returns abandoned work to the queue, due immediately.
func TestSpool_RecoverReturnsInflightWork(t *testing.T) {
	s := newSpool(t)
	if err := s.Enqueue(sample(s, t, 1)); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.Ready(time.Now())
	if _, _, err := s.Claim(ready[0]); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash: the process dies with the envelope in flight.
	if r, _ := s.Ready(time.Now()); len(r) != 0 {
		t.Fatal("precondition: nothing should be ready")
	}

	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered: got %d, want 1", n)
	}

	r, _ := s.Ready(time.Now())
	if len(r) != 1 {
		t.Fatalf("after recovery: got %d ready, want 1", len(r))
	}
	_, infl, _ := s.Len()
	if infl != 0 {
		t.Errorf("inflight after recovery: got %d, want 0", infl)
	}
}

// A body shared by two envelopes survives until the last one completes.
func TestSpool_BodyIsRefCountedAcrossSplitEnvelopes(t *testing.T) {
	s := newSpool(t)

	first := sample(s, t, 1)
	second := sample(s, t, 2)
	second.Body = first.Body // both envelopes of one split message
	second.RelayGrp = "Exchange"

	for _, e := range []*Envelope{first, second} {
		if err := s.Enqueue(e); err != nil {
			t.Fatal(err)
		}
	}

	ready, _ := s.Ready(time.Now())
	if len(ready) != 2 {
		t.Fatalf("got %d ready, want 2", len(ready))
	}

	e1, in1, err := s.Claim(ready[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(in1, e1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.BodyPath(first.Body)); err != nil {
		t.Fatal("body must survive while a sibling envelope still references it")
	}

	e2, in2, err := s.Claim(ready[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(in2, e2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.BodyPath(first.Body)); !os.IsNotExist(err) {
		t.Error("body should be removed once the last envelope completes")
	}
}

// A torn write must never be visible as a queued envelope.
func TestSpool_PartialFilesAreNeverVisible(t *testing.T) {
	s := newSpool(t)

	// Drop a truncated file directly into q/, as an interrupted non-atomic
	// writer would have. The name is well-formed (12-digit due prefix); it is
	// the contents that are torn.
	bad := filepath.Join(s.root, dirReady, "001753776000.DEADBEEF.1.1.json")
	if err := os.WriteFile(bad, []byte(`{"v":1,"uuid":"DEADB`), 0o600); err != nil {
		t.Fatal(err)
	}

	ready, err := s.Ready(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 {
		t.Fatalf("got %d, want 1 (Ready lists by name)", len(ready))
	}
	// ...but claiming it fails and parks it in dead/ rather than looping.
	if _, _, err := s.Claim(ready[0]); err == nil {
		t.Fatal("claiming a torn envelope must fail")
	}
	deadFiles, _ := os.ReadDir(filepath.Join(s.root, dirDead))
	if len(deadFiles) != 1 {
		t.Errorf("torn envelope should be parked in dead/, got %d files", len(deadFiles))
	}
	if r, _ := s.Ready(time.Now()); len(r) != 0 {
		t.Error("torn envelope should no longer be ready")
	}
}

// WriteBody must not leave a readable file behind if the source fails midway.
func TestSpool_WriteBodyIsAtomic(t *testing.T) {
	s := newSpool(t)
	_, _, err := s.WriteBody("ABC.1", &failingReader{after: 4})
	if err == nil {
		t.Fatal("expected a write error")
	}
	if _, err := os.Stat(s.BodyPath("ABC.1.eml")); !os.IsNotExist(err) {
		t.Error("a failed body write must not publish a partial file")
	}
	// And the temp file is cleaned up.
	tmps, _ := os.ReadDir(filepath.Join(s.root, dirTmp))
	if len(tmps) != 0 {
		t.Errorf("temp files left behind: %d", len(tmps))
	}
}

type failingReader struct {
	after int
	n     int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n >= f.after {
		return 0, errBoom
	}
	n := copy(p, "aaaa")
	f.n += n
	return n, nil
}

var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

func TestSpool_Bury(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)
	e.IsDSN = true
	if err := s.Enqueue(e); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.Ready(time.Now())
	got, inflight, err := s.Claim(ready[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bury(inflight, got); err != nil {
		t.Fatalf("Bury: %v", err)
	}

	dead, _ := os.ReadDir(filepath.Join(s.root, dirDead))
	if len(dead) != 1 {
		t.Errorf("dead/: got %d files, want 1", len(dead))
	}
	rdy, infl, _ := s.Len()
	if rdy != 0 || infl != 0 {
		t.Errorf("after Bury: ready=%d inflight=%d, want 0/0", rdy, infl)
	}
}

func TestSpool_List(t *testing.T) {
	s := newSpool(t)
	if err := s.Enqueue(sample(s, t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(sample(s, t, 2)); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.Ready(time.Now())
	if _, _, err := s.Claim(ready[0]); err != nil {
		t.Fatal(err)
	}

	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("List: got %d, want 2 (ready plus inflight)", len(all))
	}
}

func TestLenAll_CountsQuarantineAndDead(t *testing.T) {
	s := newSpool(t)

	// Bury one first, while it is the only thing ready, so the claim below
	// cannot pick up the envelope that is meant to stay in q/.
	if err := s.Enqueue(sample(s, t, 1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, err := s.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("ReadyAndNext returned %d names, want 1", len(names))
	}
	e, inflight, err := s.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Bury(inflight, e); err != nil {
		t.Fatalf("Bury: %v", err)
	}

	if err := s.Enqueue(sample(s, t, 2)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := s.Quarantine(sample(s, t, 3)); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	got, err := s.LenAll()
	if err != nil {
		t.Fatalf("LenAll: %v", err)
	}
	want := Counts{Ready: 1, Inflight: 0, Quarantine: 1, Dead: 1}
	if got != want {
		t.Errorf("LenAll() = %+v, want %+v", got, want)
	}

	// Len keeps its old two-value contract; six call sites depend on it.
	ready, inflightN, err := s.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if ready != 1 || inflightN != 0 {
		t.Errorf("Len() = (%d, %d), want (1, 0)", ready, inflightN)
	}
}

// A message split across a relayed group and a quarantined one shares one body.
// Completing the relayed half must not delete it out from under the held one —
// gcBody used to scan only q/ and inflight/, so it did, and releasing the
// quarantined envelope would have handed a worker a pointer to nothing.
func TestSpool_QuarantinedSiblingKeepsTheSharedBody(t *testing.T) {
	s := newSpool(t)
	relayed := sample(s, t, 1)
	held := sample(s, t, 2)

	if relayed.Body != held.Body {
		t.Fatalf("test setup: siblings should share a body, got %q and %q", relayed.Body, held.Body)
	}

	if err := s.Enqueue(relayed); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := s.Quarantine(held); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	names, err := s.Ready(time.Now())
	if err != nil || len(names) != 1 {
		t.Fatalf("Ready = %v, %v; want one envelope", names, err)
	}
	_, inflight, err := s.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Complete(inflight, relayed); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, err := os.Stat(s.BodyPath(held.Body)); err != nil {
		t.Fatalf("body of the quarantined sibling was collected: %v", err)
	}
}

// The last envelope to go still takes the body with it.
func TestSpool_BodyGoesWithTheLastReferrer(t *testing.T) {
	s := newSpool(t)
	e := sample(s, t, 1)

	if err := s.Enqueue(e); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _ := s.Ready(time.Now())
	_, inflight, err := s.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Complete(inflight, e); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, err := os.Stat(s.BodyPath(e.Body)); !os.IsNotExist(err) {
		t.Fatalf("body survived its last referrer: %v", err)
	}
}

// gcBody rules an envelope out as a referrer from its filename alone, which is
// only sound because an envelope may not reference another transaction's body.
// Enforcing that at the boundary is what keeps the shortcut honest — without
// this check the borrower below would be queued, skipped by the name scan, and
// have its body deleted while it still needed it.
func TestEnvelope_CannotReferenceAnotherTransactionsBody(t *testing.T) {
	s := newSpool(t)
	owner := sample(s, t, 1)

	const otherConn = "99999999-8888-7777-6666-555555555555"
	borrower := &Envelope{
		Version:  EnvelopeVersion,
		UUID:     otherConn + ".1.1",
		TxnUUID:  otherConn + ".1",
		ConnUUID: otherConn,
		Body:     owner.Body,
		BodySize: owner.BodySize,
		MailFrom: "me@ngm.dev",
		Rcpts:    []Recipient{{Addr: "you@ngm.dev", Status: StatusPending}},
		RelayGrp: "Outbound",
		QueuedAt: time.Now().UnixMilli(),
	}

	err := s.Enqueue(borrower)
	if err == nil {
		t.Fatal("Enqueue accepted an envelope referencing another transaction's body")
	}
	if !strings.Contains(err.Error(), "another transaction") {
		t.Errorf("Enqueue error = %q, want it to name the problem", err)
	}
}

func TestSweepBodies_ReclaimsOrphansAndSparesTheRest(t *testing.T) {
	s := newSpool(t)
	live := sample(s, t, 1)
	if err := s.Enqueue(live); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// An orphan: a body a crash left behind with no envelope pointing at it.
	orphan, _, err := s.WriteBody("DEADBEEF-0000-0000-0000-000000000000.1",
		strings.NewReader("orphaned\r\n"))
	if err != nil {
		t.Fatalf("WriteBody: %v", err)
	}

	// minAge 0 sweeps everything old enough, which here is everything.
	n, err := s.SweepBodies(0)
	if err != nil {
		t.Fatalf("SweepBodies: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepBodies reclaimed %d bodies, want 1", n)
	}
	if _, err := os.Stat(s.BodyPath(orphan)); !os.IsNotExist(err) {
		t.Errorf("orphan survived the sweep: %v", err)
	}
	if _, err := os.Stat(s.BodyPath(live.Body)); err != nil {
		t.Errorf("swept a body a queued envelope still needs: %v", err)
	}
}

// The grace period is what keeps the sweep off a body a session has written but
// not yet enqueued — the window that spans the whole data-stage policy pass.
func TestSweepBodies_SparesRecentlyWrittenBodies(t *testing.T) {
	s := newSpool(t)
	fresh, _, err := s.WriteBody("DEADBEEF-0000-0000-0000-000000000000.1",
		strings.NewReader("mid-transaction\r\n"))
	if err != nil {
		t.Fatalf("WriteBody: %v", err)
	}

	n, err := s.SweepBodies(time.Hour)
	if err != nil {
		t.Fatalf("SweepBodies: %v", err)
	}
	if n != 0 {
		t.Fatalf("SweepBodies reclaimed %d bodies, want 0", n)
	}
	if _, err := os.Stat(s.BodyPath(fresh)); err != nil {
		t.Errorf("swept a body written inside the grace period: %v", err)
	}
}

// failed-events/rejected/ is a pile nothing retries and only a retention sweep
// drains, which is exactly the shape that goes unnoticed without a gauge.
func TestLenAll_CountsRejectedEventsSeparately(t *testing.T) {
	s := newSpool(t)

	pending := filepath.Join(s.FailedEventsDir(), "0000000000000000001.connection.0001.json")
	if err := os.WriteFile(pending, []byte(`{"kind":"connection"}`), 0o640); err != nil {
		t.Fatalf("write pending event: %v", err)
	}

	rejected := filepath.Join(s.FailedEventsDir(), events.RejectedDir)
	if err := os.MkdirAll(rejected, 0o750); err != nil {
		t.Fatalf("mkdir rejected/: %v", err)
	}
	for _, n := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(rejected, n), []byte(`{"kind":"connection"}`), 0o640); err != nil {
			t.Fatalf("write rejected event: %v", err)
		}
	}

	got, err := s.LenAll()
	if err != nil {
		t.Fatalf("LenAll: %v", err)
	}
	// The sub-directory must not inflate the pending count: rejected/ is a
	// directory entry, and only files ending in .json are counted.
	if got.FailedEvents != 1 {
		t.Errorf("FailedEvents = %d, want 1", got.FailedEvents)
	}
	if got.RejectedEvents != 2 {
		t.Errorf("RejectedEvents = %d, want 2", got.RejectedEvents)
	}
}

// rejected/ is created by the first rejection, so on a healthy gateway it never
// exists — absent means zero, not broken.
func TestLenAll_ToleratesAnAbsentRejectedDirectory(t *testing.T) {
	got, err := newSpool(t).LenAll()
	if err != nil {
		t.Fatalf("LenAll on a fresh spool: %v", err)
	}
	if got.RejectedEvents != 0 {
		t.Errorf("RejectedEvents = %d on a fresh spool, want 0", got.RejectedEvents)
	}
}
