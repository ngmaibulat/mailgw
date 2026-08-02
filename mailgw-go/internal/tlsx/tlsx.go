// Package tlsx supplies the certificate the SMTP listener serves.
//
// Two jobs, both small and both stdlib-only:
//
//   - EnsureSelfSigned generates a keypair on first boot when none is
//     configured. A centrally-managed node is zero-configuration by design — no
//     environment, no arguments, no files — so there is nowhere for an operator
//     to put a certificate before the node exists. Generating one locally is the
//     same trade the Ed25519 identity already makes, and opportunistic STARTTLS
//     with a self-signed certificate is what almost every sending MTA in the
//     world will accept. It is not a substitute for a real certificate on a node
//     whose peers verify names; it is a substitute for cleartext.
//
//   - Loader watches the files and reloads them when they change, so a renewed
//     certificate does not need a process restart. restartRequired compares cert
//     PATHS, not contents, and NewServer used to load the keypair exactly once —
//     which meant a 90-day certificate rewritten in place kept serving the
//     expired one until somebody noticed.
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// File names used for the generated pair. Stable, because an operator who
// replaces the generated certificate with a real one will point at these paths.
const (
	SelfSignedCert = "self-signed.crt"
	SelfSignedKey  = "self-signed.key"
)

// selfSignedLifetime is long deliberately. This certificate is not verified by
// anybody — it exists so the session is encrypted — and an expiry that silently
// stopped inbound TLS on an unattended relay would be a worse outcome than a
// long-lived key on a host that already holds the gateway's identity key.
const selfSignedLifetime = 10 * 365 * 24 * time.Hour

// EnsureSelfSigned returns the paths of a self-signed keypair for hostname,
// generating it into dir the first time and reusing it thereafter.
//
// Reuse matters: a node that minted a fresh certificate on every restart would
// change its fingerprint under any peer that pinned or cached it, and would fill
// the data directory.
func EnsureSelfSigned(dir, hostname string) (certPath, keyPath string, err error) {
	certPath = filepath.Join(dir, SelfSignedCert)
	keyPath = filepath.Join(dir, SelfSignedKey)

	if fileExists(certPath) && fileExists(keyPath) {
		// Verified rather than assumed: a half-written pair from a crash during
		// the first boot would otherwise fail at listener bind, which is much
		// further from the cause.
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return certPath, keyPath, nil
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("tls dir %s: %w", dir, err)
	}

	certPEM, keyPEM, err := generate(hostname)
	if err != nil {
		return "", "", err
	}
	// The key first and 0600: a certificate with no key is useless, a key with
	// loose permissions is a finding.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", "", fmt.Errorf("write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", certPath, err)
	}
	return certPath, keyPath, nil
}

func generate(hostname string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hostname, Organization: []string{"mailgw-go"}},
		NotBefore:             now.Add(-time.Hour), // clock skew on a fresh VM
		NotAfter:              now.Add(selfSignedLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	// server.hostname is what the banner and the Received header say, so it is
	// the name a peer checking anything would check against.
	if ip := net.ParseIP(hostname); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else if hostname != "" {
		tmpl.DNSNames = []string{hostname}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// Reloader serves a keypair to crypto/tls and picks up changes to the files
// underneath it.
type Reloader struct {
	certPath, keyPath string

	mu      sync.Mutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

// NewReloader loads the pair once so a bad path or an unreadable key fails at
// bring-up, where the operator is watching, rather than on the first inbound
// connection that tries to negotiate TLS.
func NewReloader(certPath, keyPath string) (*Reloader, error) {
	r := &Reloader{certPath: certPath, keyPath: keyPath}
	if _, err := r.GetCertificate(nil); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate satisfies tls.Config.GetCertificate.
//
// It stats both files on every handshake, which is two syscalls against a page
// the kernel has cached, and reloads only when a timestamp moved. Cheaper than
// the alternative — a background watcher with its own lifecycle — and the
// failure mode is better: a certificate replaced by something unreadable keeps
// the previous one serving rather than taking the listener down.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	certMod, certErr := modTime(r.certPath)
	keyMod, keyErr := modTime(r.keyPath)
	unchanged := certErr == nil && keyErr == nil &&
		certMod.Equal(r.certMod) && keyMod.Equal(r.keyMod)

	if r.cert != nil && unchanged {
		return r.cert, nil
	}
	if certErr != nil || keyErr != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		if certErr != nil {
			return nil, fmt.Errorf("tls cert %s: %w", r.certPath, certErr)
		}
		return nil, fmt.Errorf("tls key %s: %w", r.keyPath, keyErr)
	}

	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		if r.cert != nil {
			// Mid-rewrite, most likely: the cert file has landed and the key has
			// not. Keep serving what worked and try again next handshake.
			return r.cert, nil
		}
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}

	r.cert, r.certMod, r.keyMod = &cert, certMod, keyMod
	return r.cert, nil
}

func modTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
