package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
)

// v2 builds a binary header: ver/cmd, fam/proto, and a payload.
func v2(verCmd, famProto byte, payload []byte) []byte {
	b := append([]byte{}, v2Sig[:]...)
	b = append(b, verCmd, famProto)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(payload)))
	b = append(b, l[:]...)
	return append(b, payload...)
}

// v2TCP4 is a well-formed PROXY/TCP4 payload.
func v2TCP4(src, dst [4]byte, sport, dport uint16) []byte {
	p := append([]byte{}, src[:]...)
	p = append(p, dst[:]...)
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], sport)
	binary.BigEndian.PutUint16(b[2:4], dport)
	return append(p, b[:]...)
}

func parseBytes(t *testing.T, raw []byte) (Header, *bufio.Reader, error) {
	t.Helper()
	br := bufio.NewReaderSize(bytes.NewReader(raw), hdrBufSize)
	h, err := Parse(br)
	return h, br, err
}

func TestParse_V1(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSrc string
		wantErr error
	}{
		{"tcp4", "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\r\n", "203.0.113.7:5000", nil},
		{"tcp6", "PROXY TCP6 2001:db8::1 2001:db8::2 5000 25\r\n", "[2001:db8::1]:5000", nil},
		{"unknown", "PROXY UNKNOWN\r\n", "", nil},
		// The specification requires a receiver to accept anything trailing
		// UNKNOWN.
		{"unknown with junk", "PROXY UNKNOWN and whatever else\r\n", "", nil},

		{"not a header", "EHLO mail.example.com\r\n", "", ErrNoHeader},
		{"too few fields", "PROXY TCP4 203.0.113.7 198.51.100.1 5000\r\n", "", ErrMalformed},
		{"too many fields", "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25 9\r\n", "", ErrMalformed},
		{"unknown protocol", "PROXY TCP5 203.0.113.7 198.51.100.1 5000 25\r\n", "", ErrMalformed},
		{"v6 literal in tcp4", "PROXY TCP4 2001:db8::1 2001:db8::2 5000 25\r\n", "", ErrMalformed},
		{"v4 literal in tcp6", "PROXY TCP6 203.0.113.7 198.51.100.1 5000 25\r\n", "", ErrMalformed},
		{"bad source", "PROXY TCP4 not-an-ip 198.51.100.1 5000 25\r\n", "", ErrMalformed},
		{"port out of range", "PROXY TCP4 203.0.113.7 198.51.100.1 70000 25\r\n", "", ErrMalformed},
		{"negative port", "PROXY TCP4 203.0.113.7 198.51.100.1 -1 25\r\n", "", ErrMalformed},
		{"non-numeric port", "PROXY TCP4 203.0.113.7 198.51.100.1 smtp 25\r\n", "", ErrMalformed},
		{"double space", "PROXY  TCP4 203.0.113.7 198.51.100.1 5000 25\r\n", "", ErrMalformed},
		{"lf only", "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\n", "", ErrMalformed},
		{"over long", "PROXY TCP4 " + strings.Repeat("2", 200) + " 1 2 3\r\n", "", ErrMalformed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, err := parseBytes(t, []byte(tc.in))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantSrc == "" {
				if h.Proxied() {
					t.Errorf("Proxied() = true, want false (no proxied address)")
				}
				return
			}
			if got := h.Src.String(); got != tc.wantSrc {
				t.Errorf("Src = %s, want %s", got, tc.wantSrc)
			}
		})
	}
}

func TestParse_V1LeavesTheStreamPositioned(t *testing.T) {
	raw := "PROXY TCP4 203.0.113.7 198.51.100.1 5000 25\r\nEHLO after\r\n"
	_, br, err := parseBytes(t, []byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rest, _ := io.ReadAll(br)
	if string(rest) != "EHLO after\r\n" {
		t.Errorf("stream left at %q, want the bytes after the header", rest)
	}
}

func TestParse_V2(t *testing.T) {
	good := v2TCP4([4]byte{203, 0, 113, 7}, [4]byte{198, 51, 100, 1}, 5000, 25)

	t.Run("tcp4", func(t *testing.T) {
		h, _, err := parseBytes(t, v2(0x21, 0x11, good))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := h.Src.String(); got != "203.0.113.7:5000" {
			t.Errorf("Src = %s", got)
		}
		if got := h.Dst.String(); got != "198.51.100.1:25" {
			t.Errorf("Dst = %s", got)
		}
	})

	t.Run("tcp6", func(t *testing.T) {
		src := netip.MustParseAddr("2001:db8::1").As16()
		dst := netip.MustParseAddr("2001:db8::2").As16()
		p := append(append([]byte{}, src[:]...), dst[:]...)
		var b [4]byte
		binary.BigEndian.PutUint16(b[0:2], 5000)
		binary.BigEndian.PutUint16(b[2:4], 25)
		p = append(p, b[:]...)

		h, _, err := parseBytes(t, v2(0x21, 0x21, p))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := h.Src.String(); got != "[2001:db8::1]:5000" {
			t.Errorf("Src = %s", got)
		}
	})

	// TLVs follow the address block and must be skipped, not misread.
	t.Run("with TLVs", func(t *testing.T) {
		payload := append(append([]byte{}, good...), 0x03, 0x00, 0x04, 1, 2, 3, 4)
		h, br, err := parseBytes(t, append(v2(0x21, 0x11, payload), []byte("EHLO after\r\n")...))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := h.Src.String(); got != "203.0.113.7:5000" {
			t.Errorf("Src = %s", got)
		}
		rest, _ := io.ReadAll(br)
		if string(rest) != "EHLO after\r\n" {
			t.Errorf("TLVs were not skipped; stream left at %q", rest)
		}
	})

	// Everything that means "no proxied client". The real peer must stand.
	t.Run("no proxied address", func(t *testing.T) {
		cases := map[string][]byte{
			"local, empty":     v2(0x20, 0x00, nil),
			"local, with junk": v2(0x20, 0x11, good),
			"unspec":           v2(0x21, 0x00, nil),
			"af_unix":          v2(0x21, 0x31, make([]byte, 216)),
		}
		for name, raw := range cases {
			t.Run(name, func(t *testing.T) {
				h, _, err := parseBytes(t, raw)
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if h.Proxied() {
					t.Error("Proxied() = true, want false")
				}
			})
		}
	})

	t.Run("rejected", func(t *testing.T) {
		cases := map[string][]byte{
			"bad version":       v2(0x31, 0x11, good),
			"bad command":       v2(0x25, 0x11, good),
			"inet over dgram":   v2(0x21, 0x12, good),
			"inet6 over dgram":  v2(0x21, 0x22, good),
			"length too short":  v2(0x21, 0x11, good[:8]),
			"header truncated":  v2Sig[:],
			"payload truncated": append(v2(0x21, 0x11, good)[:16], good[:4]...),
		}
		for name, raw := range cases {
			t.Run(name, func(t *testing.T) {
				if _, _, err := parseBytes(t, raw); err == nil {
					t.Fatal("expected an error")
				}
			})
		}
	})
}

// A peer that connects and says nothing must not be read as a valid header.
func TestParse_EmptyStream(t *testing.T) {
	if _, _, err := parseBytes(t, nil); err == nil {
		t.Fatal("expected an error on an empty stream")
	}
}
