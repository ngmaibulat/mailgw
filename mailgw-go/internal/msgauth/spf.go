package msgauth

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"

	"blitiri.com.ar/go/spf"
)

// Result is one of the seven RFC 7208 §8 result values, reused for DKIM and
// DMARC because RFC 7601 gives all three the same vocabulary.
type Result string

const (
	ResultNone      Result = "none"
	ResultNeutral   Result = "neutral"
	ResultPass      Result = "pass"
	ResultFail      Result = "fail"
	ResultSoftFail  Result = "softfail"
	ResultTempError Result = "temperror"
	ResultPermError Result = "permerror"
)

// SPFResult is the outcome of one SPF evaluation.
type SPFResult struct {
	// Value is the result. The zero value is the empty string, which reads as
	// "no check was made" — distinct from ResultNone, which means a check ran
	// and found no policy.
	Value Result

	// Domain is the identity that was checked: the envelope sender's domain,
	// or the HELO name when the sender is null.
	//
	// Recorded here because the library does not report it and the answer is
	// not derivable from the sender alone — a rule asking "which domain did
	// this pass for?" would otherwise have to re-implement the fallback.
	Domain string

	// MailFrom and Helo are the identities as given, for the headers.
	MailFrom string
	Helo     string

	// Reason is the library's explanation, for Received-SPF and
	// Authentication-Results. Free text from a remote party's SPF record can
	// reach it through the exp= modifier, so every caller must sanitise before
	// putting it in a header.
	Reason string
}

// Checked reports whether an evaluation actually ran.
func (r SPFResult) Checked() bool { return r.Value != "" }

// CheckSPF evaluates RFC 7208 for one connection.
//
// The identity is the envelope sender's domain, falling back to the HELO name
// for a null sender (§2.4) — a bounce still gets an answer, which matters here
// because this gateway generates bounces and receives them.
//
// Lookup limits stay at the library's RFC defaults and are deliberately not
// configurable, on the same reasoning internal/attach applies to maxParts and
// maxDepth: a DNS amplification budget is not a tuning knob.
func CheckSPF(ctx context.Context, r Resolver, ip netip.Addr, helo, mailFrom string) SPFResult {
	out := SPFResult{MailFrom: mailFrom, Helo: helo, Domain: spfIdentity(helo, mailFrom)}

	if !ip.IsValid() || out.Domain == "" {
		// Nothing to check against. "none" rather than "permerror": no policy
		// was found because none was asked for.
		out.Value = ResultNone
		return out
	}

	// blitiri's API takes net.IP. Unmap first: a client arriving over IPv4 on a
	// dual-stack listener has an ::ffff: address, and an SPF record's ip4:
	// mechanism would not match its 16-byte form.
	res, err := spf.CheckHostWithSender(
		net.IP(ip.Unmap().AsSlice()), helo, mailFrom,
		spf.WithContext(ctx), spf.WithResolver(r),
	)
	out.Value = Result(res)
	out.Reason = spfReason(err)
	return out
}

// spfReason keeps the library's explanation only when it explains something.
//
// blitiri returns a sentinel error on SUCCESS too — ErrMatchedIP, ErrMatchedAll
// and friends name the mechanism that decided the outcome. Putting those in a
// header reads as a failure ("spf=pass reason=\"matched ip\"") and, worse, is
// actively misleading on a refusal, where reason="matched all" means the -all
// mechanism was reached rather than that anything matched. A DNS outage or a
// malformed record, on the other hand, is exactly what an operator opening the
// headers wants to see.
func spfReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, spf.ErrMatchedAll), errors.Is(err, spf.ErrMatchedA),
		errors.Is(err, spf.ErrMatchedIP), errors.Is(err, spf.ErrMatchedMX),
		errors.Is(err, spf.ErrMatchedPTR), errors.Is(err, spf.ErrMatchedExists):
		return ""
	default:
		return err.Error()
	}
}

// spfIdentity picks the domain SPF is evaluated for.
func spfIdentity(helo, mailFrom string) string {
	if d := domainOf(mailFrom); d != "" {
		return d
	}
	// RFC 7208 §2.4: with a null sender the identity is postmaster@<helo>, so
	// the domain checked is the HELO name.
	return strings.TrimSuffix(strings.TrimSpace(helo), ".")
}

// domainOf returns the part of an address after the last "@", lower-cased, or
// "" when there is no "@".
//
// Same rule as ruleset.Domain, restated rather than imported: internal/msgauth
// must not depend on the rule engine, which is what keeps it usable from both
// the session and `explain`.
func domainOf(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(addr[i+1:], "."))
}
