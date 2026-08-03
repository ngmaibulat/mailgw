package node

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// These tests exist because of M16's lesson, restated in the repo's own
// feedback: every package test in this milestone builds its subject directly,
// and three of the things M14 adds — the resolver, the signer, and the restart
// classification — only take effect through this package's wiring. A green
// internal/... suite says nothing about whether they are connected.

const msgAuthServer = `hostname: relay.example
listen:
  - addr: "127.0.0.1:0"
outbound:
  spool_dir: /tmp/does-not-matter
msgauth:
  spf:
    enabled: true
`

// TestRestartRequired_MsgAuth: the resolver and the loaded signing keys are
// built once at bring-up and captured by the Backend and the runner, so a
// deployed bundle that changes them MUST report a restart or it would be
// reported as applied while the process kept running on what it booted with.
func TestRestartRequired_MsgAuth(t *testing.T) {
	base := mutate(t, config.BundleOptions{}, withServer(msgAuthServer)).cfg

	cases := []struct {
		name string
		body string
		want []string
	}{
		{"identical", msgAuthServer, nil},
		{
			name: "turning a check on",
			body: msgAuthServer + "  dkim:\n    enabled: true\n",
			want: []string{"msgauth"},
		},
		{
			name: "changing the authserv-id",
			body: msgAuthServer + "  authserv_id: gw.relay.example\n",
			want: []string{"msgauth"},
		},
		{
			name: "changing the DNS timeout",
			body: msgAuthServer + "  dns_timeout: 9s\n",
			want: []string{"msgauth"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := mutate(t, config.BundleOptions{}, withServer(tc.body)).cfg
			if got := restartRequired(base, next); !slices.Equal(got, tc.want) {
				t.Errorf("restartRequired = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMsgAuthChecker_BuiltOnlyWhenSomethingAsks pins the gate that keeps the
// shipped default free: with nothing configured and no rule reading spf.*,
// dkim.* or dmarc.*, the session must get a nil checker and do no DNS at all.
func TestMsgAuthChecker_BuiltOnlyWhenSomethingAsks(t *testing.T) {
	cfgOff := &config.Config{Server: config.Server{Hostname: "relay.example"}}
	cfgOn := &config.Config{Server: config.Server{
		Hostname: "relay.example",
		MsgAuth: config.MsgAuthConfig{
			SPF: config.MsgAuthCheck{Enabled: true}, MaxDKIMSignatures: 10,
		},
	}}

	plain := compileForTest(t, `
version: 1
routes:
  - name: r
    match: {always: true}
    then: [{action: relay, relay: Outbound}]
`)
	spfAware := compileForTest(t, `
version: 1
routes:
  - name: r
    match: {field: spf.result, op: eq, value: pass}
    then: [{action: relay, relay: Outbound}]
`)

	if got := msgAuthChecker(cfgOff, plain); got != nil {
		t.Error("a checker was built for a configuration and a ruleset that ask for nothing")
	}
	// Either reason on its own is enough.
	if got := msgAuthChecker(cfgOn, plain); got == nil {
		t.Error("the configuration asked for SPF and got no checker")
	}
	if got := msgAuthChecker(cfgOff, spfAware); got == nil {
		t.Error("a rule reading spf.* did not turn the check on")
	}

	// The authserv-id falls back to the hostname, and the checker carries the
	// configured bounds rather than re-reading them per message.
	m := msgAuthChecker(cfgOn, plain)
	if m.AuthservID != "relay.example" {
		t.Errorf("authserv_id = %q, want the hostname", m.AuthservID)
	}
	if m.MaxDKIMSignatures != 10 {
		t.Errorf("max_dkim_signatures = %d, want 10", m.MaxDKIMSignatures)
	}
	if m.Resolver == nil {
		t.Error("the checker has no resolver, so every check would panic")
	}
}

// TestDKIMSigner_Wiring: signing off must produce a nil signer (the runner's
// "off" contract), and signing on must produce one loaded from the key path the
// configuration names.
func TestDKIMSigner_Wiring(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "ngm.key")
	raw, err := os.ReadFile("../../internal/msgauth/testdata/test-rsa.key")
	if err != nil {
		t.Fatalf("read fixture key: %v", err)
	}
	if err := os.WriteFile(key, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := func(enabled bool, path string) *config.Config {
		return &config.Config{Server: config.Server{
			Hostname: "relay.example",
			MsgAuth: config.MsgAuthConfig{Sign: config.DKIMSignConfig{
				Enabled:          enabled,
				Canonicalization: "relaxed/relaxed",
				Keys: []config.DKIMKey{
					{Domain: "ngm.dev", Selector: "mail", Key: path},
				},
			}},
		}}
	}

	s, err := dkimSigner(cfg(false, key))
	if err != nil || s != nil {
		t.Errorf("signing off should produce no signer: %v / %v", s, err)
	}

	s, err = dkimSigner(cfg(true, key))
	if err != nil {
		t.Fatalf("dkimSigner: %v", err)
	}
	if s == nil {
		t.Fatal("signing on produced no signer")
	}

	// A key that cannot be read is fatal at bring-up rather than "carry on
	// unsigned": an operator is watching, and mail from a DMARC domain going out
	// unsigned is refused at the far end while every log line here says
	// delivered.
	if _, err := dkimSigner(cfg(true, filepath.Join(dir, "nope"))); err == nil {
		t.Error("an unreadable key was accepted at bring-up")
	}
}

// TestReportMsgAuth_NamesTheChecksARuleTurnedOn: the surprise this line exists
// to prevent is an operator writing a rule on spf.result and not realising every
// message now costs a DNS walk.
func compileForTest(t *testing.T, y string) *ruleset.Ruleset {
	t.Helper()
	p := filepath.Join(t.TempDir(), "routing.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := ruleset.LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Outbound": {{Name: "a", Exchange: "127.0.0.1", Port: 25}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := ruleset.Compile(f, tbl, ruleset.DefaultSchema())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return rs
}
