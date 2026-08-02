package deliver

import (
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"strings"
	"testing"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/tlsx"
)

// startTLSRelay runs a fake relay that offers STARTTLS with a self-signed
// certificate — which is what a great many real smarthosts and mail exchangers
// present, and the case the old code silently downgraded to cleartext.
//
// The keypair comes from tlsx.EnsureSelfSigned, the same generator a managed
// gateway uses on first boot, rather than hand-rolled x509 in a test.
func startTLSRelay(t *testing.T, b *fakeBackend, name string) relays.Relay {
	t.Helper()

	certPath, keyPath, err := tlsx.EnsureSelfSigned(filepath.Join(t.TempDir(), "tls"), "relay.test")
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := smtp.NewServer(b)
	srv.Domain = "relay.test"
	srv.MaxMessageBytes = 1 << 20
	srv.AllowInsecureAuth = true
	srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}

	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	host, port, _ := net.SplitHostPort(l.Addr().String())
	p, _ := net.LookupPort("tcp", port)
	return relays.Relay{Name: name, Exchange: host, Port: relays.Port(p)}
}

// The headline case. An opportunistic relay with a self-signed certificate must
// deliver ENCRYPTED. It used to verify the certificate, fail, and fall through
// to a cleartext redial — so the ordinary case silently sent in the clear.
//
// RFC 7435: opportunistic security is encryption without authentication.
func TestDeliver_OpportunisticEncryptsAgainstASelfSignedRelay(t *testing.T) {
	b := &fakeBackend{}
	relay := startTLSRelay(t, b, "selfsigned")
	relay.TLS = relays.TLSOpportunistic

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), Options{})

	if res.Err != nil {
		t.Fatalf("delivery failed: %v", res.Err)
	}
	if !res.TLS {
		t.Error("the session must be encrypted — an unauthenticated TLS session " +
			"is strictly better than the cleartext fallback this used to take")
	}
	if res.TLSDowngraded {
		t.Errorf("no downgrade should be recorded, got reason %q", res.TLSDowngradeReason)
	}
	if got := b.messages(); len(got) != 1 {
		t.Fatalf("relay received %d messages, want 1", len(got))
	}
}

// The unset value means opportunistic (relays.TLSPolicy), so a relay that says
// nothing about TLS gets the same treatment.
func TestDeliver_UnsetPolicyBehavesAsOpportunistic(t *testing.T) {
	b := &fakeBackend{}
	relay := startTLSRelay(t, b, "default")
	relay.TLS = ""

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), Options{})

	if res.Err != nil {
		t.Fatalf("delivery failed: %v", res.Err)
	}
	if !res.TLS {
		t.Error("an unset TLS policy is opportunistic and should still encrypt")
	}
}

// `required` is the policy an operator sets deliberately for a relay whose
// certificate they trust, so it must verify — and refuse rather than fall back.
func TestDeliver_RequiredVerifiesAndRefusesASelfSignedRelay(t *testing.T) {
	b := &fakeBackend{}
	relay := startTLSRelay(t, b, "selfsigned")
	relay.TLS = relays.TLSRequired

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), Options{})

	if res.Err == nil {
		t.Fatal("tls: required must not accept a certificate it cannot verify")
	}
	if !strings.Contains(res.Err.Error(), "TLS") {
		t.Errorf("error should mention TLS, got %v", res.Err)
	}
	if res.TLS {
		t.Error("the session must not be reported as encrypted")
	}
	if res.TLSDowngraded {
		t.Error("a required relay never downgrades — it fails")
	}
	if got := b.messages(); len(got) != 0 {
		t.Error("nothing may be transmitted when the TLS policy cannot be met")
	}
}

// A relay that does not offer STARTTLS at all is where the downgrade is real,
// and it must now be visible rather than silent.
func TestDeliver_OpportunisticDowngradeIsRecorded(t *testing.T) {
	b := &fakeBackend{}
	relay := startRelay(t, b, "plaintext") // no TLSConfig, so no STARTTLS offered
	relay.TLS = relays.TLSOpportunistic

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), Options{})

	if res.Err != nil {
		t.Fatalf("opportunistic must still deliver to a plaintext relay: %v", res.Err)
	}
	if res.TLS {
		t.Error("the session was not encrypted")
	}
	if !res.TLSDowngraded {
		t.Fatal("the downgrade must be recorded — the silence was the defect")
	}
	if res.TLSDowngradeReason == "" {
		t.Error("the downgrade should carry a reason for the log line")
	}
	if got := b.messages(); len(got) != 1 {
		t.Fatalf("relay received %d messages, want 1", len(got))
	}
}

// tls: none neither upgrades nor counts as a downgrade — it is a deliberate
// choice, not a failure.
func TestDeliver_TLSNoneIsNotADowngrade(t *testing.T) {
	b := &fakeBackend{}
	relay := startRelay(t, b, "plaintext")

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), Options{})

	if res.Err != nil {
		t.Fatalf("delivery failed: %v", res.Err)
	}
	if res.TLSDowngraded {
		t.Error("tls: none is configured plaintext, not a failed upgrade")
	}
}

// MinVersion is pinned outbound, matching the inbound side and internal/central.
func TestTLSConfigFor_PinsTLS12AndSplitsVerificationByPolicy(t *testing.T) {
	base := relays.Relay{Name: "r", Exchange: "mx.partner.com", Port: 25}

	opportunistic := base
	opportunistic.TLS = relays.TLSOpportunistic
	if cfg := tlsConfigFor(opportunistic, Options{}); !cfg.InsecureSkipVerify {
		t.Error("opportunistic must not verify — the alternative it used to take was plaintext")
	} else if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}

	required := base
	required.TLS = relays.TLSRequired
	cfg := tlsConfigFor(required, Options{})
	if cfg.InsecureSkipVerify {
		t.Error("required must verify")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.ServerName != "mx.partner.com" {
		t.Errorf("ServerName = %q, want the relay's exchange", cfg.ServerName)
	}
}
