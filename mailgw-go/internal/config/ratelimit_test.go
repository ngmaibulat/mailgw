package config

import (
	"strings"
	"testing"
	"time"
)

// TestRateLimit_DefaultsAreOff is the regression floor for the milestone. A
// configuration that says nothing about rate limits must refuse nothing, and
// there must be no default rate — a mail gateway cannot pick one without
// guessing at somebody's traffic.
func TestRateLimit_DefaultsAreOff(t *testing.T) {
	s, err := ParseServer([]byte("hostname: mx.ngm.dev\n"), FileServer)
	if err != nil {
		t.Fatalf("ParseServer: %v", err)
	}
	if s.RateLimit.Any() {
		t.Errorf("a rate limit is on in a configuration that never mentions one: %+v", s.RateLimit)
	}
	if got := s.RateLimit.Enabled(); len(got) != 0 {
		t.Errorf("Enabled() = %v, want nothing", got)
	}
	// max_keys stays 0 here on purpose: the ceiling lives in internal/ratelimit,
	// where the memory argument for the number is written down.
	if s.RateLimit.MaxKeys != 0 {
		t.Errorf("max_keys defaulted to %d in config; it defaults in the limiter", s.RateLimit.MaxKeys)
	}
}

func TestRateLimit_Parses(t *testing.T) {
	s, err := ParseServer([]byte(`hostname: mx.ngm.dev
ratelimit:
  connect_per_ip: {rate: 100, per: 1m, burst: 20}
  messages_per_sender: {rate: 500, per: 1h}
  auth_failures_per_ip: {rate: 5, per: 10m}
  max_keys: 5000
`), FileServer)
	if err != nil {
		t.Fatalf("ParseServer: %v", err)
	}

	r := s.RateLimit
	if !r.Any() {
		t.Fatal("Any() is false with three limits configured")
	}
	if r.ConnectPerIP.Rate != 100 || r.ConnectPerIP.Per.D() != time.Minute || r.ConnectPerIP.Burst != 20 {
		t.Errorf("connect_per_ip = %+v", r.ConnectPerIP)
	}
	if r.MessagesPerSender.Rate != 500 || r.MessagesPerSender.Per.D() != time.Hour {
		t.Errorf("messages_per_sender = %+v", r.MessagesPerSender)
	}
	if r.MaxKeys != 5000 {
		t.Errorf("max_keys = %d", r.MaxKeys)
	}
	// The ones not mentioned stay off.
	if r.MessagesPerUser.Enabled() || r.RcptsPerDomain.Enabled() {
		t.Error("an unmentioned limit came on")
	}

	// Enabled() is what `check` prints, so it names the keys an operator typed.
	got := strings.Join(r.Enabled(), "; ")
	for _, want := range []string{"connect_per_ip 100/1m0s burst 20", "messages_per_sender 500/1h0m0s", "auth_failures_per_ip 5/10m0s"} {
		if !strings.Contains(got, want) {
			t.Errorf("Enabled() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "rcpts_per_domain") {
		t.Errorf("Enabled() names a limit that is off: %q", got)
	}
}

func TestRateLimit_Validate(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{
			// Unlike max.connections, this is not an explicit zero overwriting a
			// default — there is no default, because a rate has no sensible one.
			name:    "a rate with no window",
			yaml:    "ratelimit:\n  connect_per_ip: {rate: 100}\n",
			wantErr: "needs a positive 'per'",
		},
		{
			name:    "a zero window",
			yaml:    "ratelimit:\n  messages_per_sender: {rate: 10, per: 0}\n",
			wantErr: "needs a positive 'per'",
		},
		{
			name:    "a negative window",
			yaml:    "ratelimit:\n  rcpts_per_domain: {rate: 10, per: -1m}\n",
			wantErr: "needs a positive 'per'",
		},
		{
			name:    "a negative burst",
			yaml:    "ratelimit:\n  connect_per_ip: {rate: 10, per: 1m, burst: -1}\n",
			wantErr: "'burst' cannot be negative",
		},
		{
			name:    "a negative max_keys",
			yaml:    "ratelimit:\n  max_keys: -1\n",
			wantErr: "max_keys cannot be negative",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseServer([]byte("hostname: mx.ngm.dev\n"+c.yaml), FileServer)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got none", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}

// TestRateLimit_ValidateIgnoresHalfFinishedEdits: a `per` with no `rate` limits
// nothing, and refusing to boot over it would be refusing to boot over a comment.
func TestRateLimit_ValidateIgnoresHalfFinishedEdits(t *testing.T) {
	s, err := ParseServer([]byte(`hostname: mx.ngm.dev
ratelimit:
  connect_per_ip: {rate: 0, per: 1m, burst: 50}
`), FileServer)
	if err != nil {
		t.Fatalf("ParseServer: %v", err)
	}
	if s.RateLimit.Any() {
		t.Error("a limit with rate 0 is on")
	}
}
