package msgauth

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

// ── the stub resolver ────────────────────────────────────────────────────────
//
// One stub serves SPF, DKIM and DMARC, because msgauth.Resolver is a single
// interface with *net.Resolver's method set. Nothing in this package's tests
// touches the network, which is what the milestone plan asks for: an SPF walk
// that resolved for real would be a test whose answer depends on somebody
// else's DNS.

type stubDNS struct {
	txt  map[string][]string
	mx   map[string][]*net.MX
	ip   map[string][]net.IPAddr
	ptr  map[string][]string
	fail map[string]error
}

func newStub() *stubDNS {
	return &stubDNS{
		txt:  map[string][]string{},
		mx:   map[string][]*net.MX{},
		ip:   map[string][]net.IPAddr{},
		ptr:  map[string][]string{},
		fail: map[string]error{},
	}
}

// notFound is what a real resolver returns for a name that does not exist, and
// both libraries branch on IsNotFound — a plain error would read as a temporary
// failure and turn every "none" in these tests into a "temperror".
func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (s *stubDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	name = strings.TrimSuffix(name, ".")
	if err, ok := s.fail[name]; ok {
		return nil, err
	}
	if v, ok := s.txt[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (s *stubDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	name = strings.TrimSuffix(name, ".")
	if v, ok := s.mx[name]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (s *stubDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	host = strings.TrimSuffix(host, ".")
	if v, ok := s.ip[host]; ok {
		return v, nil
	}
	return nil, notFound(host)
}

func (s *stubDNS) LookupAddr(_ context.Context, addr string) ([]string, error) {
	if v, ok := s.ptr[addr]; ok {
		return v, nil
	}
	return nil, notFound(addr)
}

func (s *stubDNS) withA(host string, ips ...string) *stubDNS {
	for _, ip := range ips {
		s.ip[host] = append(s.ip[host], net.IPAddr{IP: net.ParseIP(ip)})
	}
	return s
}

// dkimTXT publishes a public key the way a DKIM selector record does, so the
// round-trip test verifies against exactly the record shape a receiver reads.
func dkimTXT(t *testing.T, s *stubDNS, selector, domain string, pub crypto.PublicKey) {
	t.Helper()
	algo, der := "rsa", []byte(nil)
	if ed, ok := pub.(ed25519.PublicKey); ok {
		// RFC 8463 §3: for Ed25519 the p= tag is the RAW 32-byte key, not the
		// SubjectPublicKeyInfo an RSA record carries. Getting this wrong is a
		// permerror at every verifier, which is exactly what this fixture
		// exists to catch before a receiver does.
		algo, der = "ed25519", ed
	} else {
		var err error
		if der, err = x509.MarshalPKIXPublicKey(pub); err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
	}
	s.txt[selector+"._domainkey."+domain] = []string{
		"v=DKIM1; k=" + algo + "; p=" + base64.StdEncoding.EncodeToString(der),
	}
}

// ── fixtures ─────────────────────────────────────────────────────────────────

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// crlf renders a fixture the way it arrives over SMTP. Fixtures are stored
// LF-only so they stay diffable, and every DKIM case runs both ways — a line
// ending is exactly the difference that makes a signature stop verifying in
// production but not in a test.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func testKey(t *testing.T, name string) crypto.Signer {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	k, err := ParsePrivateKey(raw)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	return k
}

// ── ParsePrivateKey ──────────────────────────────────────────────────────────

func TestParsePrivateKey_AcceptsRSAAndEd25519(t *testing.T) {
	if _, ok := testKey(t, "test-rsa.key").(*rsa.PrivateKey); !ok {
		t.Error("test-rsa.key did not parse as an RSA key")
	}
	if _, ok := testKey(t, "test-ed25519.key").(ed25519.PrivateKey); !ok {
		t.Error("test-ed25519.key did not parse as an Ed25519 key")
	}
}

func TestParsePrivateKey_RefusesJunk(t *testing.T) {
	cases := map[string]string{
		"not PEM at all":  "hello",
		"empty":           "",
		"unknown block":   "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"truncated bytes": "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePrivateKey([]byte(in)); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}

// ── Keys ─────────────────────────────────────────────────────────────────────

func TestKeys_ResolvesByExactDomain(t *testing.T) {
	k, err := NewKeys([]Key{{Domain: "NGM.dev", Selector: "mail", Path: "testdata/test-rsa.key"}})
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}

	sel, signer, ok := k.For("ngm.dev")
	if !ok || sel != "mail" || signer == nil {
		t.Fatalf("For(ngm.dev) = %q, %v, %v", sel, signer != nil, ok)
	}
	// The domain is normalised on the way in and on the way out, so a
	// configuration written in mixed case still matches a From header in lower.
	if _, _, ok := k.For("NGM.DEV."); !ok {
		t.Error("For should normalise case and a trailing dot")
	}
	// Deliberately NOT a subdomain match: the selector is published under the
	// signing domain, so a parent key signing a subdomain misattributes the mail.
	if _, _, ok := k.For("mail.ngm.dev"); ok {
		t.Error("a key for ngm.dev must not sign for mail.ngm.dev")
	}
}

func TestKeys_RefusesBadConfiguration(t *testing.T) {
	cases := map[string][]Key{
		"duplicate domain": {
			{Domain: "ngm.dev", Selector: "a", Path: "testdata/test-rsa.key"},
			{Domain: "ngm.dev", Selector: "b", Path: "testdata/test-ed25519.key"},
		},
		"missing selector": {{Domain: "ngm.dev", Path: "testdata/test-rsa.key"}},
		"missing domain":   {{Selector: "mail", Path: "testdata/test-rsa.key"}},
		"missing path":     {{Domain: "ngm.dev", Selector: "mail"}},
		"unreadable key":   {{Domain: "ngm.dev", Selector: "mail", Path: "testdata/nope.key"}},
	}
	for name, keys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewKeys(keys); err == nil {
				t.Fatal("want an error at bring-up, got none")
			}
		})
	}
}

// ── SigningFromDomain ────────────────────────────────────────────────────────

func TestSigningFromDomain(t *testing.T) {
	cases := []struct {
		name, msg, want string
		wantErr         bool
	}{
		{name: "simple", msg: "From: alice@ngm.dev\r\n\r\nbody\r\n", want: "ngm.dev"},
		{name: "display name", msg: "From: Alice <alice@NGM.dev>\r\n\r\nbody\r\n", want: "ngm.dev"},
		{
			name: "folded",
			msg:  "Subject: x\r\nFrom: Alice\r\n <alice@ngm.dev>\r\n\r\nbody\r\n",
			want: "ngm.dev",
		},
		{
			name: "several mailboxes takes the first",
			msg:  "From: a@one.example, b@two.example\r\n\r\nbody\r\n",
			want: "one.example",
		},
		{
			name: "malformed still yields the visible domain",
			msg:  "From: Alice (unclosed <alice@ngm.dev>\r\n\r\nbody\r\n",
			want: "ngm.dev",
		},
		{
			name:    "a From in the BODY is not a From header",
			msg:     "Subject: x\r\n\r\nFrom: alice@ngm.dev\r\n",
			wantErr: true,
		},
		{name: "no From at all", msg: "Subject: x\r\n\r\nbody\r\n", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SigningFromDomain(strings.NewReader(c.msg))
			if c.wantErr {
				if !errors.Is(err, ErrNoFrom) {
					t.Fatalf("want ErrNoFrom, got %q / %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SigningFromDomain: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ── DKIM sign/verify ─────────────────────────────────────────────────────────

// TestSignDKIM_RoundTrip signs a message and verifies it with go-msgauth's own
// verifier rather than with anything written here.
//
// That distinction is the point: a canonicalisation bug is self-consistent, so a
// signer checked against its own verifier passes while every real receiver
// rejects the mail.
func TestSignDKIM_RoundTrip(t *testing.T) {
	for _, keyFile := range []string{"test-rsa.key", "test-ed25519.key"} {
		for _, can := range []string{"relaxed/relaxed", "simple/simple", "relaxed/simple"} {
			t.Run(keyFile+" "+can, func(t *testing.T) {
				signer := testKey(t, keyFile)
				dns := newStub()
				dkimTXT(t, dns, "sel1", "ngm.dev", signer.Public())

				hdr, body, err := ParseCanonicalization(can)
				if err != nil {
					t.Fatalf("ParseCanonicalization: %v", err)
				}

				msg := crlf(load(t, "plain.eml"))
				sig, err := SignDKIM(strings.NewReader(msg), SignOptions{
					Domain: "ngm.dev", Selector: "sel1", Signer: signer,
					HeaderCan: hdr, BodyCan: body,
				})
				if err != nil {
					t.Fatalf("SignDKIM: %v", err)
				}
				if !strings.HasPrefix(sig, "DKIM-Signature:") || !strings.HasSuffix(sig, "\r\n") {
					t.Fatalf("signature is not a complete header field: %q", sig)
				}

				res := VerifyDKIM(context.Background(), dns, strings.NewReader(sig+msg), 10)
				if res.Value != ResultPass {
					t.Fatalf("verify = %s (%s); want pass", res.Value, res.Reason)
				}
				if len(res.Domains) != 1 || res.Domains[0] != "ngm.dev" {
					t.Errorf("domains = %v, want [ngm.dev]", res.Domains)
				}
			})
		}
	}
}

// TestSignDKIM_LFAndCRLFAgree is the highest-value assertion in this file.
//
// DKIM canonicalisation is a line-ending question, and a signer that quietly
// disagrees with itself about CRLF produces signatures that verify here and fail
// at every receiver. Signing an LF-only message and verifying the CRLF form of
// it is what a real MTA does to mail that arrived over a lax path.
func TestSignDKIM_LFAndCRLFAgree(t *testing.T) {
	signer := testKey(t, "test-rsa.key")
	dns := newStub()
	dkimTXT(t, dns, "sel1", "ngm.dev", signer.Public())

	lf := load(t, "plain.eml")
	sig, err := SignDKIM(strings.NewReader(lf), SignOptions{
		Domain: "ngm.dev", Selector: "sel1", Signer: signer,
	})
	if err != nil {
		t.Fatalf("SignDKIM: %v", err)
	}

	res := VerifyDKIM(context.Background(), dns, strings.NewReader(crlf(sig+lf)), 10)
	if res.Value != ResultPass {
		t.Fatalf("a signature made over LF input does not verify over CRLF: %s (%s)",
			res.Value, res.Reason)
	}
}

func TestSignDKIM_RefusesIncompleteOptions(t *testing.T) {
	signer := testKey(t, "test-rsa.key")
	cases := map[string]SignOptions{
		"no domain":   {Selector: "s", Signer: signer},
		"no selector": {Domain: "d", Signer: signer},
		"no key":      {Domain: "d", Selector: "s"},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := SignDKIM(strings.NewReader("From: a@b\r\n\r\n"), o); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}

// TestSignDKIM_CoversPrependedHeaders pins the ordering trap the milestone plan
// names: a signature has to be computed over the message INCLUDING every header
// this gateway adds, or it covers a message that never goes on the wire.
func TestSignDKIM_CoversPrependedHeaders(t *testing.T) {
	signer := testKey(t, "test-rsa.key")
	dns := newStub()
	dkimTXT(t, dns, "sel1", "ngm.dev", signer.Public())

	body := crlf(load(t, "plain.eml"))
	prepend := "X-NGM-Tag: partner\r\n"

	// Signed over the message as it will be sent.
	sig, err := SignDKIM(strings.NewReader(prepend+body), SignOptions{
		Domain: "ngm.dev", Selector: "sel1", Signer: signer,
		HeaderKeys: []string{"From", "Subject", "X-NGM-Tag"},
	})
	if err != nil {
		t.Fatalf("SignDKIM: %v", err)
	}
	if res := VerifyDKIM(context.Background(), dns, strings.NewReader(sig+prepend+body), 10); res.Value != ResultPass {
		t.Fatalf("signature over the sent bytes does not verify: %s (%s)", res.Value, res.Reason)
	}

	// The same signature against the body WITHOUT the prepended header must
	// fail — otherwise this test would pass even if signing ignored it.
	if res := VerifyDKIM(context.Background(), dns, strings.NewReader(sig+body), 10); res.Value == ResultPass {
		t.Fatal("signature verified without the prepended header, so it does not cover it")
	}
}

func TestVerifyDKIM_UnsignedIsNone(t *testing.T) {
	res := VerifyDKIM(context.Background(), newStub(), strings.NewReader(crlf(load(t, "plain.eml"))), 10)
	if res.Value != ResultNone {
		t.Errorf("got %s, want none", res.Value)
	}
	if len(res.Domains) != 0 {
		t.Errorf("domains = %v, want none", res.Domains)
	}
}

func TestVerifyDKIM_TamperedBodyFails(t *testing.T) {
	signer := testKey(t, "test-rsa.key")
	dns := newStub()
	dkimTXT(t, dns, "sel1", "ngm.dev", signer.Public())

	msg := crlf(load(t, "plain.eml"))
	sig, err := SignDKIM(strings.NewReader(msg), SignOptions{
		Domain: "ngm.dev", Selector: "sel1", Signer: signer,
	})
	if err != nil {
		t.Fatalf("SignDKIM: %v", err)
	}

	tampered := strings.Replace(msg, "The numbers are attached.", "Send money instead.", 1)
	res := VerifyDKIM(context.Background(), dns, strings.NewReader(sig+tampered), 10)
	if res.Value == ResultPass {
		t.Fatal("a modified body still verified")
	}
	if len(res.Domains) != 0 {
		t.Errorf("a failing signature contributed %v to Domains; only passing ones may", res.Domains)
	}
}

func TestVerifyDKIM_MissingKeyIsPermError(t *testing.T) {
	signer := testKey(t, "test-rsa.key")
	msg := crlf(load(t, "plain.eml"))
	sig, err := SignDKIM(strings.NewReader(msg), SignOptions{
		Domain: "ngm.dev", Selector: "sel1", Signer: signer,
	})
	if err != nil {
		t.Fatalf("SignDKIM: %v", err)
	}

	// Nothing published under the selector.
	res := VerifyDKIM(context.Background(), newStub(), strings.NewReader(sig+msg), 10)
	if res.Value != ResultPermError {
		t.Errorf("got %s, want permerror", res.Value)
	}
}

func TestParseCanonicalization(t *testing.T) {
	cases := []struct {
		in, hdr, body string
		wantErr       bool
	}{
		// Empty is relaxed/relaxed, NOT RFC 6376's simple/simple: simple breaks
		// on any whitespace change in transit, and every hop is entitled to one.
		{in: "", hdr: "relaxed", body: "relaxed"},
		{in: "relaxed/relaxed", hdr: "relaxed", body: "relaxed"},
		{in: "simple/simple", hdr: "simple", body: "simple"},
		{in: "relaxed/simple", hdr: "relaxed", body: "simple"},
		{in: "SIMPLE", hdr: "simple", body: "simple"},
		{in: "relaxed/nonsense", wantErr: true},
		{in: "nonsense", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			h, b, err := ParseCanonicalization(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatal("want an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCanonicalization: %v", err)
			}
			if h != c.hdr || b != c.body {
				t.Errorf("got %s/%s, want %s/%s", h, b, c.hdr, c.body)
			}
		})
	}
}
