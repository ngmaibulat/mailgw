package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testdata/config holds copies of the real mailgw/config/*.json files plus the
// server.yaml that replaces connection.ini and friends.
const testdataDir = "../../testdata/config"

func TestLoad_RealConfigDirectory(t *testing.T) {
	cfg, err := Load(testdataDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Hostname != "devbook.local" {
		t.Errorf("hostname: got %q", cfg.Server.Hostname)
	}
	// Values carried over from connection.ini.
	if cfg.Server.Max.Bytes != 26214400 {
		t.Errorf("max.bytes: got %d", cfg.Server.Max.Bytes)
	}
	if cfg.Server.Max.LineLength != 512 {
		t.Errorf("max.line_length: got %d", cfg.Server.Max.LineLength)
	}
	if !cfg.Server.SMTPUTF8 {
		t.Error("smtputf8 should be true")
	}
	if got := cfg.Server.Inactivity.D(); got != 300*time.Second {
		t.Errorf("inactivity_timeout: got %v", got)
	}

	// The shipped relays.json uses a string port and two Exchange members.
	g, ok := cfg.Relays.Lookup("Exchange")
	if !ok {
		t.Fatal("Exchange relay group should load from the real relays.json")
	}
	if len(g.Members) != 2 {
		t.Errorf("Exchange members: got %d, want 2", len(g.Members))
	}
	if got := g.Members[0].Port.String(); got != "2525" {
		t.Errorf("port: got %q, want \"2525\"", got)
	}

	// The shipped ngmfilter.json allows loopback plus the docker bridge.
	if !cfg.Allowlist.AllowedHostPort("127.0.0.1:1234") {
		t.Error("127.0.0.1 should be allowed by the real ngmfilter.json")
	}
	if cfg.Allowlist.AllowedHostPort("8.8.8.8:25") {
		t.Error("8.8.8.8 should not be allowed")
	}

	// logging.json is read unchanged.
	if cfg.Logging.URLDelivery == "" || cfg.Logging.URLConn == "" || cfg.Logging.URLQueue == "" {
		t.Errorf("logging.json not fully loaded: %+v", cfg.Logging)
	}
}

func TestLoad_AppliesDefaultsWhenServerYamlAbsent(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join(testdataDir, "relays.json"), filepath.Join(dir, FileRelays))
	copyFile(t, filepath.Join(testdataDir, "ngmfilter.json"), filepath.Join(dir, FileFilter))

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Max.Bytes != 26214400 {
		t.Errorf("default max.bytes: got %d", cfg.Server.Max.Bytes)
	}
	if len(cfg.Server.Listen) != 1 || cfg.Server.Listen[0].Addr != "0.0.0.0:2525" {
		t.Errorf("default listen: got %+v", cfg.Server.Listen)
	}
	if !cfg.Server.SMTPUTF8 {
		t.Error("smtputf8 should default to true")
	}
}

// A missing or malformed allowlist must stop the process from starting, since
// it is the only thing preventing an open relay.
func TestLoad_FailsWhenAllowlistIsUnusable(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join(testdataDir, "relays.json"), filepath.Join(dir, FileRelays))

	if _, err := Load(dir); err == nil {
		t.Fatal("Load must fail when ngmfilter.json is missing")
	}

	if err := os.WriteFile(filepath.Join(dir, FileFilter), []byte(`{"allowed":"127.0.0.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load must fail when 'allowed' is not an array")
	}
}

func TestLoad_FailsWithoutRelays(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join(testdataDir, "ngmfilter.json"), filepath.Join(dir, FileFilter))
	if _, err := Load(dir); err == nil {
		t.Fatal("Load must fail when relays.json is missing")
	}
}

func TestDuration_AcceptsStringAndSeconds(t *testing.T) {
	var v struct {
		A Duration `json:"a"`
		B Duration `json:"b"`
		C Duration `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"a":"4h","b":30,"c":"1500ms"}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.A.D() != 4*time.Hour {
		t.Errorf("a: got %v", v.A.D())
	}
	if v.B.D() != 30*time.Second {
		t.Errorf("b: got %v", v.B.D())
	}
	if v.C.D() != 1500*time.Millisecond {
		t.Errorf("c: got %v", v.C.D())
	}

	var bad Duration
	if err := json.Unmarshal([]byte(`"not-a-duration"`), &bad); err == nil {
		t.Error("expected an error for an unparseable duration")
	}
}

func TestBackoffFor_RepeatsFinalEntry(t *testing.T) {
	o := Outbound{Backoff: []Duration{
		Duration(time.Minute), Duration(5 * time.Minute), Duration(time.Hour),
	}}
	cases := map[int]time.Duration{
		1:  time.Minute,
		2:  5 * time.Minute,
		3:  time.Hour,
		4:  time.Hour, // final entry repeats
		99: time.Hour,
		0:  time.Minute, // defensive: attempt 0 behaves as the first
		-1: time.Minute,
	}
	for attempt, want := range cases {
		if got := o.BackoffFor(attempt); got != want {
			t.Errorf("attempt %d: got %v, want %v", attempt, got, want)
		}
	}
}

func TestAttachFailClosedByDefault(t *testing.T) {
	if !(AttachConfig{}).FailClosed() {
		t.Error("an unset attach.fail must mean closed")
	}
	if !(AttachConfig{Fail: "closed"}).FailClosed() {
		t.Error("closed must mean closed")
	}
	if (AttachConfig{Fail: "open"}).FailClosed() {
		t.Error("open must mean open")
	}
	if (AttachConfig{Fail: "OPEN"}).FailClosed() {
		t.Error("the comparison should be case-insensitive")
	}
}

func TestValidate_RejectsBadServerConfig(t *testing.T) {
	base := func() Server {
		s := defaults()
		s.Hostname = "h"
		return s
	}

	cases := map[string]func(*Server){
		"no hostname":      func(s *Server) { s.Hostname = "" },
		"no listener":      func(s *Server) { s.Listen = nil },
		"empty addr":       func(s *Server) { s.Listen = []Listener{{Addr: ""}} },
		"zero max bytes":   func(s *Server) { s.Max.Bytes = 0 },
		"no backoff":       func(s *Server) { s.Outbound.Backoff = nil },
		"bad jitter":       func(s *Server) { s.Outbound.Jitter = 1.5 },
		"no spool dir":     func(s *Server) { s.Outbound.SpoolDir = "" },
		"bad attach fail":  func(s *Server) { s.Attach.Fail = "sometimes" },
		"attach no url":    func(s *Server) { s.Attach.Enabled = true; s.Attach.URL = "" },
		"bad dsn return":   func(s *Server) { s.DSN.Return = "everything" },
		"starttls no cert": func(s *Server) { s.TLS.STARTTLS = true },
		"implicit no cert": func(s *Server) { s.Listen = []Listener{{Addr: ":465", ImplicitTLS: true}} },
	}
	for name, mutate := range cases {
		s := base()
		mutate(&s)
		if err := s.validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}

	ok := base()
	if err := ok.validate(); err != nil {
		t.Errorf("the base config should validate, got %v", err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}
