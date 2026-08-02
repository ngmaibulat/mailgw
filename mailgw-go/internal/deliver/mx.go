package deliver

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// Host is one resolved delivery target: a hostname to dial, and the preference
// the DNS said to try it at.
type Host struct {
	Name string
	Pref int
}

// Resolver expands a relay whose exchange is a domain into the hosts that accept
// mail for it.
//
// The zero value is usable and resolves through the system resolver, caching for
// DefaultTTL.
type Resolver struct {
	// Lookup is overridable so tests need no DNS. Nil uses net.LookupMX.
	Lookup func(ctx context.Context, domain string) ([]*net.MX, error)
	// TTL is how long an answer is reused. Zero means DefaultTTL.
	TTL time.Duration
	// now is overridable so a test can age the cache without sleeping.
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedMX
}

// DefaultTTL is how long an MX answer is reused.
//
// Go's resolver does not expose record TTLs — net.LookupMX returns names and
// preferences and nothing else — so this is a fixed interval rather than the
// zone's own. Five minutes is short enough that a planned MX change takes effect
// within a maintenance window, and long enough that a busy queue is not doing a
// DNS round trip per envelope.
const DefaultTTL = 5 * time.Minute

// maxCacheEntries bounds the cache after a sweep has removed what it can.
//
// A constant rather than a configuration key, on the same reasoning
// internal/attach applies to maxParts and maxDepth: this is a ceiling on memory
// under a pathological input, not a knob anyone tunes for throughput. Past it
// the whole map is dropped, because a cache is only ever an optimisation — the
// cost of being wrong is one DNS lookup, and an eviction policy that has to be
// explained is worse than that.
const maxCacheEntries = 1024

type cachedMX struct {
	hosts []Host
	at    time.Time
}

func (r *Resolver) ttl() time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	return DefaultTTL
}

func (r *Resolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Hosts returns the mail exchangers for a domain, best preference first.
//
// A domain with no MX records falls back to the domain itself, per RFC 5321
// §5.1: the absence of an MX record means "deliver to the A/AAAA record of this
// name", not "this domain does not accept mail". Treating it as a failure is a
// classic way to be unable to reach small domains.
func (r *Resolver) Hosts(ctx context.Context, domain string) ([]Host, error) {
	// Lower-cased as well as trimmed: DNS names are case-insensitive, and
	// without this Example.COM and example.com take two cache entries — and,
	// downstream, two pool keys, since poolKey is built from relay.Addr().
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" {
		return nil, fmt.Errorf("mx: empty domain")
	}

	if hosts, ok := r.cached(domain); ok {
		return hosts, nil
	}

	lookup := r.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, d string) ([]*net.MX, error) {
			return net.DefaultResolver.LookupMX(ctx, d)
		}
	}

	records, err := lookup(ctx, domain)
	if err != nil {
		// A domain that genuinely has no MX record reports NotFound rather than
		// an empty list, and that is the implicit-MX case, not an error.
		var dnsErr *net.DNSError
		if !asDNSNotFound(err, &dnsErr) {
			return nil, fmt.Errorf("mx %s: %w", domain, err)
		}
		records = nil
	}

	hosts := make([]Host, 0, len(records))
	for _, m := range records {
		name := strings.TrimSuffix(m.Host, ".")
		// RFC 7505: a single "." target is an explicit null MX — the domain is
		// declaring that it accepts no mail at all. Falling back to the implicit
		// A record here would deliver to a host that said not to.
		if name == "" {
			return nil, fmt.Errorf("mx %s: null MX; the domain accepts no mail", domain)
		}
		hosts = append(hosts, Host{Name: name, Pref: int(m.Pref)})
	}

	if len(hosts) == 0 {
		hosts = []Host{{Name: domain, Pref: 0}}
	}
	sortHosts(hosts)

	r.store(domain, hosts)
	return hosts, nil
}

// sortHosts orders by preference and shuffles equal-preference bands, which is
// what RFC 5321 §5.1 asks for and what relays.Group.Attempts already does for a
// configured group — the two orderings should feel the same to an operator.
//
// Called on every read, not once per lookup. Shuffling only when the answer was
// resolved would fix the order for the whole TTL, so every envelope to a domain
// would take the same exchanger and the spreading this exists for would happen
// once an hour rather than once a message.
func sortHosts(hosts []Host) {
	sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Pref < hosts[j].Pref })
	for start := 0; start < len(hosts); {
		end := start + 1
		for end < len(hosts) && hosts[end].Pref == hosts[start].Pref {
			end++
		}
		if band := hosts[start:end]; len(band) > 1 {
			rand.Shuffle(len(band), func(i, j int) { band[i], band[j] = band[j], band[i] })
		}
		start = end
	}
}

func (r *Resolver) cached(domain string) ([]Host, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cache[domain]
	if !ok || r.clock().Sub(c.at) > r.ttl() {
		return nil, false
	}
	// A copy, so the reshuffle below cannot reorder what a concurrent caller is
	// already walking — and so the cached order stays the resolved one.
	hosts := append([]Host(nil), c.hosts...)
	sortHosts(hosts)
	return hosts, true
}

// store caches an answer, and is also where the cache is bounded.
//
// cached() checks the TTL on READ only, so an expired entry is never returned
// but never removed either — with use_mx against many domains that grew for the
// life of the process. Eviction happens here rather than in a background
// goroutine because the Resolver is rebuilt on every bring-up, is not retained
// anywhere, and has no Close(): giving it a lifecycle would cost a field and a
// stop path to reclaim a map.
//
// Safe against an in-flight caller: cached() returns a defensive copy, so
// deleting an entry cannot change a slice somebody is already reading.
func (r *Resolver) store(domain string, hosts []Host) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]cachedMX{}
	}
	r.cache[domain] = cachedMX{hosts: append([]Host(nil), hosts...), at: r.clock()}

	if len(r.cache) <= maxCacheEntries {
		return
	}
	now, ttl := r.clock(), r.ttl()
	for d, c := range r.cache {
		if d != domain && now.Sub(c.at) > ttl {
			delete(r.cache, d)
		}
	}
	// Still over: every entry is live, so there is nothing to expire and the
	// workload is simply wider than the cache. Start again rather than evict by
	// a policy nobody can predict — keeping the answer just stored, which is the
	// one the caller is about to use.
	if len(r.cache) > maxCacheEntries {
		r.cache = map[string]cachedMX{domain: r.cache[domain]}
	}
}

// asDNSNotFound reports whether err is DNS saying "no such record", which for an
// MX query is the implicit-MX case rather than a failure.
func asDNSNotFound(err error, out **net.DNSError) bool {
	var e *net.DNSError
	if !errors.As(err, &e) {
		return false
	}
	*out = e
	return e.IsNotFound
}

// Expand turns one relay into the list actually dialled.
//
// Without use_mx the relay is its own single target, which is every existing
// configuration and stays byte-identical. With it, the relay's exchange is a
// DOMAIN and this returns one synthetic relay per mail exchanger, each carrying
// the same port, credentials and TLS policy.
//
// Producing whole relays rather than bare hostnames is what keeps the rest of
// the delivery path untouched: Deliver dials relay.Addr() and tlsConfigFor sets
// ServerName from relay.Exchange, so an MX host is verified against its own
// name, which is the only certificate it could present.
func Expand(ctx context.Context, r *Resolver, relay relays.Relay) ([]relays.Relay, error) {
	if !relay.UseMX {
		return []relays.Relay{relay}, nil
	}
	if r == nil {
		r = &Resolver{}
	}

	hosts, err := r.Hosts(ctx, relay.Exchange)
	if err != nil {
		return nil, err
	}

	out := make([]relays.Relay, 0, len(hosts))
	for _, h := range hosts {
		m := relay
		m.Exchange = h.Name
		// The name keeps the configured relay in front so a log line still says
		// which relay group member this came from, then the host that was
		// actually dialled.
		m.Name = relay.Name + "/" + h.Name
		out = append(out, m)
	}
	return out, nil
}
