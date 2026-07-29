package uuidx

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// scraper mirrors tests/smtp/src/smtp.ts:143 exactly:
//
//	queued.raw.match(/\(([0-9A-F-]+)(?:\.\d+)?\)/i)?.[1]
//
// The client uses the captured group as the connection UUID and then searches
// the database with WHERE uuid LIKE '<captured>%'.
var scraper = regexp.MustCompile(`(?i)\(([0-9A-F-]+)(?:\.\d+)?\)`)

func TestNew_IsScrapeable(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := New()
		if !id.Valid() {
			t.Fatalf("New() produced an invalid id: %q", id)
		}
		reply := fmt.Sprintf("250 2.0.0 Message queued (%s)", id)
		m := scraper.FindStringSubmatch(reply)
		if m == nil {
			t.Fatalf("reply %q did not match the client's scraper", reply)
		}
		if m[1] != string(id) {
			t.Fatalf("scraper captured %q, want %q", m[1], id)
		}
	}
}

// The reply carries the transaction id (X.1), but the scraper's optional
// (?:\.\d+)? group strips the suffix, so the client ends up with the connection
// id X — which is what the LIKE 'X%' query needs.
func TestQueuedReply_ScraperCapturesConnectionRoot(t *testing.T) {
	conn := New()
	txn := conn.Child(1)

	reply := fmt.Sprintf("250 2.0.0 Message queued (%s)", txn)
	m := scraper.FindStringSubmatch(reply)
	if m == nil {
		t.Fatalf("reply %q did not match", reply)
	}
	if m[1] != string(conn) {
		t.Fatalf("captured %q, want the connection root %q", m[1], conn)
	}

	// And the delivery id must still share that prefix, so one LIKE finds all
	// three rows.
	del := txn.Child(1)
	for _, id := range []ID{conn, txn, del} {
		if !strings.HasPrefix(string(id), string(conn)) {
			t.Errorf("%q is not prefixed by the connection id %q", id, conn)
		}
	}
}

func TestChild_BuildsTheDocumentedHierarchy(t *testing.T) {
	conn := ID("ABCDEF01-2345-6789-ABCD-EF0123456789")

	txn1 := conn.Child(1)
	if got, want := string(txn1), string(conn)+".1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A second message on the same connection.
	txn2 := conn.Child(2)
	if got, want := string(txn2), string(conn)+".2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if txn1 == txn2 {
		t.Error("two messages on one connection must get distinct ids")
	}

	del := txn1.Child(1)
	if got, want := string(del), string(conn)+".1.1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRoot(t *testing.T) {
	conn := ID("ABCDEF01-2345-6789-ABCD-EF0123456789")
	cases := map[ID]ID{
		conn:                      conn,
		conn.Child(1):             conn,
		conn.Child(1).Child(1):    conn,
		conn.Child(12).Child(345): conn,
	}
	for in, want := range cases {
		if got := in.Root(); got != want {
			t.Errorf("%q.Root(): got %q, want %q", in, got, want)
		}
	}
}

func TestValid(t *testing.T) {
	valid := []ID{
		"ABCDEF01-2345-6789-ABCD-EF0123456789",
		"abcdef01-2345-6789-abcd-ef0123456789",
		"DEADBEEF",
		"DEADBEEF.1",
		"DEADBEEF.1.1",
		"DEADBEEF.12.345",
	}
	for _, id := range valid {
		if !id.Valid() {
			t.Errorf("%q should be valid", id)
		}
	}

	invalid := []ID{
		"",
		".",
		".1",
		"DEADBEEF.",
		"DEADBEEF..1",
		"DEADBEEF.a",       // non-numeric suffix
		"DEAD BEEF",        // space
		"DEADBEEF)",        // would break out of the reply parens
		"DEADBEEF\r\n250 ", // response splitting
		"../../etc/passwd", // path traversal via a spool filename
		"ZZZZ",             // not hex
	}
	for _, id := range invalid {
		if id.Valid() {
			t.Errorf("%q should be invalid", id)
		}
	}
}

// Valid() is the guard that keeps an id from breaking the reply it is embedded
// in; prove a rejected id really would have.
func TestValid_RejectsIDsThatWouldCorruptTheReply(t *testing.T) {
	bad := ID("DEADBEEF) 550 nope (")
	if bad.Valid() {
		t.Fatal("this id must be rejected")
	}
	reply := fmt.Sprintf("250 2.0.0 Message queued (%s)", bad)
	if m := scraper.FindStringSubmatch(reply); m != nil && m[1] == string(bad) {
		t.Fatal("scraper should not round-trip a corrupted id")
	}
}
