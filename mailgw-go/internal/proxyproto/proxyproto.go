// Package proxyproto implements the HAProxy PROXY protocol, v1 text and v2
// binary, in front of the inbound SMTP listener.
//
// It exists because the IP allowlist is the only inbound gate this gateway has
// (internal/config/allowlist.go). Behind an L4 load balancer or a TLS
// terminator every connection arrives from one address, so allowlisting that
// address opens the relay to everything behind it and every conn.* rule matches
// on the balancer rather than the client.
//
// Hand-rolled and stdlib-only, matching how this module has treated every
// comparable choice — internal/obs over prometheus/client_golang, ruleset/glob.go
// over gobwas/glob, internal/store's own migrations.
package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// Errors a connection can be dropped for. All four take the same action —
// close, silently, because the PROXY protocol has no error response — so the
// taxonomy is for the log line and the tests, not for control flow.
var (
	// ErrUntrusted means the peer is not in proxy_trusted. Nothing was read from
	// it: the check happens before the first byte, so a hostile peer costs one
	// accept and one close.
	ErrUntrusted = errors.New("proxyproto: peer is not in proxy_trusted")
	// ErrNoHeader means the connection did not begin with a PROXY header at all
	// — most often a client talking SMTP straight to a listener that expects a
	// balancer in front of it.
	ErrNoHeader = errors.New("proxyproto: connection does not begin with a PROXY header")
	// ErrMalformed means it began with one and the header is not valid. The
	// wrapped reason is the useful part.
	ErrMalformed = errors.New("proxyproto: malformed PROXY header")
)

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w (%s)", ErrMalformed, fmt.Sprintf(format, args...))
}

// v2Sig is the 12-byte signature that starts every v2 header.
var v2Sig = [12]byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

// v1Prefix is what a v1 header starts with.
var v1Prefix = []byte("PROXY ")

// v1 headers are capped by the specification at 107 bytes including CRLF.
const v1MaxLen = 107

// v2 command and address-family codes.
const (
	cmdLocal = 0x0
	cmdProxy = 0x1

	famUnspec = 0x0
	famINET   = 0x1
	famINET6  = 0x2

	protoStream = 0x1
)

// Header is what one PROXY header claims.
//
// The zero value means "this connection carries no proxied address" — a v2 LOCAL
// command, a v1 UNKNOWN, or an address family this gateway cannot express as a
// TCP address — and the caller must keep the real peer address.
type Header struct {
	Src netip.AddrPort // the client, as the proxy saw it
	Dst netip.AddrPort // the address the client connected to, on the proxy
}

// Proxied reports whether the header named a client.
func (h Header) Proxied() bool { return h.Src.IsValid() }

// Parse reads exactly one PROXY header from br and leaves br positioned on the
// first byte after it. br must be at the very first byte of the connection.
func Parse(br *bufio.Reader) (Header, error) {
	// The shortest legal v1 header, "PROXY UNKNOWN\r\n", is 15 bytes, so
	// demanding 12 for the signature check can never over-read a valid header.
	// A peer that sends fewer and then stops hits the read deadline, which is
	// the fail-closed direction.
	sig, err := br.Peek(len(v2Sig))
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Header{}, ErrNoHeader
		}
		return Header{}, err
	}
	if bytes.Equal(sig, v2Sig[:]) {
		return parseV2(br)
	}
	if !bytes.HasPrefix(sig, v1Prefix) {
		return Header{}, ErrNoHeader
	}
	return parseV1(br)
}

// parseV1 reads the text form: "PROXY TCP4 <src> <dst> <sport> <dport>\r\n".
func parseV1(br *bufio.Reader) (Header, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			// The buffer is larger than the 107-byte maximum, so anything that
			// overflows it is already illegal.
			return Header{}, malformed("v1 header longer than %d bytes", v1MaxLen)
		}
		if errors.Is(err, io.EOF) {
			return Header{}, malformed("v1 header not terminated")
		}
		return Header{}, err
	}
	if len(line) > v1MaxLen {
		return Header{}, malformed("v1 header is %d bytes, maximum is %d", len(line), v1MaxLen)
	}
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return Header{}, malformed("v1 header not CRLF-terminated")
	}

	// Split on single spaces rather than strings.Fields: the specification
	// mandates exactly one, and Fields would collapse runs and silently accept a
	// nonconforming sender.
	f := strings.Split(string(line[:len(line)-2]), " ")
	if len(f) < 2 || f[0] != "PROXY" {
		return Header{}, ErrNoHeader
	}

	switch f[1] {
	case "UNKNOWN":
		// The specification permits a receiver to see anything after UNKNOWN and
		// requires it be accepted. There is no proxied address.
		return Header{}, nil
	case "TCP4", "TCP6":
	default:
		return Header{}, malformed("unknown v1 protocol %q", f[1])
	}

	if len(f) != 6 {
		return Header{}, malformed("v1 %s header has %d fields, want 6", f[1], len(f))
	}

	src, err := netip.ParseAddr(f[2])
	if err != nil {
		return Header{}, malformed("bad v1 source address %q", f[2])
	}
	dst, err := netip.ParseAddr(f[3])
	if err != nil {
		return Header{}, malformed("bad v1 destination address %q", f[3])
	}
	// Check the addresses against the family the header declared, so
	// "PROXY TCP4 ::1 ..." is refused rather than quietly reinterpreted.
	if want4 := f[1] == "TCP4"; want4 != (src.Is4() && dst.Is4()) {
		return Header{}, malformed("v1 %s header carries addresses of another family", f[1])
	}

	sport, err := parsePort(f[4])
	if err != nil {
		return Header{}, malformed("bad v1 source port %q", f[4])
	}
	dport, err := parsePort(f[5])
	if err != nil {
		return Header{}, malformed("bad v1 destination port %q", f[5])
	}

	return Header{
		Src: netip.AddrPortFrom(src, sport),
		Dst: netip.AddrPortFrom(dst, dport),
	}, nil
}

func parsePort(s string) (uint16, error) {
	// ParseUint with a 16-bit size rejects both a negative and an out-of-range
	// port, and its base-10 parse rejects "0x10" and leading '+'.
	p, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(p), nil
}

// parseV2 reads the binary form.
func parseV2(br *bufio.Reader) (Header, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return Header{}, malformed("v2 header truncated: %v", err)
	}

	if v := hdr[12] >> 4; v != 2 {
		return Header{}, malformed("v2 version is %d", v)
	}
	cmd := hdr[12] & 0x0f
	fam := hdr[13] >> 4
	proto := hdr[13] & 0x0f
	n := int(binary.BigEndian.Uint16(hdr[14:16]))

	// How much of the address block is wanted. Everything else — LOCAL, UNSPEC,
	// AF_UNIX, an unknown family, and every TLV — is skipped by the same CopyN
	// below, so there is exactly one discard path.
	take := 0
	switch {
	case cmd == cmdLocal:
		// LOCAL means the connection is the proxy's own (a health check), so
		// there is no client to report and the real peer address stands.
	case cmd != cmdProxy:
		return Header{}, malformed("v2 command is %d", cmd)
	case fam == famINET && proto == protoStream:
		take = 12
	case fam == famINET6 && proto == protoStream:
		take = 36
	case fam == famINET || fam == famINET6:
		return Header{}, malformed("v2 TCP family %d with transport %d", fam, proto)
	case fam == famUnspec:
		// Explicitly "the proxy is not telling us", per the specification.
	}

	if take > n {
		return Header{}, malformed("v2 address block wants %d bytes, length is %d", take, n)
	}

	var h Header
	if take > 0 {
		var b [36]byte
		if _, err := io.ReadFull(br, b[:take]); err != nil {
			return Header{}, malformed("v2 address block truncated: %v", err)
		}
		h = decodeV2(fam, b[:take])
		n -= take
	}

	// The remainder of the address block plus every TLV, discarded without
	// allocating. n came from a uint16, so this is bounded at 64 KiB and runs
	// under the same read deadline as everything above.
	if n > 0 {
		if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
			return Header{}, malformed("v2 trailing data truncated: %v", err)
		}
	}
	return h, nil
}

func decodeV2(fam byte, b []byte) Header {
	if fam == famINET {
		src := netip.AddrFrom4([4]byte(b[0:4]))
		dst := netip.AddrFrom4([4]byte(b[4:8]))
		return Header{
			Src: netip.AddrPortFrom(src, binary.BigEndian.Uint16(b[8:10])),
			Dst: netip.AddrPortFrom(dst, binary.BigEndian.Uint16(b[10:12])),
		}
	}
	src := netip.AddrFrom16([16]byte(b[0:16]))
	dst := netip.AddrFrom16([16]byte(b[16:32]))
	return Header{
		Src: netip.AddrPortFrom(src, binary.BigEndian.Uint16(b[32:34])),
		Dst: netip.AddrPortFrom(dst, binary.BigEndian.Uint16(b[34:36])),
	}
}
