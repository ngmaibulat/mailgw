package msgauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Key is one signing identity: which domain it speaks for, the selector its
// public half is published under, and where the private half lives.
//
// The path is here and the key material is not, and that is the whole point. A
// private key never travels in a configuration bundle — the console stores every
// version for ever and serves it to every gateway on the profile — so a bundle
// can name this file and nothing more. The consequence is stated plainly in
// plans/M14: a fleet cannot be given a signing key from the console, exactly as
// it cannot be given a real TLS certificate.
type Key struct {
	Domain   string
	Selector string
	Path     string
}

// Keys resolves a domain to the key that signs for it.
//
// Like tlsx.Reloader it stats the file and reloads when the timestamp moves, so
// a key rotated in place is picked up without a restart, and an unreadable
// replacement keeps the previous one signing rather than silently stopping.
// Unlike tlsx it generates nothing: a self-generated DKIM key whose public half
// is not in DNS produces signatures that FAIL verification, which is strictly
// worse than not signing at all.
type Keys struct {
	mu       sync.Mutex
	byDomain map[string]*loadedKey
}

type loadedKey struct {
	Key
	signer crypto.Signer
	mod    time.Time
}

// NewKeys loads every key once, so a bad path, a wrong permission or an
// unparseable file fails at bring-up where an operator is watching rather than
// on the first message that needed signing.
func NewKeys(keys []Key) (*Keys, error) {
	k := &Keys{byDomain: make(map[string]*loadedKey, len(keys))}
	for _, key := range keys {
		d := strings.ToLower(strings.TrimSpace(key.Domain))
		if d == "" || key.Selector == "" || key.Path == "" {
			return nil, fmt.Errorf("dkim key: domain, selector and key path are all required")
		}
		if _, dup := k.byDomain[d]; dup {
			// Two keys for one domain has no defensible meaning: whichever won
			// would depend on map order, and the operator would see signatures
			// from a selector they did not choose.
			return nil, fmt.Errorf("dkim key: duplicate domain %q", d)
		}
		key.Domain = d
		lk := &loadedKey{Key: key}
		if _, err := k.reload(lk); err != nil {
			return nil, err
		}
		k.byDomain[d] = lk
	}
	return k, nil
}

// Len reports how many domains can be signed for.
func (k *Keys) Len() int {
	if k == nil {
		return 0
	}
	return len(k.byDomain)
}

// Domains lists the domains this can sign for, for `check` output.
func (k *Keys) Domains() []Key {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Key, 0, len(k.byDomain))
	for _, lk := range k.byDomain {
		out = append(out, lk.Key)
	}
	return out
}

// For returns the signing identity for a domain, or ok=false when none is
// configured for it.
//
// Exact match only. A key for example.com does not sign for mail.example.com:
// the selector is published in DNS under the signing domain, so signing a
// subdomain's mail with the parent's key produces a d= the receiver looks up
// against the parent — which works only if the operator meant it, and silently
// misattributes the mail if they did not.
func (k *Keys) For(domain string) (selector string, signer crypto.Signer, ok bool) {
	if k == nil {
		return "", nil, false
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	lk, found := k.byDomain[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))]
	if !found {
		return "", nil, false
	}
	s, err := k.reload(lk)
	if err != nil {
		return "", nil, false
	}
	return lk.Selector, s, true
}

// reload re-reads a key when its file has changed. The caller holds k.mu,
// except during NewKeys where nothing else can see the map yet.
func (k *Keys) reload(lk *loadedKey) (crypto.Signer, error) {
	fi, err := os.Stat(lk.Path)
	if err != nil {
		if lk.signer != nil {
			// Mid-rotation, most likely. Keep signing with what worked.
			return lk.signer, nil
		}
		return nil, fmt.Errorf("dkim key %s: %w", lk.Path, err)
	}
	if lk.signer != nil && fi.ModTime().Equal(lk.mod) {
		return lk.signer, nil
	}

	raw, err := os.ReadFile(lk.Path)
	if err == nil {
		var s crypto.Signer
		if s, err = ParsePrivateKey(raw); err == nil {
			lk.signer, lk.mod = s, fi.ModTime()
			return s, nil
		}
	}
	if lk.signer != nil {
		return lk.signer, nil
	}
	return nil, fmt.Errorf("dkim key %s: %w", lk.Path, err)
}

// ParsePrivateKey reads a PEM-encoded RSA or Ed25519 private key.
//
// Those two are what RFC 6376 and RFC 8463 define for DKIM; an ECDSA key parses
// as a crypto.Signer and would be rejected by go-msgauth anyway, so it is
// refused here where the error can say why.
func ParsePrivateKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("not a PEM file")
	}

	var key any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block %q", block.Type)
	}
	if err != nil {
		return nil, err
	}

	switch k := key.(type) {
	case *rsa.PrivateKey:
		if bits := k.N.BitLen(); bits < 1024 {
			// RFC 8301 §3.2 makes 1024 the floor. Below it a verifier is
			// entitled to ignore the signature outright, so signing would cost
			// CPU for a header nobody credits.
			return nil, fmt.Errorf("RSA key is %d bits; RFC 8301 requires at least 1024", bits)
		}
		return k, nil
	case ed25519.PrivateKey:
		return k, nil
	case *ecdsa.PrivateKey:
		return nil, fmt.Errorf("ECDSA keys are not defined for DKIM; use RSA or Ed25519")
	default:
		return nil, fmt.Errorf("unsupported key type %T", key)
	}
}
