package msgauth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMResult collapses every signature on a message into one answer.
type DKIMResult struct {
	// Value is the message's result. Empty means no verification ran.
	Value Result

	// Domains are the d= values of the signatures that PASSED, sorted and
	// deduplicated. Only passing ones: a rule matching on a failed signature's
	// domain would be matching on a claim anybody can make.
	Domains []string

	// Reason explains a non-pass, for Authentication-Results. Sanitise before
	// putting it in a header — it can quote a remote signature's contents.
	Reason string

	// Signatures is every verification, in the order the signatures appeared.
	// Nothing consumes it yet; it exists so a future per-signature rule field
	// does not need this function's shape to change.
	Signatures []Signature
}

// Signature is one DKIM signature's verification.
type Signature struct {
	Domain     string
	Identifier string
	Value      Result
	Err        string
}

// Checked reports whether verification actually ran.
func (r DKIMResult) Checked() bool { return r.Value != "" }

// VerifyDKIM verifies every DKIM signature on a message.
//
// max bounds how many signatures are checked; each one costs a DNS lookup and
// an RSA verification, so an unbounded count is an amplification primitive
// against this gateway. Signatures past the bound are ignored and the result is
// still reported for the ones that were checked — refusing to answer because a
// message carried too many signatures would be a worse outcome than answering
// about the first ten.
func VerifyDKIM(ctx context.Context, r Resolver, msg io.Reader, max int) DKIMResult {
	verifs, err := dkim.VerifyWithOptions(msg, &dkim.VerifyOptions{
		LookupTXT:        txtLookup(ctx, r),
		MaxVerifications: max,
	})
	// ErrTooManySignatures comes back WITH the verifications it did perform, so
	// it is not a failure — everything else that returns a nil slice is.
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		// A message whose header block could not be read is not a DKIM failure,
		// it is an unanswerable question. permerror says so; fail would assert
		// that a signature was present and wrong.
		return DKIMResult{Value: ResultPermError, Reason: err.Error()}
	}
	if len(verifs) == 0 {
		return DKIMResult{Value: ResultNone}
	}

	out := DKIMResult{Signatures: make([]Signature, 0, len(verifs))}
	var domains []string
	worst := ResultNone
	var worstReason string

	for _, v := range verifs {
		s := Signature{Domain: strings.ToLower(v.Domain), Identifier: v.Identifier}
		switch {
		case v.Err == nil:
			s.Value = ResultPass
		case dkim.IsTempFail(v.Err):
			s.Value = ResultTempError
		case dkim.IsPermFail(v.Err):
			s.Value = ResultPermError
		default:
			s.Value = ResultFail
		}
		if v.Err != nil {
			s.Err = v.Err.Error()
		}
		out.Signatures = append(out.Signatures, s)

		if s.Value == ResultPass {
			domains = append(domains, s.Domain)
			continue
		}
		if dkimRank(s.Value) > dkimRank(worst) {
			worst, worstReason = s.Value, s.Err
		}
	}

	if len(domains) > 0 {
		// One passing signature is a pass: RFC 6376 §6.1 makes each signature
		// an independent claim, and a valid one is not weakened by a broken one
		// beside it.
		sort.Strings(domains)
		out.Value = ResultPass
		out.Domains = dedupe(domains)
		return out
	}
	out.Value = worst
	out.Reason = worstReason
	return out
}

// dkimRank orders the non-pass outcomes so the reported one is the most
// informative: a temporary error is worth retrying and outranks a permanent
// one, which in turn outranks a plain bad signature.
func dkimRank(r Result) int {
	switch r {
	case ResultTempError:
		return 3
	case ResultPermError:
		return 2
	case ResultFail:
		return 1
	}
	return 0
}

func dedupe(sorted []string) []string {
	out := sorted[:0:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// SignOptions configures SignDKIM.
type SignOptions struct {
	Domain     string
	Selector   string
	Signer     crypto.Signer
	HeaderKeys []string
	HeaderCan  string
	BodyCan    string
	Expiration time.Duration
	// Now is overridable so a test can pin the t= tag. Nil means time.Now.
	Now func() time.Time
}

// DefaultHeaderKeys is the header set signed when a configuration names none.
//
// RFC 6376 §5.4.1's recommended list, minus the headers this gateway or the
// next hop adds or rewrites. Notably absent: Received (every hop adds one) and
// Authentication-Results (we may have just added one, and a downstream boundary
// is entitled to strip it).
var DefaultHeaderKeys = []string{
	"From", "To", "Cc", "Subject", "Date", "Message-ID", "Reply-To",
	"In-Reply-To", "References", "MIME-Version",
	"Content-Type", "Content-Transfer-Encoding",
}

// SignDKIM computes a message's DKIM-Signature header field and returns it,
// including the trailing CRLF. It does NOT return the message.
//
// This is deliberately not dkim.Sign, which buffers the whole message in memory
// so it can emit the signature ahead of it. The caller here has the message on
// disk and can simply open it twice: two passes over a spool file beats holding
// a 25 MiB message in RAM once per delivery attempt.
//
// The reader must supply the message EXACTLY as it will go on the wire —
// including every header this gateway prepends. A signature over anything else
// covers a message that will never exist.
func SignDKIM(msg io.Reader, o SignOptions) (string, error) {
	if o.Domain == "" || o.Selector == "" || o.Signer == nil {
		return "", errors.New("dkim: domain, selector and key are all required")
	}

	keys := o.HeaderKeys
	if len(keys) == 0 {
		keys = DefaultHeaderKeys
	}

	opts := &dkim.SignOptions{
		Domain:                 o.Domain,
		Selector:               o.Selector,
		Signer:                 o.Signer,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: canonicalization(o.HeaderCan),
		BodyCanonicalization:   canonicalization(o.BodyCan),
		HeaderKeys:             keys,
	}
	if o.Expiration > 0 {
		now := time.Now
		if o.Now != nil {
			now = o.Now
		}
		opts.Expiration = now().Add(o.Expiration)
	}

	s, err := dkim.NewSigner(opts)
	if err != nil {
		return "", fmt.Errorf("dkim signer: %w", err)
	}
	if _, err := io.Copy(s, msg); err != nil {
		// Close releases the signer's goroutine; its own error is uninteresting
		// once the copy has already failed.
		_ = s.Close()
		return "", fmt.Errorf("dkim sign: %w", err)
	}
	if err := s.Close(); err != nil {
		return "", fmt.Errorf("dkim sign: %w", err)
	}
	return s.Signature(), nil
}

// Canonicalization pairs accepted in configuration.
const (
	CanonSimple  = "simple"
	CanonRelaxed = "relaxed"
)

// ParseCanonicalization splits a "header/body" pair such as "relaxed/relaxed".
//
// A bare value applies to both, which is how the c= tag itself reads. An empty
// string yields relaxed/relaxed rather than RFC 6376's simple/simple default:
// simple canonicalization breaks on any whitespace change in transit, and every
// mail system between here and the recipient is entitled to make one.
func ParseCanonicalization(s string) (header, body string, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return CanonRelaxed, CanonRelaxed, nil
	}
	header, body, ok := strings.Cut(s, "/")
	if !ok {
		body = header
	}
	for _, c := range []string{header, body} {
		if c != CanonSimple && c != CanonRelaxed {
			return "", "", fmt.Errorf("unknown canonicalization %q (want simple or relaxed, or a pair such as relaxed/relaxed)", s)
		}
	}
	return header, body, nil
}

func canonicalization(s string) dkim.Canonicalization {
	if s == CanonSimple {
		return dkim.CanonicalizationSimple
	}
	return dkim.CanonicalizationRelaxed
}
