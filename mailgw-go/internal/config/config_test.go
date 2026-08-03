package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// bundleWith returns the fixture bundle with fn applied, marshalled.
//
// These used to be TestLoad_* cases over a configuration directory. The
// directory loader is gone — Central Management is the only source — so the
// assertions that still mean something were moved onto the bundle path and the
// ones already covered by bundle_test.go were dropped rather than duplicated.
func bundleConfig(t *testing.T, fn func(*Bundle)) *Config {
	t.Helper()
	b := testdataBundle(t)
	if fn != nil {
		fn(&b)
	}
	parsed, err := ParseBundle(marshal(t, b))
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	cfg, err := parsed.Config(BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle.Config: %v", err)
	}
	return cfg
}

// A bundle whose server profile says nothing still gets a working gateway.
//
// The connection cap and the rejected-event retention are the two worth
// pinning: both were added long after the first configurations were written, so
// if they did not default, adding the key would have been a no-op for every
// deployment that already existed.
func TestBundleConfig_AppliesDefaults(t *testing.T) {
	cfg := bundleConfig(t, func(b *Bundle) { b.Server = nil })

	if cfg.Server.Max.Bytes != 26214400 {
		t.Errorf("default max.bytes: got %d", cfg.Server.Max.Bytes)
	}
	if len(cfg.Server.Listen) != 1 || cfg.Server.Listen[0].Addr != "0.0.0.0:2525" {
		t.Errorf("default listen: got %+v", cfg.Server.Listen)
	}
	if !cfg.Server.SMTPUTF8 {
		t.Error("smtputf8 should default to true")
	}
	if cfg.Server.Max.Connections != 1024 {
		t.Errorf("default max.connections: got %d, want 1024", cfg.Server.Max.Connections)
	}
	if got := cfg.Server.Events.RejectedRetention.D(); got != 720*time.Hour {
		t.Errorf("default events.rejected_retention: got %v, want 720h", got)
	}
	if got := cfg.Server.ShutdownTimeout.D(); got != DefaultShutdownTimeout {
		t.Errorf("shutdown_timeout: got %v, want %v", got, DefaultShutdownTimeout)
	}
}

// A relay port arrives as a JSON string in the shipped shape, and must survive.
func TestBundleConfig_RelayPortAndGroupsSurvive(t *testing.T) {
	cfg := bundleConfig(t, nil)

	g, ok := cfg.Relays.Lookup("Exchange")
	if !ok {
		t.Fatal("the Exchange relay group should be present")
	}
	if got := g.Members[0].Port.String(); got != "2525" {
		t.Errorf("port: got %q, want \"2525\"", got)
	}
	if !cfg.Allowlist.AllowedHostPort("127.0.0.1:1234") {
		t.Error("127.0.0.1 should be allowed")
	}
	if cfg.Allowlist.AllowedHostPort("8.8.8.8:25") {
		t.Error("8.8.8.8 should not be allowed")
	}
	if cfg.Logging.URLDelivery == "" || cfg.Logging.URLConn == "" || cfg.Logging.URLQueue == "" {
		t.Errorf("logging endpoints not carried: %+v", cfg.Logging)
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

func TestAttachBlockActionDefaultsToReject(t *testing.T) {
	// npFilterAttach.js:45 answered DENYSOFT; we default to a permanent refusal
	// because the digest is on a blocklist and a retry cannot change that.
	if got := (AttachConfig{}).BlockAction(); got != AttachBlockReject {
		t.Errorf("unset on_block = %q, want %q", got, AttachBlockReject)
	}
	if got := (AttachConfig{OnBlock: "QUARANTINE"}).BlockAction(); got != AttachBlockQuarantine {
		t.Errorf("on_block = %q, want %q", got, AttachBlockQuarantine)
	}
	if got := defaults().Attach.BlockAction(); got != AttachBlockReject {
		t.Errorf("defaults().Attach.BlockAction() = %q", got)
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
		"bad attach fail":  func(s *Server) { s.Attach.Fail = "discard" },
		"bad attach block": func(s *Server) { s.Attach.OnBlock = "explode" },
		"attach no url":    func(s *Server) { s.Attach.Enabled = true; s.Attach.URL = "" },
		"bad dsn return":   func(s *Server) { s.DSN.Return = "everything" },
		"implicit no cert": func(s *Server) { s.Listen = []Listener{{Addr: ":465", ImplicitTLS: true}} },
		// Both of these used to be accepted and then silently mean "no bound":
		// attach.timeout feeds a context deadline read inside the DATA reply, and
		// inactivity_timeout feeds the SMTP read/write deadlines.
		"zero attach timeout": func(s *Server) {
			s.Attach.Enabled = true
			s.Attach.URL = "http://127.0.0.1:3000/filter/md5"
			s.Attach.Timeout = 0
		},
		"zero inactivity timeout": func(s *Server) { s.Inactivity = 0 },
		// Same shape again: ParseServer unmarshals over defaults(), so an
		// explicit 0 overwrites 1024 and would mean "no cap" — the state the key
		// exists to end.
		"zero max connections":     func(s *Server) { s.Max.Connections = 0 },
		"negative max connections": func(s *Server) { s.Max.Connections = -1 },
		// proxy_trusted is the only thing between a forged PROXY header and an
		// open relay, so an empty or unparseable list is refused rather than
		// silently meaning "trust nobody" (which drops every connection) or
		// "trust anyone".
		"proxy_protocol without trusted": func(s *Server) {
			s.Listen = []Listener{{Addr: ":25", ProxyProtocol: true}}
		},
		"proxy_protocol empty trusted": func(s *Server) {
			s.Listen = []Listener{{Addr: ":25", ProxyProtocol: true, ProxyTrusted: []string{}}}
		},
		"proxy_trusted unparseable": func(s *Server) {
			s.Listen = []Listener{{Addr: ":25", ProxyProtocol: true, ProxyTrusted: []string{"not-an-address"}}}
		},
		"proxy_trusted without proxy_protocol": func(s *Server) {
			s.Listen = []Listener{{Addr: ":25", ProxyTrusted: []string{"10.0.0.0/8"}}}
		},
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

// A valid PROXY-protocol listener validates, and the keys are omitted from the
// marshalled form when unset — so an existing bundle keeps hashing identically.
func TestValidate_ProxyProtocolListener(t *testing.T) {
	s := defaults()
	s.Hostname = "h"
	s.Listen = []Listener{{
		Addr:          ":25",
		ProxyProtocol: true,
		ProxyTrusted:  []string{"10.0.0.0/8", "192.0.2.1", "::1"},
	}}

	if err := s.validate(); err != nil {
		t.Fatalf("a well-formed proxy listener should validate, got %v", err)
	}

	raw, err := json.Marshal(Listener{Addr: ":25"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "proxy_") {
		t.Errorf("a listener that says nothing about the PROXY protocol should not "+
			"emit the keys, got %s", raw)
	}
}

// ParsePrefixes is the allowlist's own parser, exported for proxy_trusted — so
// it must keep the two normalisations that make the allowlist work: a sloppy
// CIDR is masked, and a v4-mapped v6 prefix becomes plain v4.
func TestParsePrefixes(t *testing.T) {
	got, err := ParsePrefixes([]string{"10.1.2.3/8", "192.0.2.1", "::ffff:127.0.0.1/104"})
	if err != nil {
		t.Fatalf("ParsePrefixes: %v", err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.1/32", "127.0.0.0/8"}
	for i, w := range want {
		if got[i].String() != w {
			t.Errorf("prefix %d = %s, want %s", i, got[i], w)
		}
	}

	if _, err := ParsePrefixes([]string{"nope"}); err == nil {
		t.Error("an unparseable entry must be an error")
	}
}

// attach.timeout only has to be positive when the scanner is actually built, so
// the shipped default — scanning off, and every other key at its default —
// still validates.
func TestValidate_ZeroAttachTimeoutIsFineWhileScanningIsOff(t *testing.T) {
	s := defaults()
	s.Hostname = "h"
	s.Attach.Timeout = 0

	if err := s.validate(); err != nil {
		t.Errorf("a zero attach.timeout with attach disabled should validate, got %v", err)
	}
}

// starttls is an opt-OUT and defaults to true, so — unlike implicit_tls — it
// cannot require a keypair: that would reject every configuration that never
// mentions TLS at all. Without one it is simply inert.
func TestValidate_STARTTLSWithoutAKeypairIsNotAnError(t *testing.T) {
	s := defaults()
	s.Hostname = "h"
	if !s.TLS.STARTTLS {
		t.Fatal("starttls should default to true")
	}
	if err := s.validate(); err != nil {
		t.Errorf("the default config no longer validates: %v", err)
	}
	if s.WantsTLS() != true {
		t.Error("WantsTLS should be true when starttls is on")
	}
	if s.ImplicitTLSWanted() {
		t.Error("no listener asked for implicit TLS")
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
