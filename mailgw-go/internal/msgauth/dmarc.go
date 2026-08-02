package msgauth

import (
	"context"
	"errors"
	"strings"

	"github.com/emersion/go-msgauth/dmarc"
)

// Alignment modes, named locally so the third-party spelling appears once.
//
// Note go-msgauth declares AlignmentRelaxed as an untyped constant, so it is
// restated here with its type to keep comparisons honest.
const (
	relaxed dmarc.AlignmentMode = dmarc.AlignmentRelaxed
	strict  dmarc.AlignmentMode = dmarc.AlignmentStrict
)

// DMARCResult is the outcome of evaluating the From domain's DMARC record
// against the SPF and DKIM results.
type DMARCResult struct {
	// Value is the result. Empty means no evaluation ran.
	Value Result

	// Policy is the published policy that applies to THIS message — p=, or sp=
	// when the record was found on a parent domain and the From domain is a
	// subdomain of it. One of "none", "quarantine" or "reject"; empty when no
	// record was found.
	//
	// Reported rather than acted on. Whether a p=reject failure is rejected,
	// quarantined or merely tagged is a rule the operator writes; a gateway
	// that silently started refusing mail on an upgrade would be the opposite
	// of every other default here.
	Policy string

	// FromDomain is the RFC5322.From domain the evaluation was about.
	FromDomain string

	// SPFAligned and DKIMAligned record which identifier carried the pass. A
	// DMARC pass needs exactly one of them.
	SPFAligned  bool
	DKIMAligned bool

	// Reason explains a non-pass, for Authentication-Results.
	Reason string
}

// Checked reports whether an evaluation actually ran.
func (r DMARCResult) Checked() bool { return r.Value != "" }

// EvaluateDMARC applies RFC 7489 to an SPF and a DKIM result.
//
// It is policy over two facts and nothing else — it performs exactly one DNS
// lookup (two when the record is inherited from a parent), which is why it is
// last in the sequence and why it is small.
func EvaluateDMARC(ctx context.Context, r Resolver, fromDomain string, spf SPFResult, dk DKIMResult) DMARCResult {
	out := DMARCResult{FromDomain: strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fromDomain), "."))}
	if out.FromDomain == "" {
		// No usable From header. RFC 7489 §6.6.1 says a message without exactly
		// one From domain cannot be evaluated; "none" is the honest answer.
		out.Value = ResultNone
		out.Reason = "no From domain"
		return out
	}

	rec, owner, err := lookupDMARC(ctx, r, out.FromDomain)
	switch {
	case errors.Is(err, dmarc.ErrNoPolicy):
		out.Value = ResultNone
		return out
	case err != nil && dmarc.IsTempFail(err):
		out.Value = ResultTempError
		out.Reason = err.Error()
		return out
	case err != nil:
		// A malformed record is the domain owner's error and cannot become
		// correct on a retry.
		out.Value = ResultPermError
		out.Reason = err.Error()
		return out
	}

	out.Policy = string(rec.Policy)
	if owner != out.FromDomain && rec.SubdomainPolicy != "" {
		// The record was inherited; sp= is what its owner said should happen to
		// subdomains, and this message is from one.
		out.Policy = string(rec.SubdomainPolicy)
	}

	if spf.Value == ResultPass {
		out.SPFAligned = aligned(spf.Domain, out.FromDomain, rec.SPFAlignment)
	}
	for _, d := range dk.Domains {
		if aligned(d, out.FromDomain, rec.DKIMAlignment) {
			out.DKIMAligned = true
			break
		}
	}

	if out.SPFAligned || out.DKIMAligned {
		out.Value = ResultPass
		return out
	}
	out.Value = ResultFail
	out.Reason = dmarcReason(spf, dk)
	return out
}

// lookupDMARC finds the record for a domain, falling back to its parent.
//
// RFC 7489 §6.6.3 says the fallback is to the ORGANIZATIONAL domain, which is
// defined by the public suffix list. There is no PSL here and adding one would
// be a megabyte of data and a fourth dependency for a field nothing rejects on,
// so this walks up exactly one label and only when doing so cannot land on a
// public suffix by the crude test of leaving at least two labels behind.
//
// The consequence, stated plainly: mail from a.b.example.com inherits nothing
// from example.com's record. That under-reports DMARC coverage — it can turn a
// pass into a none, never a fail into a pass — which is the direction an
// approximation in an advisory field should err.
func lookupDMARC(ctx context.Context, r Resolver, domain string) (*dmarc.Record, string, error) {
	opts := &dmarc.LookupOptions{LookupTXT: txtLookup(ctx, r)}

	rec, err := dmarc.LookupWithOptions(domain, opts)
	if err == nil {
		return rec, domain, nil
	}
	if !errors.Is(err, dmarc.ErrNoPolicy) {
		return nil, "", err
	}

	parent := parentDomain(domain)
	if strings.Count(parent, ".") < 1 {
		return nil, "", dmarc.ErrNoPolicy
	}
	rec, err = dmarc.LookupWithOptions(parent, opts)
	if err != nil {
		return nil, "", err
	}
	return rec, parent, nil
}

func parentDomain(d string) string {
	if i := strings.IndexByte(d, '.'); i >= 0 {
		return d[i+1:]
	}
	return ""
}

// aligned reports whether an authenticated domain aligns with the From domain.
//
// Strict alignment is exact equality. Relaxed alignment is defined over
// organizational domains, and is approximated here — for the reason given on
// lookupDMARC — as "equal, or one is a subdomain of the other". That covers the
// case relaxed alignment exists for (a bounce address at mail.example.com for a
// From at example.com) and misses only siblings, a.example.com against
// b.example.com, which is again a pass reported as a fail rather than the
// reverse.
func aligned(authDomain, fromDomain string, mode dmarc.AlignmentMode) bool {
	a := strings.ToLower(strings.TrimSuffix(authDomain, "."))
	f := strings.ToLower(strings.TrimSuffix(fromDomain, "."))
	if a == "" || f == "" {
		return false
	}
	if a == f {
		return true
	}
	if mode == strict {
		return false
	}
	return strings.HasSuffix(a, "."+f) || strings.HasSuffix(f, "."+a)
}

func dmarcReason(spf SPFResult, dk DKIMResult) string {
	switch {
	case spf.Value == ResultPass && len(dk.Domains) > 0:
		return "neither SPF nor DKIM identifier is aligned with the From domain"
	case spf.Value == ResultPass:
		return "SPF passed but is not aligned with the From domain"
	case len(dk.Domains) > 0:
		return "DKIM passed but no signature is aligned with the From domain"
	default:
		return "no aligned authenticated identifier"
	}
}
