package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The byte-slice entry points exist so that a configuration bundle pulled from
// Central Management is validated by exactly the code a directory on disk is.
// These tests are what makes "exactly" true rather than aspirational: if the
// two paths ever diverge, that divergence is a fail-open bug in the making.

// TestParseAllowlist_MatchesLoadAllowlist runs one corpus through both entry
// points and requires them to agree on the verdict AND on failing closed.
func TestParseAllowlist_MatchesLoadAllowlist(t *testing.T) {
	bodies := []string{
		`{"allowed":["127.0.0.1","::1","172.18.0.1"]}`,
		`{"allowed":["10.0.0.0/8","2001:db8::/32"]}`,
		`{"allowed":[],"allow_all":true}`,
		`{"allowed":["::ffff:127.0.0.1"]}`,
		// Everything below must fail, and must deny while doing so.
		`null`,
		`{}`,
		`{"allowed":"127.0.0.1"}`,
		`{"allowed":{"ip":"127.0.0.1"}}`,
		`{"allowed":5}`,
		`{"allowed":[]}`,
		`{"allowed":["not-an-ip"]}`,
		`{"allowed":["10.0.0.0/99"]}`,
		`{"allowed":[""]}`,
		`{"allowed":`,
	}

	for _, body := range bodies {
		path := write(t, body)

		fromFile, fileErr := LoadAllowlist(path)
		fromBytes, byteErr := ParseAllowlist([]byte(body), path)

		if (fileErr == nil) != (byteErr == nil) {
			t.Errorf("%s: LoadAllowlist err=%v but ParseAllowlist err=%v", body, fileErr, byteErr)
			continue
		}
		if fileErr != nil {
			// The fail-closed contract is the whole point: a caller that
			// ignores the error must still deny every peer.
			if allowed(t, fromBytes, "127.0.0.1") {
				t.Errorf("%s: a failed parse must deny every peer", body)
			}
			continue
		}
		if !reflect.DeepEqual(fromFile, fromBytes) {
			t.Errorf("%s: file and bytes produced different allowlists:\n  file  %s\n  bytes %s",
				body, fromFile, fromBytes)
		}
	}
}

// A bundle whose allowlist key is absent arrives as a nil RawMessage. That must
// be a named error, not "unexpected end of JSON input", and it must deny.
func TestParseAllowlist_EmptyInputFailsClosed(t *testing.T) {
	for _, raw := range [][]byte{nil, {}} {
		a, err := ParseAllowlist(raw, "allowlist profile")
		if err == nil {
			t.Fatal("expected an error for empty allowlist bytes")
		}
		if !strings.Contains(err.Error(), "missing allowlist") {
			t.Errorf("unhelpful message for empty input: %v", err)
		}
		if a.Allowed(netip.MustParseAddr("127.0.0.1")) {
			t.Error("empty input must deny every peer")
		}
	}
}

// TestParseServer_MatchesLoad is the "did splitting parse from read change file
// mode?" assertion. It is deliberately compared field for field against the
// shipped fixture rather than against a hand-written expectation.
func TestParseServer_MatchesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "config")

	raw, err := os.ReadFile(filepath.Join(dir, FileServer))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	parsed, err := ParseServer(raw, FileServer, false)
	if err != nil {
		t.Fatalf("ParseServer: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(parsed, cfg.Server) {
		t.Errorf("ParseServer and Load disagree:\n  parsed %+v\n  loaded %+v", parsed, cfg.Server)
	}
}

// The shipped fixture must survive strict parsing, or a managed gateway could
// never be given it — which would make the central path untestable against the
// one configuration the whole suite already trusts.
func TestParseServer_ShippedFixtureSurvivesStrictMode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "config", FileServer))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := ParseServer(raw, FileServer, true); err != nil {
		t.Fatalf("the shipped server.yaml must parse strictly: %v", err)
	}
}

func TestParseServer_StrictRejectsUnknownKeys(t *testing.T) {
	// A misspelling that a lax parser silently drops, leaving the default in
	// place and the operator convinced they changed something.
	body := []byte("hostnmae: relay.example\nlisten:\n  - addr: \"0.0.0.0:2525\"\n")

	lax, err := ParseServer(body, FileServer, false)
	if err != nil {
		t.Fatalf("lax parse should accept an unknown key: %v", err)
	}
	if lax.Hostname != "localhost" {
		t.Errorf("expected the default hostname to survive, got %q", lax.Hostname)
	}

	if _, err := ParseServer(body, FileServer, true); err == nil {
		t.Fatal("strict parse must reject an unknown key")
	}
}

func TestParseServer_DefaultsAndValidation(t *testing.T) {
	s, err := ParseServer(nil, FileServer, false)
	if err != nil {
		t.Fatalf("an empty server profile should yield the defaults: %v", err)
	}
	if s.Hostname != "localhost" || len(s.Listen) != 1 {
		t.Errorf("unexpected defaults: %+v", s)
	}
	if s.Outbound.SpoolDir != DefaultSpoolDir {
		t.Errorf("spool dir default = %q, want %q", s.Outbound.SpoolDir, DefaultSpoolDir)
	}

	if _, err := ParseServer([]byte(`hostname: ""`), FileServer, false); err == nil {
		t.Error("validation must still run on the byte path")
	}
}
