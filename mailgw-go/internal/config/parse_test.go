package config

import (
	"net/netip"
	"strings"
	"testing"
)

// These tests used to assert that two entry points — bytes from a bundle and a
// path on disk — validated a configuration identically, because a divergence
// between them would have been a fail-open bug in the making. There is one
// entry point now, so what is left is the contract itself: every rejection
// must also deny.

// TestParseAllowlist_FailsClosedOverTheCorpus runs the whole corpus through
// ParseAllowlist and requires every failure to produce a deny-all list.
func TestParseAllowlist_FailsClosedOverTheCorpus(t *testing.T) {
	bodies := []string{
		`{"allowed":["127.0.0.1","::1","172.18.0.1"]}`,
		`{"allowed":["10.0.0.0/8","2001:db8::/32"]}`,
		`{"allowed":[],"allow_all":true}`,
		`{"allowed":["::ffff:127.0.0.1"]}`,
		// Everything below must fail, and must deny while doing so.
		`null`,
		`{}`,
		`{"allowed":"127.0.0.1"}`,
		`{"allowed":{"ip":"127.0.0.1"}}`,
		`{"allowed":5}`,
		`{"allowed":[]}`,
		`{"allowed":["not-an-ip"]}`,
		`{"allowed":["10.0.0.0/99"]}`,
		`{"allowed":[""]}`,
		`{"allowed":`,
	}

	for _, body := range bodies {
		got, err := ParseAllowlist([]byte(body), "allowlist profile")
		if err == nil {
			continue
		}
		// The fail-closed contract is the whole point: a caller that ignores
		// the error must still deny every peer.
		if allowed(t, got, "127.0.0.1") {
			t.Errorf("%s: a failed parse must deny every peer", body)
		}
	}
}

// A bundle whose allowlist key is absent arrives as a nil RawMessage. That must
// be a named error, not "unexpected end of JSON input", and it must deny.
func TestParseAllowlist_EmptyInputFailsClosed(t *testing.T) {
	for _, raw := range [][]byte{nil, {}} {
		a, err := ParseAllowlist(raw, "allowlist profile")
		if err == nil {
			t.Fatal("expected an error for empty allowlist bytes")
		}
		if !strings.Contains(err.Error(), "missing allowlist") {
			t.Errorf("unhelpful message for empty input: %v", err)
		}
		if a.Allowed(netip.MustParseAddr("127.0.0.1")) {
			t.Error("empty input must deny every peer")
		}
	}
}

// A server profile is always parsed strictly.
//
// This used to assert that the same bytes were ACCEPTED by a lax file-mode
// parse and REJECTED by a strict central one — the divergence that made `check`
// have to report which parser it had used. There is one parser now, and a
// misspelling is an error wherever it came from: silently dropping it leaves
// the default in place and the operator convinced they changed something.
func TestParseServer_StrictRejectsUnknownKeys(t *testing.T) {
	body := []byte("hostnmae: relay.example\nlisten:\n  - addr: \"0.0.0.0:2525\"\n")

	if _, err := ParseServer(body, FileServer); err == nil {
		t.Fatal("an unknown key must be rejected")
	} else if !strings.Contains(err.Error(), "hostnmae") {
		t.Errorf("the error must name the offending key, got: %v", err)
	}
}

func TestParseServer_DefaultsAndValidation(t *testing.T) {
	s, err := ParseServer(nil, FileServer)
	if err != nil {
		t.Fatalf("an empty server profile should yield the defaults: %v", err)
	}
	if s.Hostname != "localhost" || len(s.Listen) != 1 {
		t.Errorf("unexpected defaults: %+v", s)
	}
	if s.Outbound.SpoolDir != DefaultSpoolDir {
		t.Errorf("spool dir default = %q, want %q", s.Outbound.SpoolDir, DefaultSpoolDir)
	}

	if _, err := ParseServer([]byte(`hostname: ""`), FileServer); err == nil {
		t.Error("validation must still run on the byte path")
	}
}
