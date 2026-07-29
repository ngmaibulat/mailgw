package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// write drops content into a temp ngmfilter.json and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ngmfilter.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func mustLoad(t *testing.T, content string) *Allowlist {
	t.Helper()
	a, err := LoadAllowlist(write(t, content))
	if err != nil {
		t.Fatalf("LoadAllowlist: unexpected error: %v", err)
	}
	return a
}

func allowed(t *testing.T, a *Allowlist, s string) bool {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("bad test address %q: %v", s, err)
	}
	return a.Allowed(addr)
}

// The four cases asserted by mailgw/tests/npFilter.test.js.

func TestAllowlist_AllowsListedIP(t *testing.T) {
	a := mustLoad(t, `{"allowed":["127.0.0.1","::1","172.18.0.1"]}`)
	for _, ip := range []string{"127.0.0.1", "::1", "172.18.0.1"} {
		if !allowed(t, a, ip) {
			t.Errorf("%s should be allowed", ip)
		}
	}
}

func TestAllowlist_DeniesUnlistedIP(t *testing.T) {
	a := mustLoad(t, `{"allowed":["127.0.0.1"]}`)
	for _, ip := range []string{"10.0.0.1", "203.0.113.10", "2001:db8::1"} {
		if allowed(t, a, ip) {
			t.Errorf("%s should be denied", ip)
		}
	}
}

// npFilter.js:52 treats a null config as fatal and denies every connection.
func TestAllowlist_FailsClosedOnNullConfig(t *testing.T) {
	a, err := LoadAllowlist(write(t, `null`))
	if err == nil {
		t.Fatal("expected an error for a null config")
	}
	if allowed(t, a, "127.0.0.1") {
		t.Error("a failed load must deny every peer")
	}
}

// npFilter.js:52 also requires `allowed` to be an array specifically.
func TestAllowlist_FailsClosedWhenAllowedIsNotAnArray(t *testing.T) {
	for _, body := range []string{
		`{"allowed":"127.0.0.1"}`,
		`{"allowed":{"ip":"127.0.0.1"}}`,
		`{"allowed":5}`,
		`{}`,
	} {
		a, err := LoadAllowlist(write(t, body))
		if err == nil {
			t.Errorf("%s: expected an error", body)
		}
		if allowed(t, a, "127.0.0.1") {
			t.Errorf("%s: a failed load must deny every peer", body)
		}
	}
}

func TestAllowlist_FailsClosedOnMissingFile(t *testing.T) {
	a, err := LoadAllowlist(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if allowed(t, a, "127.0.0.1") {
		t.Error("a failed load must deny every peer")
	}
}

func TestAllowlist_ZeroValueDeniesEverything(t *testing.T) {
	var a Allowlist
	if allowed(t, &a, "127.0.0.1") {
		t.Error("the zero value must deny every peer")
	}
	if (*Allowlist)(nil).Allowed(netip.MustParseAddr("127.0.0.1")) {
		t.Error("a nil allowlist must deny every peer")
	}
}

// New behaviour beyond Haraka parity: CIDR blocks.

func TestAllowlist_CIDR(t *testing.T) {
	a := mustLoad(t, `{"allowed":["10.0.0.0/8","2001:db8::/32"]}`)
	cases := map[string]bool{
		"10.0.0.1":       true,
		"10.255.255.254": true,
		"11.0.0.1":       false,
		"2001:db8::1":    true,
		"2001:db9::1":    false,
	}
	for ip, want := range cases {
		if got := allowed(t, a, ip); got != want {
			t.Errorf("%s: got %v, want %v", ip, got, want)
		}
	}
}

// A CIDR with host bits set should still behave as the masked network rather
// than silently matching nothing.
func TestAllowlist_CIDRWithHostBitsIsMasked(t *testing.T) {
	a := mustLoad(t, `{"allowed":["10.1.2.3/8"]}`)
	if !allowed(t, a, "10.9.9.9") {
		t.Error("10.9.9.9 should match 10.1.2.3/8 once masked to 10.0.0.0/8")
	}
}

// A dual-stack listener reports v4 peers as ::ffff:a.b.c.d; those must match a
// plain "127.0.0.1" entry.
func TestAllowlist_V4MappedV6MatchesV4Entry(t *testing.T) {
	a := mustLoad(t, `{"allowed":["127.0.0.1","10.0.0.0/8"]}`)
	if !allowed(t, a, "::ffff:127.0.0.1") {
		t.Error("::ffff:127.0.0.1 should match the 127.0.0.1 entry")
	}
	if !allowed(t, a, "::ffff:10.1.1.1") {
		t.Error("::ffff:10.1.1.1 should match the 10.0.0.0/8 entry")
	}
}

// ...and the reverse: a v4-mapped entry should match an unmapped v4 peer.
func TestAllowlist_V4MappedEntryMatchesV4Peer(t *testing.T) {
	a := mustLoad(t, `{"allowed":["::ffff:127.0.0.1"]}`)
	if !allowed(t, a, "127.0.0.1") {
		t.Error("127.0.0.1 should match a ::ffff:127.0.0.1 entry")
	}
}

func TestAllowlist_EmptyArrayRequiresExplicitAllowAll(t *testing.T) {
	a, err := LoadAllowlist(write(t, `{"allowed":[]}`))
	if err == nil {
		t.Fatal("an empty allowlist must be an error unless allow_all is set")
	}
	if allowed(t, a, "127.0.0.1") {
		t.Error("a failed load must deny every peer")
	}

	b := mustLoad(t, `{"allowed":[],"allow_all":true}`)
	if !allowed(t, b, "203.0.113.10") {
		t.Error("allow_all should accept any peer")
	}
}

func TestAllowlist_RejectsMalformedEntry(t *testing.T) {
	for _, body := range []string{
		`{"allowed":["not-an-ip"]}`,
		`{"allowed":["10.0.0.0/99"]}`,
		`{"allowed":[""]}`,
	} {
		a, err := LoadAllowlist(write(t, body))
		if err == nil {
			t.Errorf("%s: expected an error", body)
		}
		if allowed(t, a, "127.0.0.1") {
			t.Errorf("%s: a failed load must deny every peer", body)
		}
	}
}

func TestAllowlist_AllowedHostPort(t *testing.T) {
	a := mustLoad(t, `{"allowed":["127.0.0.1"]}`)
	if !a.AllowedHostPort("127.0.0.1:54321") {
		t.Error("host:port form should be accepted")
	}
	if !a.AllowedHostPort("127.0.0.1") {
		t.Error("bare address form should be accepted")
	}
	if a.AllowedHostPort("10.0.0.1:25") {
		t.Error("unlisted peer should be denied")
	}
	if a.AllowedHostPort("garbage") {
		t.Error("unparseable address must be denied, not allowed")
	}
}
