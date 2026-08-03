package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// These test `check`'s reporting, which is why they live here rather than in
// internal/node with the gateway wiring they were written beside: M19 moved the
// composition root out of package main and left the operator-facing output
// behind, so the tests followed the code they exercise.

func TestReportMsgAuth_NamesTheChecksARuleTurnedOn(t *testing.T) {
	cfg := &config.Config{Server: config.Server{Hostname: "relay.example"}}
	rules := compileForTest(t, `
version: 1
routes:
  - name: r
    match: {field: dmarc.result, op: eq, value: pass}
    then: [{action: relay, relay: Outbound}]
`)

	out := captureStderr(t, func() { reportMsgAuth(cfg, rules) })
	for _, want := range []string{"spf", "dkim", "dmarc", "turned on by a rule"} {
		if !strings.Contains(out, want) {
			t.Errorf("check output does not mention %q:\n%s", want, out)
		}
	}
}

func TestReportRateLimits_SaysWhenNothingIsThrottled(t *testing.T) {
	cfg := &config.Config{Server: config.Server{Hostname: "relay.example"}}

	out := captureStderr(t, func() { reportRateLimits(cfg) })
	if !strings.Contains(out, "off — nothing is throttled") {
		t.Errorf("check does not say that nothing is limited:\n%s", out)
	}

	cfg.Server.RateLimit.ConnectPerIP = config.RateLimit{Rate: 10, Per: config.Duration(time.Minute)}
	out = captureStderr(t, func() { reportRateLimits(cfg) })
	for _, want := range []string{"connect_per_ip 10/1m0s", "per gateway", "4xx"} {
		if !strings.Contains(out, want) {
			t.Errorf("check output does not mention %q:\n%s", want, out)
		}
	}
}

// TestListenerChain_PerIPRateLimit drives the REAL chain — proxyproto, Meter,
// TLS, Guard, Throttle, Limit — because that composition is assembled in this
// package and nowhere else.

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

// captureStderr collects what a function writes to os.Stderr — which is where
// `check` puts everything an operator is meant to read.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()

	fn()
	w.Close()
	os.Stderr = saved
	return <-done
}
