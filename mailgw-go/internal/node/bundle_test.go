package node

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// The fixture bundle every test in this package deploys.
//
// It used to be composed by reading testdata/config off disk, so that a test
// could assert a bundle and a directory produced the same running
// configuration. There is no directory any more — Central Management is the
// only configuration source — so the fixture is the wire format itself.
const (
	fixtureServer = `
hostname: devbook.local
local_domains: [devbook.local, ngm.dev]
listen:
    - addr: "0.0.0.0:2525"
max:
    bytes: 26214400
    recipients: 100
`

	fixtureRouting = `
version: 1
routes:
    - name: To Exchange
      match: {field: rcpt.domain, op: eq, value: ngm.dev}
      then: [{action: relay, relay: Exchange}]
    - name: Default
      match: {always: true}
      then: [{action: relay, relay: Outbound}]
`

	fixtureAllowlist = `{"allowed": ["127.0.0.1", "::1", "172.18.0.1"]}`

	fixtureRelays = `{
  "Exchange": [
    {"name":"Exch-01","auth_user":"someuser","auth_pass":"somepass","priority":0,
     "exchange":"sandbox.smtp.mailtrap.io","port":"2525"}
  ],
  "Outbound": [
    {"name":"Default","auth_user":"someuser","auth_pass":"somepass","priority":0,
     "exchange":"sandbox.smtp.mailtrap.io","port":"2525"}
  ]
}`
)

func bundleFromTestdata(t *testing.T) []byte {
	t.Helper()

	server, routing := fixtureServer, fixtureRouting

	var byGroup map[string][]relays.Relay
	if err := json.Unmarshal([]byte(fixtureRelays), &byGroup); err != nil {
		t.Fatalf("parse the relay fixture: %v", err)
	}

	raw, err := json.Marshal(config.Bundle{
		Format:    config.BundleFormat,
		Server:    &server,
		Routing:   &routing,
		Allowlist: []byte(fixtureAllowlist),
		Relays:    byGroup,
		Logging: config.Logging{
			URLConn:     "http://logservice:3000/api/connection",
			URLQueue:    "http://logservice:3000/api/queue",
			URLDelivery: "http://logservice:3000/api/delivery",
		},
	})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return raw
}

// The fixture must survive the real loader: this is the cmd-level half of what
// the CI `check -config` step used to assert about a sample directory.
func TestLoadBundle_FixtureCompiles(t *testing.T) {
	l, err := loadBundle(bundleFromTestdata(t), config.BundleOptions{}, "bundle v1 (version_id 1)")
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	if l.cfg.Server.Hostname != "devbook.local" {
		t.Errorf("hostname = %q", l.cfg.Server.Hostname)
	}
	if len(l.rules.Routes) != 2 {
		t.Fatalf("got %d route rules, want 2", len(l.rules.Routes))
	}
	if got := l.rules.Groups(); len(got) != 2 {
		t.Errorf("relay groups referenced = %v, want two", got)
	}
}

// A gateway with no ruleset profile assigned is legal: default_action answers
// every recipient. It must not be an apply failure, because that would make
// "deploy the allowlist first, rules second" impossible.
func TestLoadBundle_NoRulesetProfile(t *testing.T) {
	var b config.Bundle
	if err := json.Unmarshal(bundleFromTestdata(t), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b.Routing = nil
	raw, _ := json.Marshal(b)

	l, err := loadBundle(raw, config.BundleOptions{}, "bundle v1 (version_id 1)")
	if err != nil {
		t.Fatalf("a bundle with no ruleset profile must load: %v", err)
	}
	if len(l.rules.Routes) != 0 || len(l.rules.Policy) != 0 {
		t.Error("expected no rules")
	}
	if l.rules.Default.Kind != ruleset.ActTempfail {
		t.Errorf("default action = %v, want the 451 that Haraka's hook_get_mx gave", l.rules.Default)
	}
	// The operator has to be able to tell this apart from a deployed ruleset.
	if !strings.Contains(l.source, "no ruleset profile") {
		t.Errorf("source %q should say the ruleset is missing", l.source)
	}
}

// The gateway is authoritative for rule validation — the console does shape
// checks only. A ruleset that does not compile must be an error here, so it
// comes back as apply_error rather than as a gateway that silently drops mail.
func TestLoadBundle_RejectsUncompilableRules(t *testing.T) {
	cases := map[string]string{
		"unknown field":  "version: 1\nroutes:\n  - name: x\n    match: {field: mail.nosuchfield, op: eq, value: a}\n    then: [{action: relay, relay: Outbound}]\n",
		"unknown relay":  "version: 1\nroutes:\n  - name: x\n    match: {always: true}\n    then: [{action: relay, relay: NoSuchGroup}]\n",
		"unknown key":    "version: 1\nroutes:\n  - name: x\n    piority: 5\n    match: {always: true}\n    then: [{action: relay, relay: Outbound}]\n",
		"bad yaml types": "version: 1\nroutes:\n  - name: x\n    priority: high\n    match: {always: true}\n    then: [{action: relay, relay: Outbound}]\n",
	}

	for name, routing := range cases {
		var b config.Bundle
		if err := json.Unmarshal(bundleFromTestdata(t), &b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		b.Routing = &routing
		raw, _ := json.Marshal(b)

		if _, err := loadBundle(raw, config.BundleOptions{}, "bundle"); err == nil {
			t.Errorf("%s: expected the bundle to be refused", name)
		}
	}
}

// discardLogger keeps test output readable: these tests exercise paths that
// deliberately log errors.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
