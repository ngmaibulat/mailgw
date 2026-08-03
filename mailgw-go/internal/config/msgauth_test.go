package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMsgAuth_DefaultsAreOff is the regression floor for the whole milestone.
//
// A configuration that says nothing about msgauth must behave exactly as it did
// before msgauth existed: no DNS, no second pass over the body, no header added
// and nothing stripped.
func TestMsgAuth_DefaultsAreOff(t *testing.T) {
	s, err := ParseServer([]byte("hostname: mx.ngm.dev\n"), FileServer)
	if err != nil {
		t.Fatalf("ParseServer: %v", err)
	}
	m := s.MsgAuth
	if m.Wants() {
		t.Error("msgauth is on in a configuration that never mentions it")
	}
	if m.SPF.Enabled || m.DKIM.Enabled || m.DMARC.Enabled || m.Sign.Enabled {
		t.Errorf("a switch defaults to on: %+v", m)
	}
	// The bounds are defaulted even though nothing reads them yet, so turning a
	// check on is a one-line change rather than three — the same treatment
	// outbound.max_messages_per_connection gets.
	if m.MaxDKIMSignatures != 10 {
		t.Errorf("max_dkim_signatures default: got %d, want 10", m.MaxDKIMSignatures)
	}
	if m.DNSTimeout.D() != 5*time.Second {
		t.Errorf("dns_timeout default: got %v, want 5s", m.DNSTimeout.D())
	}
	if m.Sign.Canonicalization != "relaxed/relaxed" {
		t.Errorf("canonicalization default: got %q", m.Sign.Canonicalization)
	}
}

// TestMsgAuth_AuthservIDFor pins the DSNConfig.PostmasterFor precedent: a
// default that depends on another configurable value is resolved where the value
// is known, not in defaults().
func TestMsgAuth_AuthservIDFor(t *testing.T) {
	if got := (MsgAuthConfig{}).AuthservIDFor("mx.ngm.dev"); got != "mx.ngm.dev" {
		t.Errorf("empty authserv_id: got %q, want the hostname", got)
	}
	if got := (MsgAuthConfig{AuthservID: " gw.ngm.dev "}).AuthservIDFor("mx.ngm.dev"); got != "gw.ngm.dev" {
		t.Errorf("explicit authserv_id: got %q", got)
	}
}

func TestMsgAuth_Validate(t *testing.T) {
	base := "hostname: mx.ngm.dev\n"

	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			// The house rule: ParseServer unmarshals over defaults(), so an
			// explicit 0 means "no bound" — which is the state the key exists to
			// end. It has to be an error, not a fallback to the default.
			name:    "an explicit zero dns_timeout is an error",
			yaml:    "msgauth:\n  spf: {enabled: true}\n  dns_timeout: 0\n",
			wantErr: "msgauth.dns_timeout must be positive",
		},
		{
			name:    "an explicit zero max_dkim_signatures is an error",
			yaml:    "msgauth:\n  dkim: {enabled: true}\n  max_dkim_signatures: 0\n",
			wantErr: "msgauth.max_dkim_signatures must be positive",
		},
		{
			name:    "dmarc alone would fail every message",
			yaml:    "msgauth:\n  dmarc: {enabled: true}\n",
			wantErr: "DMARC is alignment over their results",
		},
		{
			name:    "an authserv_id with a semicolon would break the header",
			yaml:    "msgauth:\n  spf: {enabled: true}\n  authserv_id: \"gw.ngm.dev; spf=pass\"\n",
			wantErr: "must be a single token",
		},
		{
			name:    "signing with no keys",
			yaml:    "msgauth:\n  sign: {enabled: true}\n",
			wantErr: "needs at least one msgauth.sign.keys entry",
		},
		{
			name: "an unknown canonicalization",
			yaml: "msgauth:\n  sign:\n    enabled: true\n    canonicalization: loose/loose\n" +
				"    keys: [{domain: ngm.dev, selector: mail, key: /tmp/k}]\n",
			wantErr: "canonicalization",
		},
		{
			// RFC 6376 section 5.4 requires it and go-msgauth refuses to build a
			// signer without it — caught at bring-up, so the failure is a
			// configuration error rather than every message going out unsigned.
			name: "a header list without From",
			yaml: "msgauth:\n  sign:\n    enabled: true\n    headers: [Subject, Date]\n" +
				"    keys: [{domain: ngm.dev, selector: mail, key: /tmp/k}]\n",
			wantErr: "must include From",
		},
		{
			name: "a negative expiration",
			yaml: "msgauth:\n  sign:\n    enabled: true\n    expiration: -1h\n" +
				"    keys: [{domain: ngm.dev, selector: mail, key: /tmp/k}]\n",
			wantErr: "cannot be negative",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseServer([]byte(base+c.yaml), FileServer)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got none", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}

func TestMsgAuth_ValidateAccepts(t *testing.T) {
	cases := map[string]string{
		"spf only":         "msgauth:\n  spf: {enabled: true}\n",
		"dkim only":        "msgauth:\n  dkim: {enabled: true}\n",
		"dmarc beside spf": "msgauth:\n  spf: {enabled: true}\n  dmarc: {enabled: true}\n",
		"a bare canonicalization applies to both": "msgauth:\n  sign:\n    enabled: true\n" +
			"    canonicalization: simple\n    keys: [{domain: ngm.dev, selector: mail, key: /tmp/k}]\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseServer([]byte("hostname: mx.ngm.dev\n"+y), FileServer); err != nil {
				t.Fatalf("ParseServer: %v", err)
			}
		})
	}
}

// TestValidateDKIM_ReadsTheKeyFiles is why this check is cross-file rather than
// part of Server.validate: a key the gateway cannot read is one whose mail goes
// out UNSIGNED, which a domain publishing DMARC sees as a failure at the far end
// while every log line here says "delivered". `check` has to find that.
func TestValidateDKIM_ReadsTheKeyFiles(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.key")
	copyFile(t, "../msgauth/testdata/test-rsa.key", good)
	junk := filepath.Join(dir, "junk.key")
	if err := os.WriteFile(junk, []byte("this is not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := func(keys ...DKIMKey) *Config {
		return &Config{Server: Server{
			Hostname: "mx.ngm.dev",
			MsgAuth:  MsgAuthConfig{Sign: DKIMSignConfig{Enabled: true, Keys: keys}},
		}}
	}

	if err := cfg(DKIMKey{Domain: "ngm.dev", Selector: "mail", Key: good}).validateDKIM(); err != nil {
		t.Errorf("a readable key was rejected: %v", err)
	}

	for name, c := range map[string]*Config{
		"missing file":     cfg(DKIMKey{Domain: "ngm.dev", Selector: "mail", Key: filepath.Join(dir, "nope")}),
		"unparseable file": cfg(DKIMKey{Domain: "ngm.dev", Selector: "mail", Key: junk}),
		"no selector":      cfg(DKIMKey{Domain: "ngm.dev", Key: good}),
		"no domain":        cfg(DKIMKey{Selector: "mail", Key: good}),
		"no path":          cfg(DKIMKey{Domain: "ngm.dev", Selector: "mail"}),
		"duplicate domain": cfg(
			DKIMKey{Domain: "ngm.dev", Selector: "a", Key: good},
			DKIMKey{Domain: "NGM.DEV", Selector: "b", Key: good},
		),
	} {
		t.Run(name, func(t *testing.T) {
			if err := c.validateDKIM(); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}

	// Signing off means the keys are not consulted at all, so an operator can
	// leave a stale entry behind while the feature is disabled.
	off := cfg(DKIMKey{Domain: "ngm.dev", Selector: "mail", Key: filepath.Join(dir, "nope")})
	off.Server.MsgAuth.Sign.Enabled = false
	if err := off.validateDKIM(); err != nil {
		t.Errorf("keys were validated with signing off: %v", err)
	}
}
