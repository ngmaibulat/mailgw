package queue

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// signTestKey is the RSA key internal/msgauth's own tests use. Shared rather
// than duplicated so a change to the fixture cannot leave the two suites
// verifying different things.
const signTestKey = "../msgauth/testdata/test-rsa.key"

type signStubDNS struct{ txt map[string][]string }

func (s signStubDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := s.txt[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (s signStubDNS) LookupMX(_ context.Context, n string) ([]*net.MX, error) {
	return nil, &net.DNSError{Err: "no such host", Name: n, IsNotFound: true}
}

func (s signStubDNS) LookupIPAddr(_ context.Context, h string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: h, IsNotFound: true}
}

func (s signStubDNS) LookupAddr(_ context.Context, a string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", Name: a, IsNotFound: true}
}

func loadSigningKey(t *testing.T) crypto.Signer {
	t.Helper()
	raw, err := os.ReadFile(signTestKey)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	k, err := msgauth.ParsePrivateKey(raw)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	return k
}

// publishKey builds the resolver a verifier would consult.
func publishKey(t *testing.T, selector, domain string, pub crypto.PublicKey) signStubDNS {
	t.Helper()
	var der []byte
	algo := "rsa"
	if ed, ok := pub.(ed25519.PublicKey); ok {
		algo, der = "ed25519", ed
	} else {
		var err error
		if der, err = x509.MarshalPKIXPublicKey(pub); err != nil {
			t.Fatal(err)
		}
	}
	return signStubDNS{txt: map[string][]string{
		selector + "._domainkey." + domain: {
			"v=DKIM1; k=" + algo + "; p=" + base64.StdEncoding.EncodeToString(der),
		},
	}}
}

// capturingRunner builds a runner whose Deliver records the EXACT bytes handed
// to the relay.
//
// That is the point of this file. A signature verified against the reader the
// signer was given proves nothing — it has to be verified against what actually
// went on the wire, which is the only place a missing prepended header would
// show up.
func capturingRunner(t *testing.T, signer *Signer, captured *string) (*Runner, *obs.Metrics) {
	t.Helper()

	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Outbound": {{Name: "a", Exchange: "127.0.0.1", Port: 25}},
	})
	if err != nil {
		t.Fatal(err)
	}

	m := obs.New()
	r := NewRunner(newSpool(t), RunnerConfig{
		Relays:       tbl,
		PollInterval: time.Hour,
		Concurrency:  1,
		PerGroup:     1,
		Signer:       signer,
		Metrics:      m,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Deliver: func(_ context.Context, relay relays.Relay, msg deliver.Message, _ deliver.Options) *deliver.Result {
			b, err := io.ReadAll(msg.Body)
			if err != nil {
				t.Errorf("read message body: %v", err)
			}
			*captured = string(b)
			return &deliver.Result{
				Relay: relay,
				Rcpts: []deliver.RcptResult{
					{Addr: msg.Rcpts[0], Outcome: deliver.OutcomeDelivered},
				},
			}
		},
	})
	return r, m
}

// signedEnvelope enqueues a message with a From header and an add_header-style
// prepend, then runs one attempt.
func signedEnvelope(t *testing.T, r *Runner, from string, prepend []Header) {
	t.Helper()

	const conn = "ABCDEF01-2345-6789-ABCD-EF0123456789"
	txn := conn + ".1"
	body, size, err := r.spool.WriteBody(txn,
		strings.NewReader("Received: from somewhere\r\nFrom: "+from+
			"\r\nSubject: signed\r\n\r\nthe body\r\n"))
	if err != nil {
		t.Fatalf("WriteBody: %v", err)
	}

	env := &Envelope{
		Version: EnvelopeVersion, UUID: txn + ".1", TxnUUID: txn, ConnUUID: conn,
		Body: body, BodySize: size, MailFrom: "bounces@relay.example",
		Rcpts:    []Recipient{{Addr: "you@example.net", Status: StatusPending}},
		RelayGrp: "Outbound", QueuedAt: time.Now().UnixMilli(),
		Prepend: prepend,
	}
	if err := r.spool.Enqueue(env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil || len(names) != 1 {
		t.Fatalf("ReadyAndNext: %v (%d names)", err, len(names))
	}
	e, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), e, inflight)
}

func signerFor(t *testing.T, domain string) *Signer {
	t.Helper()
	keys, err := msgauth.NewKeys([]msgauth.Key{{Domain: domain, Selector: "sel1", Path: signTestKey}})
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	return NewSigner(keys, "relaxed", "relaxed", nil, 0)
}

// TestSign_CoversTheBytesHandedToTheRelay is the assertion plans/M14 asks for by
// name, and the one that catches the ordering trap: signing must happen AFTER
// every header this gateway prepends, or the signature covers a message that
// never exists.
func TestSign_CoversTheBytesHandedToTheRelay(t *testing.T) {
	key := loadSigningKey(t)
	dns := publishKey(t, "sel1", "ngm.dev", key.Public())

	var wire string
	r, m := capturingRunner(t, signerFor(t, "ngm.dev"), &wire)
	signedEnvelope(t, r, "Alice <alice@ngm.dev>", []Header{
		{Name: "X-NGM-Tag", Value: "partner"},
		{Name: "Authentication-Results", Value: "gw.ngm.dev; spf=pass smtp.mailfrom=alice@ngm.dev"},
	})

	if !strings.HasPrefix(wire, "DKIM-Signature:") {
		t.Fatalf("the signature is not the first header on the wire:\n%s", wire)
	}
	// Both prepended headers are inside what went out...
	for _, want := range []string{"X-NGM-Tag: partner", "Authentication-Results: gw.ngm.dev"} {
		if !strings.Contains(wire, want) {
			t.Errorf("missing %q from the delivered message:\n%s", want, wire)
		}
	}
	// ...and the signature verifies over exactly those bytes.
	res := msgauth.VerifyDKIM(context.Background(), dns, strings.NewReader(wire), 10)
	if res.Value != msgauth.ResultPass {
		t.Fatalf("the delivered bytes do not verify: %s (%s)", res.Value, res.Reason)
	}
	if len(res.Domains) != 1 || res.Domains[0] != "ngm.dev" {
		t.Errorf("d= is %v, want [ngm.dev]", res.Domains)
	}

	if got := m.DKIMSigned.Load(); got != 1 {
		t.Errorf("dkim_signed = %d, want 1", got)
	}
	if got := m.DKIMSignFailed.Load(); got != 0 {
		t.Errorf("dkim_sign_failed = %d, want 0", got)
	}
}

// TestSign_PrependedHeadersAreCovered proves the previous test would fail for an
// implementation that signed the spooled body alone.
//
// It uses Reply-To rather than X-NGM-Tag deliberately, and the difference is
// worth stating: DKIM only covers the header fields named in h=, so an arbitrary
// X- header this gateway prepends is NOT protected by the signature and removing
// one downstream does not break it. What the ordering trap actually protects is
// a prepended header whose name IS in the signed set — where signing the spooled
// body first would produce a signature over a different value, or over no value
// where one is about to appear.
//
// The signed set is DefaultHeaderKeys, and go-msgauth lists every name in h=
// whether the message carries it or not, so a Reply-To injected after signing
// breaks the signature. That is the RFC 6376 §5.4.2 over-listing trick, and it
// is what makes this assertion possible at all.
func TestSign_PrependedHeadersAreCovered(t *testing.T) {
	key := loadSigningKey(t)
	dns := publishKey(t, "sel1", "ngm.dev", key.Public())

	var wire string
	r, _ := capturingRunner(t, signerFor(t, "ngm.dev"), &wire)
	signedEnvelope(t, r, "alice@ngm.dev", []Header{{Name: "Reply-To", Value: "desk@ngm.dev"}})

	if !strings.Contains(wire, "Reply-To: desk@ngm.dev") {
		t.Fatalf("the prepended header was never on the wire:\n%s", wire)
	}
	if res := msgauth.VerifyDKIM(context.Background(), dns, strings.NewReader(wire), 10); res.Value != msgauth.ResultPass {
		t.Fatalf("the delivered bytes do not verify: %s (%s)", res.Value, res.Reason)
	}

	// Remove it and the signature must break: it was inside what was signed.
	tampered := strings.Replace(wire, "Reply-To: desk@ngm.dev\r\n", "", 1)
	if res := msgauth.VerifyDKIM(context.Background(), dns, strings.NewReader(tampered), 10); res.Value == msgauth.ResultPass {
		t.Fatal("removing a prepended, signed header did not break the signature, " +
			"so the signature was computed before the prepend block")
	}
}

// TestSign_KeyIsChosenByTheFromHeader: d= must align with RFC5322.From for DMARC
// to credit the signature downstream. The envelope sender here is
// bounces@relay.example and there is no key for it — a signer keyed off the
// envelope would sign nothing.
func TestSign_KeyIsChosenByTheFromHeader(t *testing.T) {
	var wire string
	r, m := capturingRunner(t, signerFor(t, "ngm.dev"), &wire)
	signedEnvelope(t, r, "alice@NGM.dev", nil)

	if !strings.Contains(wire, "d=ngm.dev") {
		t.Errorf("d= does not follow the From header:\n%s", wire)
	}
	if got := m.DKIMSigned.Load(); got != 1 {
		t.Errorf("dkim_signed = %d, want 1", got)
	}
}

// TestSign_NoKeyIsSilent: a gateway relaying other people's mail signs the
// domains it is responsible for and leaves the rest alone. That is not a
// failure, so it must not move dkim_sign_failed — which exists to mean
// "signing was configured for this domain and did not work".
func TestSign_NoKeyIsSilent(t *testing.T) {
	var wire string
	r, m := capturingRunner(t, signerFor(t, "ngm.dev"), &wire)
	signedEnvelope(t, r, "someone@other.example", nil)

	if strings.Contains(wire, "DKIM-Signature:") {
		t.Errorf("signed a domain with no configured key:\n%s", wire)
	}
	if !strings.Contains(wire, "From: someone@other.example") {
		t.Errorf("the message was not delivered:\n%s", wire)
	}
	if got := m.DKIMSignFailed.Load(); got != 0 {
		t.Errorf("dkim_sign_failed = %d; not signing is not failing to sign", got)
	}
	if got := m.DKIMSigned.Load(); got != 0 {
		t.Errorf("dkim_signed = %d, want 0", got)
	}
}

// TestSign_NoFromHeaderIsSilent: a message with no From cannot be signed and
// cannot be evaluated for DMARC either, so it goes out unsigned rather than
// being held.
func TestSign_NoFromHeaderIsSilent(t *testing.T) {
	var wire string
	r, m := capturingRunner(t, signerFor(t, "ngm.dev"), &wire)

	const conn = "ABCDEF01-2345-6789-ABCD-EF0123456789"
	txn := conn + ".1"
	body, size, err := r.spool.WriteBody(txn, strings.NewReader("Subject: no from\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatalf("WriteBody: %v", err)
	}
	env := &Envelope{
		Version: EnvelopeVersion, UUID: txn + ".1", TxnUUID: txn, ConnUUID: conn,
		Body: body, BodySize: size, MailFrom: "me@ngm.dev",
		Rcpts:    []Recipient{{Addr: "you@example.net", Status: StatusPending}},
		RelayGrp: "Outbound", QueuedAt: time.Now().UnixMilli(),
	}
	if err := r.spool.Enqueue(env); err != nil {
		t.Fatal(err)
	}
	names, _, _, _ := r.spool.ReadyAndNext(time.Now())
	e, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatal(err)
	}
	r.attempt(context.Background(), e, inflight)

	if strings.Contains(wire, "DKIM-Signature:") {
		t.Errorf("signed a message with no From header:\n%s", wire)
	}
	if got := m.DKIMSignFailed.Load(); got != 0 {
		t.Errorf("dkim_sign_failed = %d; an unsignable message is not a signing failure", got)
	}
}

// TestSign_NilSignerChangesNothing is the regression floor: with signing off the
// bytes on the wire must be what they were before this code existed.
func TestSign_NilSignerChangesNothing(t *testing.T) {
	var wire string
	r, m := capturingRunner(t, nil, &wire)
	signedEnvelope(t, r, "alice@ngm.dev", []Header{{Name: "X-NGM-Tag", Value: "partner"}})

	if !strings.HasPrefix(wire, "X-NGM-Tag: partner\r\n") {
		t.Errorf("the prepend block is no longer first with signing off:\n%s", wire)
	}
	if strings.Contains(wire, "DKIM-Signature") {
		t.Errorf("a signature appeared with no signer:\n%s", wire)
	}
	if got := m.Snapshot()["dkim_signed"]; got != 0 {
		t.Errorf("dkim_signed = %d with signing off", got)
	}
}

// TestNewSigner_EmptyKeySetIsNil keeps the "nil means off" contract in one
// place, so a caller cannot end up with a signer that can never sign anything.
func TestNewSigner_EmptyKeySetIsNil(t *testing.T) {
	if s := NewSigner(nil, "relaxed", "relaxed", nil, 0); s != nil {
		t.Error("a nil key set should produce no signer")
	}
	empty, err := msgauth.NewKeys(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := NewSigner(empty, "relaxed", "relaxed", nil, 0); s != nil {
		t.Error("an empty key set should produce no signer")
	}
}
