package queue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// M9.3: Reschedule and Bury commit the new copy before removing the inflight
// one, so a crash between those two steps leaves the same envelope in two
// places. Recover() re-stamps the due second, so the re-queued copy lands under
// a different filename than the rescheduled one — nothing collided, nothing
// deduped, and the message went out twice.

// The uuid root must be hex digits and dashes, and every component after it a
// decimal number — see uuidx.ID.Valid.
const testConnUUID = "AAAABBBB-CCCC-DDDD-EEEE-FFFF00001111"

func testEnvelope(suffix string) *Envelope {
	now := time.Now().UnixMilli()
	return &Envelope{
		Version:  EnvelopeVersion,
		UUID:     testConnUUID + suffix,
		TxnUUID:  testConnUUID + ".1",
		ConnUUID: testConnUUID,
		// An envelope may only reference its own transaction's body, which is
		// what lets gcBody resolve referrers from filenames.
		Body:     testConnUUID + ".1.eml",
		BodySize: 10,
		MailFrom: "sender@example.com",
		Rcpts:    []Recipient{{Addr: "rcpt@example.com", Status: StatusPending}},
		RelayGrp: "Outbound",
		QueuedAt: now,
		NextAt:   now,
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return len(entries)
}

// writeInflight fakes the state a crash mid-Reschedule leaves behind: the
// inflight file Claim created is still there.
func writeInflight(t *testing.T, s *Spool, e *Envelope, dueMillis int64) string {
	t.Helper()
	name := "4242." + readyName(dueMillis, e.UUID)
	if err := s.writeEnvelope(dirInflight, name, e); err != nil {
		t.Fatalf("write inflight: %v", err)
	}
	return name
}

func TestRecover_DropsInflightDuplicateOfRescheduledEnvelope(t *testing.T) {
	s, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	e := testEnvelope(".1.1")
	claimedDue := time.Now().UnixMilli()

	// The crash window: Reschedule wrote the new q/ copy, due in the future...
	e.Attempts = 1
	e.NextAt = time.Now().Add(5 * time.Minute).UnixMilli()
	if err := s.writeEnvelope(dirReady, readyName(e.NextAt, e.UUID), e); err != nil {
		t.Fatalf("write ready: %v", err)
	}
	// ...and the inflight copy it had not yet removed is still present.
	writeInflight(t, s, testEnvelope(".1.1"), claimedDue)

	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 0 {
		t.Errorf("Recover re-queued %d envelope(s); the rescheduled copy already covers it", n)
	}
	if got := countFiles(t, filepath.Join(s.root, dirReady)); got != 1 {
		t.Errorf("q/ holds %d envelopes, want 1 — the message would be delivered twice", got)
	}
	if got := countFiles(t, filepath.Join(s.root, dirInflight)); got != 0 {
		t.Errorf("inflight/ holds %d files, want 0", got)
	}

	// The surviving copy must be the newer, post-attempt one.
	entries, _ := os.ReadDir(filepath.Join(s.root, dirReady))
	got, err := readEnvelope(filepath.Join(s.root, dirReady, entries[0].Name()))
	if err != nil {
		t.Fatalf("read surviving envelope: %v", err)
	}
	if got.Attempts != 1 {
		t.Errorf("surviving envelope Attempts = %d, want 1 — the stale inflight copy won", got.Attempts)
	}
}

// Bury has the same shape, and getting it wrong is worse: the envelope was
// deliberately given up on, and resurrecting it puts it back in the retry loop.
func TestRecover_DropsInflightDuplicateOfBuriedEnvelope(t *testing.T) {
	s, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	e := testEnvelope(".1.2")
	if err := s.writeEnvelope(dirDead, e.UUID+".json", e); err != nil {
		t.Fatalf("write dead: %v", err)
	}
	writeInflight(t, s, e, time.Now().UnixMilli())

	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 0 {
		t.Errorf("Recover re-queued %d envelope(s); it was buried", n)
	}
	if got := countFiles(t, filepath.Join(s.root, dirReady)); got != 0 {
		t.Errorf("q/ holds %d envelopes, want 0 — a buried envelope was resurrected", got)
	}
	if got := countFiles(t, filepath.Join(s.root, dirInflight)); got != 0 {
		t.Errorf("inflight/ holds %d files, want 0", got)
	}
}

// The ordinary case must be untouched: a genuine crash mid-attempt leaves an
// inflight file with no committed twin, and that one has to come back.
func TestRecover_RequeuesOrphanedInflightEnvelope(t *testing.T) {
	s, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	e := testEnvelope(".1.3")
	// Due far in the future, so the re-stamp to "now" is observable.
	writeInflight(t, s, e, time.Now().Add(time.Hour).UnixMilli())

	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover re-queued %d envelope(s), want 1", n)
	}
	if got := countFiles(t, filepath.Join(s.root, dirInflight)); got != 0 {
		t.Errorf("inflight/ holds %d files, want 0", got)
	}

	ready, err := s.Ready(time.Now())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("Ready returned %d envelopes, want 1 — the due time was not re-stamped", len(ready))
	}
}

// Two distinct envelopes sharing a transaction body must both survive; the
// dedupe keys on the envelope uuid, not the transaction.
func TestRecover_KeepsDistinctSiblingEnvelopes(t *testing.T) {
	s, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	writeInflight(t, s, testEnvelope(".1.1"), time.Now().UnixMilli())
	writeInflight(t, s, testEnvelope(".1.2"), time.Now().UnixMilli())

	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 2 {
		t.Errorf("Recover re-queued %d envelopes, want 2", n)
	}
	if got := countFiles(t, filepath.Join(s.root, dirReady)); got != 2 {
		t.Errorf("q/ holds %d envelopes, want 2", got)
	}
}

func TestUUIDFromFilename(t *testing.T) {
	// The uuid itself contains dots, which is why these trim rather than split.
	if got, ok := uuidFromReadyName("000001234567.CONN.1.2.json"); !ok || got != "CONN.1.2" {
		t.Errorf("uuidFromReadyName: got %q %v, want \"CONN.1.2\" true", got, ok)
	}
	if got, ok := uuidFromPlainName("CONN.1.2.json"); !ok || got != "CONN.1.2" {
		t.Errorf("uuidFromPlainName: got %q %v, want \"CONN.1.2\" true", got, ok)
	}
	if _, ok := uuidFromReadyName("not-a-queue-file"); ok {
		t.Error("uuidFromReadyName accepted a foreign filename")
	}
	if _, ok := uuidFromPlainName("README"); ok {
		t.Error("uuidFromPlainName accepted a foreign filename")
	}
	if got := stripClaimPrefix("4242.000001234567.CONN.1.2.json"); got != "000001234567.CONN.1.2.json" {
		t.Errorf("stripClaimPrefix: got %q", got)
	}
	if got := stripClaimPrefix("000001234567.CONN.1.2.json"); got != "CONN.1.2.json" {
		// The due prefix is numeric too, so an unclaimed name loses it. Recover
		// only ever passes inflight names, which always carry the pid.
		t.Logf("note: stripClaimPrefix on an unclaimed name yields %q", got)
	}
}
