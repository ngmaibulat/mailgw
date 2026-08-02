// Package msgauth answers three questions about a message: did the sending IP
// have permission to use the envelope sender's domain (SPF), do the message's
// DKIM signatures verify, and do either of those align with the From header the
// way the From domain's DMARC record demands.
//
// Like internal/dsn and internal/attach it knows nothing about the spool, the
// session, the configuration or the rule engine. Everything it needs arrives as
// an argument, which is what lets the same code back the SMTP session and
// `mailgw-go explain`, and what lets every test here run against fixtures and a
// map-backed resolver rather than against the network.
//
// It also renders the two headers that record the answers — Authentication-Results
// (RFC 7601) and Received-SPF (RFC 7208 §9.1) — and strips inbound
// Authentication-Results fields that forge this gateway's own authserv-id, which
// RFC 7601 §5 requires of anything that adds one.
package msgauth

import (
	"context"
	"net"
	"time"
)

// Resolver is the DNS surface the checks here need.
//
// The method set is exactly *net.Resolver's, which is also exactly
// blitiri.com.ar/go/spf's DNSResolver — so a value of this type is assignable to
// the library's interface without an adapter, and one stub in a test serves SPF,
// DKIM and DMARC at once.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// NewResolver returns a Resolver backed by the system resolver, bounding each
// individual lookup at timeout.
//
// The per-lookup bound is a floor, not the real limit: every caller here also
// passes a context, and an inbound check runs inside the SMTP reply, so it is
// the caller's deadline that has to cut the whole walk short. This exists so a
// single black-holed nameserver cannot stall a walk that the context would only
// interrupt at its very end.
func NewResolver(timeout time.Duration) Resolver {
	return &netResolver{timeout: timeout, r: net.DefaultResolver}
}

type netResolver struct {
	timeout time.Duration
	r       *net.Resolver
}

func (n *netResolver) sub(ctx context.Context) (context.Context, context.CancelFunc) {
	if n.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, n.timeout)
}

func (n *netResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	ctx, cancel := n.sub(ctx)
	defer cancel()
	return n.r.LookupTXT(ctx, name)
}

func (n *netResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	ctx, cancel := n.sub(ctx)
	defer cancel()
	return n.r.LookupMX(ctx, name)
}

func (n *netResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	ctx, cancel := n.sub(ctx)
	defer cancel()
	return n.r.LookupIPAddr(ctx, host)
}

func (n *netResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	ctx, cancel := n.sub(ctx)
	defer cancel()
	return n.r.LookupAddr(ctx, addr)
}

// txtLookup adapts a Resolver to the func(string) ([]string, error) shape
// go-msgauth's dkim and dmarc packages take.
//
// The context is captured rather than passed, because those two libraries
// declare their hook without one. That is safe here and nowhere near as bad as
// it looks: the closure is built per call, lives only for that call, and the
// captured context is the caller's — which is exactly the deadline the lookup
// should die on.
func txtLookup(ctx context.Context, r Resolver) func(string) ([]string, error) {
	return func(name string) ([]string, error) {
		return r.LookupTXT(ctx, name)
	}
}
