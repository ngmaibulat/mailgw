package deliver

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

func staticMX(records ...*net.MX) func(context.Context, string) ([]*net.MX, error) {
	return func(context.Context, string) ([]*net.MX, error) { return records, nil }
}

func TestHosts_OrdersByPreference(t *testing.T) {
	r := &Resolver{Lookup: staticMX(
		&net.MX{Host: "backup.partner.com.", Pref: 20},
		&net.MX{Host: "primary.partner.com.", Pref: 10},
	)}

	hosts, err := r.Hosts(context.Background(), "partner.com")
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want 2", len(hosts))
	}
	if hosts[0].Name != "primary.partner.com" {
		t.Errorf("first host = %q, want the lowest preference", hosts[0].Name)
	}
	// The trailing dot is how DNS renders a name and not how you dial one.
	for _, h := range hosts {
		if strings.HasSuffix(h.Name, ".") {
			t.Errorf("host %q kept its trailing dot", h.Name)
		}
	}
}

// RFC 5321 §5.1: no MX record means "deliver to the A record of this name", not
// "this domain takes no mail". Treating it as a failure is a classic way to be
// unable to reach small domains.
func TestHosts_FallsBackToTheDomainItself(t *testing.T) {
	r := &Resolver{Lookup: staticMX()}

	hosts, err := r.Hosts(context.Background(), "small.example")
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Name != "small.example" {
		t.Errorf("got %v, want the domain itself as an implicit MX", hosts)
	}
}

// A NXDOMAIN-shaped answer is the same implicit-MX case, not an error.
func TestHosts_TreatsNotFoundAsImplicitMX(t *testing.T) {
	r := &Resolver{Lookup: func(context.Context, string) ([]*net.MX, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "small.example", IsNotFound: true}
	}}

	hosts, err := r.Hosts(context.Background(), "small.example")
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Name != "small.example" {
		t.Errorf("got %v, want the domain itself", hosts)
	}
}

// A real lookup failure — SERVFAIL, timeout — must NOT become an implicit MX.
// Delivering to the domain's A record because DNS was briefly unreachable would
// send mail to whatever happens to answer there.
func TestHosts_PropagatesARealLookupFailure(t *testing.T) {
	r := &Resolver{Lookup: func(context.Context, string) ([]*net.MX, error) {
		return nil, &net.DNSError{Err: "server misbehaving", Name: "partner.com", IsTemporary: true}
	}}

	if _, err := r.Hosts(context.Background(), "partner.com"); err == nil {
		t.Fatal("a DNS failure was silently treated as an implicit MX")
	}
}

// RFC 7505: a "." target says the domain accepts no mail at all. Falling through
// to the implicit A record would deliver to a host that explicitly said not to.
func TestHosts_RefusesANullMX(t *testing.T) {
	r := &Resolver{Lookup: staticMX(&net.MX{Host: ".", Pref: 0})}

	_, err := r.Hosts(context.Background(), "nomail.example")
	if err == nil {
		t.Fatal("a null MX was accepted")
	}
	if !strings.Contains(err.Error(), "no mail") {
		t.Errorf("error %q does not explain the null MX", err)
	}
}

func TestHosts_CachesAndExpires(t *testing.T) {
	calls := 0
	now := time.Now()
	r := &Resolver{
		TTL: time.Minute,
		now: func() time.Time { return now },
		Lookup: func(context.Context, string) ([]*net.MX, error) {
			calls++
			return []*net.MX{{Host: "mx.partner.com.", Pref: 10}}, nil
		},
	}

	for range 3 {
		if _, err := r.Hosts(context.Background(), "partner.com"); err != nil {
			t.Fatalf("Hosts: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("resolved %d times, want 1 — the cache is not being used", calls)
	}

	now = now.Add(2 * time.Minute)
	if _, err := r.Hosts(context.Background(), "partner.com"); err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if calls != 2 {
		t.Errorf("resolved %d times after the TTL expired, want 2", calls)
	}
}

// Every existing configuration has no use_mx, and must dial exactly what it says.
func TestExpand_LeavesAnOrdinaryRelayAlone(t *testing.T) {
	relay := relays.Relay{Name: "smarthost", Exchange: "mail.example.com", Port: 587}

	got, err := Expand(context.Background(), &Resolver{}, relay)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(got) != 1 || got[0] != relay {
		t.Errorf("Expand changed a non-MX relay: %v", got)
	}
}

func TestExpand_ProducesOneRelayPerExchanger(t *testing.T) {
	r := &Resolver{Lookup: staticMX(
		&net.MX{Host: "mx1.partner.com.", Pref: 10},
		&net.MX{Host: "mx2.partner.com.", Pref: 20},
	)}
	relay := relays.Relay{
		Name: "partner", Exchange: "partner.com", Port: 25, UseMX: true,
		TLS: relays.TLSRequired, AuthUser: "u", AuthPass: "p",
	}

	got, err := Expand(context.Background(), r, relay)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d relays, want 2", len(got))
	}

	if got[0].Exchange != "mx1.partner.com" {
		t.Errorf("first target = %q, want the lowest-preference exchanger", got[0].Exchange)
	}
	for _, m := range got {
		// Credentials and policy must ride along, or an MX-resolved relay would
		// silently deliver in the clear or unauthenticated.
		if m.Port != 25 || m.TLS != relays.TLSRequired || m.AuthUser != "u" || m.AuthPass != "p" {
			t.Errorf("target %q lost the relay's port, credentials or TLS policy: %+v", m.Exchange, m)
		}
		// The name says which configured relay this came from and which host was
		// actually dialled — a log line naming only "partner" three times is
		// useless when one exchanger is the broken one.
		if !strings.HasPrefix(m.Name, "partner/") {
			t.Errorf("target name %q does not identify its relay and host", m.Name)
		}
	}
}

// The whole point of expanding into relays rather than bare hostnames: TLS is
// verified against the exchanger's own name, which is the only certificate it
// could present. Verifying against the envelope domain would fail on every
// correctly-configured MX.
func TestExpand_TLSVerifiesAgainstTheExchangerName(t *testing.T) {
	r := &Resolver{Lookup: staticMX(&net.MX{Host: "mx1.partner.com.", Pref: 10})}
	relay := relays.Relay{Name: "partner", Exchange: "partner.com", Port: 25, UseMX: true}

	got, err := Expand(context.Background(), r, relay)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	cfg := tlsConfigFor(got[0], Options{})
	if cfg.ServerName != "mx1.partner.com" {
		t.Errorf("ServerName = %q, want the exchanger's own name", cfg.ServerName)
	}
}

func cacheLen(r *Resolver) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cache)
}

// The TTL used to be checked on read only, so an expired entry was never
// returned and never removed either: with use_mx against many domains the map
// grew for the life of the process.
func TestStore_SweepsExpiredEntries(t *testing.T) {
	now := time.Now()
	r := &Resolver{
		TTL:    time.Minute,
		now:    func() time.Time { return now },
		Lookup: staticMX(&net.MX{Host: "mx.partner.com.", Pref: 10}),
	}

	for i := range maxCacheEntries {
		if _, err := r.Hosts(context.Background(), "d"+strconv.Itoa(i)+".example"); err != nil {
			t.Fatalf("Hosts: %v", err)
		}
	}
	if got := cacheLen(r); got != maxCacheEntries {
		t.Fatalf("cache holds %d entries, want %d before anything expires", got, maxCacheEntries)
	}

	// Age everything past the TTL, then store once more. The sweep on that write
	// is what reclaims the lot.
	now = now.Add(2 * time.Minute)
	if _, err := r.Hosts(context.Background(), "fresh.example"); err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if got := cacheLen(r); got != 1 {
		t.Errorf("cache holds %d entries after a sweep, want 1", got)
	}
}

// A workload wider than the cache within one TTL window has nothing to expire.
// The map is dropped rather than evicted by a policy nobody can predict — a
// cache is an optimisation, and being wrong costs one DNS lookup.
func TestStore_BoundsTheCacheWhenNothingCanExpire(t *testing.T) {
	now := time.Now()
	r := &Resolver{
		TTL:    time.Hour,
		now:    func() time.Time { return now },
		Lookup: staticMX(&net.MX{Host: "mx.partner.com.", Pref: 10}),
	}

	for i := range maxCacheEntries * 3 {
		if _, err := r.Hosts(context.Background(), "d"+strconv.Itoa(i)+".example"); err != nil {
			t.Fatalf("Hosts: %v", err)
		}
	}
	if got := cacheLen(r); got > maxCacheEntries {
		t.Errorf("cache grew to %d entries, want at most %d", got, maxCacheEntries)
	}

	// The answer just stored is the one the caller is about to use, so it must
	// survive the reset.
	last := "d" + strconv.Itoa(maxCacheEntries*3-1) + ".example"
	if _, ok := r.cached(last); !ok {
		t.Error("the most recently stored answer was evicted by its own write")
	}
}
