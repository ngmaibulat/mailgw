// Package config parses and validates the configuration bundle Central
// Management deploys. There is no other source: nothing here reads a
// configuration directory, and the gateway reads no environment.
package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
)

// Allowlist decides which peers may open an SMTP session.
//
// This is the only gate protecting the relay: the recipient stage accepts every
// address (parity with mailgw/plugins/npFilter.js:73, which returns OK
// unconditionally). It therefore fails closed by construction — the zero value
// permits nothing, so a caller that ignores a load error still denies traffic.
type Allowlist struct {
	prefixes []netip.Prefix
	allowAll bool
}

// ngmfilterFile is the on-disk shape of ngmfilter.json. `allowed` is typed as
// json.RawMessage rather than []string so that a non-array value (e.g. a bare
// string) is reported as a validation error instead of being silently coerced —
// mailgw/tests/npFilter.test.js asserts this case denies all connections.
type ngmfilterFile struct {
	Allowed  json.RawMessage `json:"allowed"`
	AllowAll bool            `json:"allow_all"`
}

// ParseAllowlist parses an allowlist profile's bytes. name appears only in
// error messages, and is the bundle key.
//
// The fail-closed contract is the point of it, and it is inherited from
// mailgw/plugins/npFilter.js:52-57: every failure path returns a deny-all
// Allowlist alongside the error, so a caller that ignores the error still
// denies traffic. There used to be a LoadAllowlist beside this that read the
// same bytes off disk; nothing on this host supplies configuration any more.
func ParseAllowlist(raw []byte, name string) (*Allowlist, error) {
	// A bundle whose allowlist key is absent arrives as a nil RawMessage, and
	// json.Unmarshal would report "unexpected end of JSON input", which says
	// nothing about what is actually wrong.
	if len(raw) == 0 {
		return &Allowlist{}, fmt.Errorf("%s: missing allowlist", name)
	}

	// A literal `null` unmarshals into the zero struct without error, so the
	// Allowed==nil check below is what actually catches it.
	var f ngmfilterFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return &Allowlist{}, fmt.Errorf("parse %s: %w", name, err)
	}
	if len(f.Allowed) == 0 {
		return &Allowlist{}, fmt.Errorf("%s: missing 'allowed' array", name)
	}

	var entries []string
	if err := json.Unmarshal(f.Allowed, &entries); err != nil {
		return &Allowlist{}, fmt.Errorf("%s: 'allowed' must be an array of strings: %w", name, err)
	}

	list := &Allowlist{allowAll: f.AllowAll}
	for i, e := range entries {
		p, err := parseEntry(e)
		if err != nil {
			return &Allowlist{}, fmt.Errorf("%s: allowed[%d] %q: %w", name, i, e, err)
		}
		list.prefixes = append(list.prefixes, p)
	}

	// An empty allowlist would be indistinguishable from a misconfiguration, and
	// getting it wrong in the other direction opens the relay. Require the
	// operator to say so explicitly.
	if len(list.prefixes) == 0 && !list.allowAll {
		return &Allowlist{}, fmt.Errorf("%s: 'allowed' is empty; set \"allow_all\": true to accept every peer", name)
	}

	return list, nil
}

// ParsePrefixes parses a list of bare addresses and CIDR blocks, the same syntax
// ngmfilter.json's "allowed" uses.
//
// Exported so listen[].proxy_trusted can be validated and built from the same
// parser the allowlist uses — including its Masked() normalisation and its
// ::ffff: rewrite — rather than internal/proxyproto growing a second one and a
// dependency on this package.
func ParsePrefixes(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		p, err := parseEntry(e)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", e, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// parseEntry accepts either a bare address ("127.0.0.1", "::1") or a CIDR
// block ("10.0.0.0/8"). Bare addresses become single-host prefixes.
func parseEntry(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Prefix{}, fmt.Errorf("empty entry")
	}

	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		// Masked() zeroes host bits so that a sloppy "10.1.2.3/8" still behaves
		// as 10.0.0.0/8 rather than failing to match anything.
		return normalizePrefix(p.Masked()), nil
	}

	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// normalizePrefix rewrites an IPv4-mapped IPv6 prefix (::ffff:a.b.c.d/N) into
// its plain IPv4 form so it can match an unmapped v4 peer address.
func normalizePrefix(p netip.Prefix) netip.Prefix {
	if !p.Addr().Is4In6() {
		return p
	}
	// The mapped form carries a 96-bit prefix of v4-mapped padding.
	bits := p.Bits() - 96
	if bits < 0 {
		return p
	}
	return netip.PrefixFrom(p.Addr().Unmap(), bits)
}

// Allowed reports whether addr may open a session. IPv4-mapped IPv6 addresses
// are unmapped first, so a peer arriving as ::ffff:127.0.0.1 on a dual-stack
// listener matches a "127.0.0.1" entry.
func (a *Allowlist) Allowed(addr netip.Addr) bool {
	if a == nil {
		return false
	}
	if a.allowAll {
		return true
	}
	if !addr.IsValid() {
		return false
	}

	addr = addr.Unmap()
	for _, p := range a.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// AllowedHostPort is a convenience wrapper for a net.Addr string of the form
// "host:port", which is what net.Conn.RemoteAddr().String() yields.
func (a *Allowlist) AllowedHostPort(hostport string) bool {
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil {
		// Fall back to a bare address; some listeners report one.
		addr, aerr := netip.ParseAddr(hostport)
		if aerr != nil {
			return false
		}
		return a.Allowed(addr)
	}
	return a.Allowed(ap.Addr())
}

// Len reports the number of configured prefixes, for logging and for the
// startup banner.
func (a *Allowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.prefixes)
}

// AllowAll reports whether the allowlist was explicitly opened to every peer.
func (a *Allowlist) AllowAll() bool { return a != nil && a.allowAll }

// String renders the configured prefixes for log lines.
func (a *Allowlist) String() string {
	if a == nil {
		return "<deny all>"
	}
	if a.allowAll {
		return "<allow all>"
	}
	if len(a.prefixes) == 0 {
		return "<deny all>"
	}
	parts := make([]string, 0, len(a.prefixes))
	for _, p := range a.prefixes {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ",")
}
