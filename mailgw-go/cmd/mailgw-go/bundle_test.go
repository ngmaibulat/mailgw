package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

const testdataDir = "../../testdata/config"

// bundleFromTestdata composes the bundle Central Management would deploy for
// the configuration directory the rest of the suite runs on.
func bundleFromTestdata(t *testing.T) []byte {
	t.Helper()

	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(testdataDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return raw
	}

	server := string(read(config.FileServer))
	routing := string(read(config.FileRouting))

	var byGroup map[string][]relays.Relay
	if err := json.Unmarshal(read(config.FileRelays), &byGroup); err != nil {
		t.Fatalf("parse %s: %v", config.FileRelays, err)
	}
	var logging config.Logging
	if err := json.Unmarshal(read(config.FileLogging), &logging); err != nil {
		t.Fatalf("parse %s: %v", config.FileLogging, err)
	}

	raw, err := json.Marshal(config.Bundle{
		Format:    config.BundleFormat,
		Server:    &server,
		Routing:   &routing,
		Allowlist: read(config.FileFilter),
		Relays:    byGroup,
		Logging:   logging,
	})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return raw
}

// TestLoadBundle_EqualsFileMode is the claim this milestone actually makes: a
// bundle pulled from the console and a directory on disk produce the same
// running configuration. If this fails, "check predicts a successful start" is
// no longer true for a managed gateway.
//
// The compiled *Ruleset is compared rule by rule rather than with DeepEqual:
// CompiledRule holds matcher closures, and two closures are never equal even
// when they came from identical source.
func TestLoadBundle_EqualsFileMode(t *testing.T) {
	fromDir, err := load(testdataDir)
	if err != nil {
		t.Fatalf("load(dir): %v", err)
	}
	fromBundle, err := loadBundle(bundleFromTestdata(t), config.BundleOptions{}, "bundle v1 (version_id 1)")
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}

	if !reflect.DeepEqual(fromBundle.cfg.Server, fromDir.cfg.Server) {
		t.Errorf("server config differs:\n  bundle %+v\n  dir    %+v", fromBundle.cfg.Server, fromDir.cfg.Server)
	}
	if fromBundle.cfg.Logging != fromDir.cfg.Logging {
		t.Errorf("logging differs: bundle %+v, dir %+v", fromBundle.cfg.Logging, fromDir.cfg.Logging)
	}
	if !fromBundle.cfg.Relays.Equal(fromDir.cfg.Relays) {
		t.Error("relay tables differ")
	}
	if fromBundle.cfg.Allowlist.String() != fromDir.cfg.Allowlist.String() {
		t.Errorf("allowlists differ: bundle %s, dir %s", fromBundle.cfg.Allowlist, fromDir.cfg.Allowlist)
	}

	// The rule file is pure data, so this one is safe to compare wholesale.
	if !reflect.DeepEqual(fromBundle.file, fromDir.file) {
		t.Error("parsed rule files differ")
	}

	if fromBundle.rules.Default != fromDir.rules.Default {
		t.Errorf("default action differs: bundle %v, dir %v", fromBundle.rules.Default, fromDir.rules.Default)
	}
	if !reflect.DeepEqual(fromBundle.rules.Groups(), fromDir.rules.Groups()) {
		t.Errorf("relay groups referenced differ: bundle %v, dir %v",
			fromBundle.rules.Groups(), fromDir.rules.Groups())
	}
	if !reflect.DeepEqual(fromBundle.rules.FieldsUsed(), fromDir.rules.FieldsUsed()) {
		t.Error("fields used differ")
	}
	compareRules(t, "policy", fromBundle.rules.Policy, fromDir.rules.Policy)
	compareRules(t, "routes", fromBundle.rules.Routes, fromDir.rules.Routes)
}

func compareRules(t *testing.T, what string, got, want []ruleset.CompiledRule) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rules from the bundle, %d from the directory", what, len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Name != w.Name || g.Priority != w.Priority || g.Stage != w.Stage ||
			g.Index != w.Index || g.RcptScoped != w.RcptScoped {
			t.Errorf("%s[%d]: %+v != %+v", what, i, g, w)
		}
		if !reflect.DeepEqual(g.Then, w.Then) {
			t.Errorf("%s[%d] %q: actions differ", what, i, w.Name)
		}
		if g.Describe() != w.Describe() {
			t.Errorf("%s[%d] %q: predicates differ:\n  bundle %s\n  dir    %s",
				what, i, w.Name, g.Describe(), w.Describe())
		}
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
