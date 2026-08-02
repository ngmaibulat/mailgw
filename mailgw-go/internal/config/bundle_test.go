package config

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// testdataBundle builds a bundle equivalent to testdata/config, the fixture the
// whole suite already trusts. Composing it here rather than committing a copy
// means the two cannot drift apart silently.
func testdataBundle(t *testing.T) Bundle {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "config")

	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return raw
	}

	server := string(read(FileServer))
	routing := string(read(FileRouting))

	var byGroup map[string][]relays.Relay
	if err := json.Unmarshal(read(FileRelays), &byGroup); err != nil {
		t.Fatalf("parse %s: %v", FileRelays, err)
	}
	var logging Logging
	if err := json.Unmarshal(read(FileLogging), &logging); err != nil {
		t.Fatalf("parse %s: %v", FileLogging, err)
	}

	return Bundle{
		Format:    BundleFormat,
		Server:    &server,
		Routing:   &routing,
		Allowlist: read(FileFilter),
		Relays:    byGroup,
		Logging:   logging,
	}
}

func marshal(t *testing.T, b Bundle) []byte {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return raw
}

// The claim this milestone makes is that a bundle and a directory produce the
// same configuration. This is the config half of it; the rule half is in
// cmd/mailgw-go/bundle_test.go.
func TestBundleConfig_MatchesDirectoryLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "config")

	fromDir, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	b, err := ParseBundle(marshal(t, testdataBundle(t)))
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	fromBundle, err := b.Config(BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle.Config: %v", err)
	}

	if fromBundle.Server.Hostname != fromDir.Server.Hostname {
		t.Errorf("hostname: bundle %q, dir %q", fromBundle.Server.Hostname, fromDir.Server.Hostname)
	}
	if fromBundle.Logging != fromDir.Logging {
		t.Errorf("logging: bundle %+v, dir %+v", fromBundle.Logging, fromDir.Logging)
	}
	if !fromBundle.Relays.Equal(fromDir.Relays) {
		t.Errorf("relays: bundle %v, dir %v", fromBundle.Relays.Names(), fromDir.Relays.Names())
	}
	if fromBundle.Allowlist.String() != fromDir.Allowlist.String() {
		t.Errorf("allowlist: bundle %s, dir %s", fromBundle.Allowlist, fromDir.Allowlist)
	}
	// Dir is the one field that must NOT match: a bundle did not come from a
	// directory and must not claim to have.
	if fromBundle.Dir != "" {
		t.Errorf("a bundle-sourced config should have no Dir, got %q", fromBundle.Dir)
	}
}

func TestParseBundle_RejectsUnknownFormat(t *testing.T) {
	b := testdataBundle(t)
	b.Format = 2

	_, err := ParseBundle(marshal(t, b))
	if err == nil {
		t.Fatal("an unknown bundle format must be refused, not guessed at")
	}
	// The message has to name both numbers or an operator cannot tell which
	// side needs upgrading.
	for _, want := range []string{"2", "1", "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q should mention %q", err, want)
		}
	}
}

func TestParseBundle_RejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", "[]", `{"format":1`} {
		if _, err := ParseBundle([]byte(raw)); err == nil {
			t.Errorf("%q: expected an error", raw)
		}
	}
}

// No server profile is a working configuration for everything but the relays —
// the same thing an absent server.yaml means on disk.
func TestBundleConfig_NoServerProfileUsesDefaults(t *testing.T) {
	b := testdataBundle(t)
	b.Server = nil

	cfg, err := parseAndConfig(t, b, BundleOptions{SpoolDir: "/var/lib/mailgw-go/queue"})
	if err != nil {
		t.Fatalf("Bundle.Config: %v", err)
	}
	if cfg.Server.Hostname != "localhost" {
		t.Errorf("hostname = %q, want the default", cfg.Server.Hostname)
	}
	if cfg.Server.Outbound.SpoolDir != "/var/lib/mailgw-go/queue" {
		t.Errorf("spool dir = %q, want the option to have replaced the compiled-in default",
			cfg.Server.Outbound.SpoolDir)
	}
}

// An operator who chose a spool directory always wins over the process default,
// or a deployed configuration would be quietly overridden.
func TestBundleConfig_ExplicitSpoolDirWins(t *testing.T) {
	server := "hostname: relay.example\noutbound:\n  spool_dir: /srv/spool\n"
	b := testdataBundle(t)
	b.Server = &server

	cfg, err := parseAndConfig(t, b, BundleOptions{SpoolDir: "/var/lib/mailgw-go/queue"})
	if err != nil {
		t.Fatalf("Bundle.Config: %v", err)
	}
	if cfg.Server.Outbound.SpoolDir != "/srv/spool" {
		t.Errorf("spool dir = %q, want the profile's choice", cfg.Server.Outbound.SpoolDir)
	}
}

// The central path parses the server profile strictly: it came from a console
// textarea, which is exactly where a silent typo does damage.
func TestBundleConfig_ServerProfileIsStrict(t *testing.T) {
	server := "hostnmae: relay.example\n"
	b := testdataBundle(t)
	b.Server = &server

	if _, err := parseAndConfig(t, b, BundleOptions{}); err == nil {
		t.Fatal("an unknown key in the server profile must be rejected")
	}
}

// A gateway with no allowlist profile gets {"allowed":[]} from the console.
// That must fail the apply — it is not a gateway that accepts nothing, it is a
// gateway that was not finished being configured.
func TestBundleConfig_EmptyAllowlistFailsClosed(t *testing.T) {
	b := testdataBundle(t)
	b.Allowlist = json.RawMessage(`{"allowed":[]}`)

	if _, err := parseAndConfig(t, b, BundleOptions{}); err == nil {
		t.Fatal("an empty allowlist must fail the bundle")
	}
}

// ...but an operator who deliberately opts in gets what they asked for. Without
// allow_all crossing the wire there is no way to express this from the console
// at all.
func TestBundleConfig_AllowAllCrossesTheWire(t *testing.T) {
	b := testdataBundle(t)
	b.Allowlist = json.RawMessage(`{"allowed":[],"allow_all":true}`)

	cfg, err := parseAndConfig(t, b, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle.Config: %v", err)
	}
	if !cfg.Allowlist.AllowAll() {
		t.Error("allow_all did not survive the bundle")
	}
	if !cfg.Allowlist.Allowed(netip.MustParseAddr("203.0.113.10")) {
		t.Error("allow_all should accept any peer")
	}
}

// A gateway with no relay group cannot deliver anything, so the whole bundle
// fails rather than applying rules that can only queue mail forever.
func TestBundleConfig_NoRelayGroupsIsAnError(t *testing.T) {
	b := testdataBundle(t)
	b.Relays = map[string][]relays.Relay{}

	_, err := parseAndConfig(t, b, BundleOptions{})
	if err == nil {
		t.Fatal("a bundle with no relay groups must be refused")
	}
	if !strings.Contains(err.Error(), "relay group") {
		t.Errorf("message should point at relay groups: %v", err)
	}
}

func TestRedactBundle_HidesSecrets(t *testing.T) {
	b := testdataBundle(t)
	b.Logging.APIKey = "super-secret"

	out, err := RedactBundle(marshal(t, b))
	if err != nil {
		t.Fatalf("RedactBundle: %v", err)
	}
	rendered := string(out)

	if strings.Contains(rendered, "super-secret") {
		t.Error("the logservice key survived redaction")
	}
	// The fixture's relays carry a plaintext auth_pass; whatever it is, it must
	// not appear.
	for _, members := range b.Relays {
		for _, m := range members {
			if m.AuthPass != "" && strings.Contains(rendered, m.AuthPass) {
				t.Errorf("relay password %q survived redaction", m.AuthPass)
			}
		}
	}
	if !strings.Contains(rendered, "[redacted]") {
		t.Error("expected redaction markers in the output")
	}
	// Redaction must not eat the rest of the document.
	if !strings.Contains(rendered, "url_delivery") {
		t.Error("redaction dropped unrelated keys")
	}
}

func parseAndConfig(t *testing.T, b Bundle, opts BundleOptions) (*Config, error) {
	t.Helper()
	parsed, err := ParseBundle(marshal(t, b))
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	return parsed.Config(opts)
}

// --- inbound AUTH credentials (M13) ---

// A bcrypt hash of nothing in particular. Fixed rather than generated so this
// file needs no dependency on golang.org/x/crypto.
const testAuthHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func TestBundleConfig_AuthCredentialsRoundTrip(t *testing.T) {
	b := testdataBundle(t)
	b.Auth = &Auth{Users: []AuthUser{{User: "app@ngm.dev", Hash: testAuthHash}}}

	cfg, err := parseAndConfig(t, b, BundleOptions{})
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	hash, ok := cfg.Auth.Lookup("app@ngm.dev")
	if !ok || hash != testAuthHash {
		t.Errorf("Lookup(app@ngm.dev) = %q, %v; want the deployed hash", hash, ok)
	}
	if !cfg.Auth.Enabled() {
		t.Error("Enabled() is false with a credential deployed")
	}
}

// The invariant that protects the fleet from a pointless redeploy: a bundle
// with no auth key must produce exactly the configuration it produced before
// the key existed, and must serialise without one.
func TestBundleConfig_NoAuthKeyIsUnchanged(t *testing.T) {
	b := testdataBundle(t)

	if raw := string(marshal(t, b)); strings.Contains(raw, `"auth"`) {
		t.Errorf("a bundle with no credentials still serialises an auth key:\n%s", raw)
	}

	cfg, err := parseAndConfig(t, b, BundleOptions{})
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg.Auth.Enabled() {
		t.Error("Enabled() is true with no credentials in the bundle")
	}
	if _, ok := cfg.Auth.Lookup("anyone"); ok {
		t.Error("Lookup found a user in an empty credential set")
	}
}

// A hash the gateway cannot parse must fail the whole bundle. The mistake this
// catches is a plaintext password pasted where a hash belongs, which otherwise
// looks like a working configuration that simply never lets anybody in.
func TestBundleConfig_RejectsCredentialsThatAreNotHashes(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth Auth
		want string
	}{
		{"plaintext password", Auth{Users: []AuthUser{{User: "a", Hash: "hunter2"}}}, "not a bcrypt hash"},
		{"missing hash", Auth{Users: []AuthUser{{User: "a"}}}, "'hash' is required"},
		{"missing user", Auth{Users: []AuthUser{{Hash: testAuthHash}}}, "'user' is required"},
		{"duplicate user", Auth{Users: []AuthUser{
			{User: "a", Hash: testAuthHash}, {User: "a", Hash: testAuthHash},
		}}, "duplicate user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testdataBundle(t)
			b.Auth = &tc.auth

			_, err := parseAndConfig(t, b, BundleOptions{})
			if err == nil {
				t.Fatalf("Config accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Config error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// `config show` is a command the docs tell operators to run and paste, so a
// bcrypt hash — offline-crackable, even if it is not a password — must not
// survive it.
func TestRedactBundle_HidesCredentialHashes(t *testing.T) {
	b := testdataBundle(t)
	b.Auth = &Auth{Users: []AuthUser{{User: "app@ngm.dev", Hash: testAuthHash}}}

	out, err := RedactBundle(marshal(t, b))
	if err != nil {
		t.Fatalf("RedactBundle: %v", err)
	}
	rendered := string(out)

	if strings.Contains(rendered, testAuthHash) {
		t.Error("a credential hash survived redaction")
	}
	// The username is not a secret and is what makes the output useful.
	if !strings.Contains(rendered, "app@ngm.dev") {
		t.Error("redaction ate the username as well as the hash")
	}
}
