package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureSelfSigned_GeneratesAUsablePair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")

	certPath, keyPath, err := EnsureSelfSigned(dir, "mx.example.com")
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("the generated pair does not load: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if leaf.Subject.CommonName != "mx.example.com" {
		t.Errorf("CN = %q, want the server hostname", leaf.Subject.CommonName)
	}
	if err := leaf.VerifyHostname("mx.example.com"); err != nil {
		t.Errorf("SAN does not cover the hostname: %v", err)
	}
	if leaf.NotAfter.Before(time.Now().Add(365 * 24 * time.Hour)) {
		t.Errorf("NotAfter = %s; a short-lived unattended certificate stops inbound TLS silently", leaf.NotAfter)
	}

	// The key sits next to the gateway's identity key on a root-owned volume;
	// it should not be world-readable.
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A node that minted a new certificate on every restart would change its
// fingerprint under anything that cached it, and would fill the data directory.
func TestEnsureSelfSigned_ReusesWhatItGenerated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")

	certPath, _, err := EnsureSelfSigned(dir, "mx.example.com")
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}
	first, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if _, _, err := EnsureSelfSigned(dir, "mx.example.com"); err != nil {
		t.Fatalf("second EnsureSelfSigned: %v", err)
	}
	second, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(first) != string(second) {
		t.Error("the certificate was regenerated on the second call")
	}
}

// A crash between writing the key and the certificate must not leave a node that
// cannot start.
func TestEnsureSelfSigned_ReplacesAnUnusablePair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{SelfSignedCert, SelfSignedKey} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("truncated"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	certPath, keyPath, err := EnsureSelfSigned(dir, "mx.example.com")
	if err != nil {
		t.Fatalf("EnsureSelfSigned: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("a corrupt pair was left in place: %v", err)
	}
}

func TestReloader_PicksUpARenewedCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	certPath, keyPath, err := EnsureSelfSigned(dir, "first.example.com")
	if err != nil {
		t.Fatal(err)
	}

	r, err := NewReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewReloader: %v", err)
	}
	before, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cn := leafOf(t, before).Subject.CommonName; cn != "first.example.com" {
		t.Fatalf("CN = %q", cn)
	}

	// Renewal in place, as certbot would do it.
	renewed := filepath.Join(t.TempDir(), "renewed")
	newCert, newKey, err := EnsureSelfSigned(renewed, "second.example.com")
	if err != nil {
		t.Fatal(err)
	}
	copyFile(t, newKey, keyPath)
	copyFile(t, newCert, certPath)
	bump(t, certPath)
	bump(t, keyPath)

	after, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cn := leafOf(t, after).Subject.CommonName; cn != "second.example.com" {
		t.Errorf("CN = %q after renewal; the reloader is still serving the old pair", cn)
	}
}

// A rewrite caught half-done, or a file briefly missing, must not take the
// listener down: the previous pair is still valid and still better than nothing.
func TestReloader_KeepsTheLastGoodPairOnABadRewrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	certPath, keyPath, err := EnsureSelfSigned(dir, "mx.example.com")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReloader(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(certPath, []byte("half a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	bump(t, certPath)

	got, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate returned an error rather than the last good pair: %v", err)
	}
	if cn := leafOf(t, got).Subject.CommonName; cn != "mx.example.com" {
		t.Errorf("CN = %q", cn)
	}

	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetCertificate(nil); err != nil {
		t.Errorf("a missing certificate file took TLS down: %v", err)
	}
}

func TestNewReloader_FailsAtBringUpOnABadPath(t *testing.T) {
	if _, err := NewReloader("/nope/cert.pem", "/nope/key.pem"); err == nil {
		t.Error("a missing keypair should fail where the operator is watching")
	}
}

func leafOf(t *testing.T, c *tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return leaf
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// bump makes the change visible to a mtime comparison regardless of the
// filesystem's timestamp resolution.
func bump(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}
