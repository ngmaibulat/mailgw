package msgauth

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/emersion/go-msgauth/authres"
)

// HeaderAuthResults is the header this gateway adds to record what it verified.
const HeaderAuthResults = "Authentication-Results"

// HeaderReceivedSPF is the header that records the SPF result on its own, for
// systems that predate RFC 7601 or only ever learned to read this one.
const HeaderReceivedSPF = "Received-SPF"

// maxReason bounds how much of a remote party's explanation is quoted.
//
// Both an SPF exp= modifier and a DKIM verification error can contain text
// chosen by whoever is being checked, and it ends up in a header this gateway
// signs its name to. Truncating is not a security control — sanitizeHeaderValue
// at the session is — it just stops one message's header from being a kilobyte
// of somebody else's prose.
const maxReason = 180

// FormatAuthResults renders the Authentication-Results value for one message.
//
// Methods that did not run are omitted rather than reported as "none": "none"
// is an assertion that a check ran and found no policy, and claiming it for a
// check that never happened would be a lie in the one header whose whole
// purpose is to be trusted downstream.
//
// Returns "" when nothing ran at all, which is the caller's signal not to add
// the header.
func FormatAuthResults(authservID string, spf SPFResult, dk DKIMResult, dm DMARCResult) string {
	var results []authres.Result

	if spf.Checked() {
		results = append(results, &authres.SPFResult{
			Value:  authres.ResultValue(spf.Value),
			Reason: clip(spf.Reason),
			From:   spf.MailFrom,
			Helo:   spf.Helo,
		})
	}
	if dk.Checked() {
		// One result per passing signature, so header.d is unambiguous; a
		// single collapsed line saying "pass" with three domains attached is
		// not something RFC 7601 can express.
		switch {
		case dk.Value == ResultPass:
			for _, d := range dk.Domains {
				results = append(results, &authres.DKIMResult{
					Value: authres.ResultPass, Domain: d,
				})
			}
		default:
			results = append(results, &authres.DKIMResult{
				Value: authres.ResultValue(dk.Value), Reason: clip(dk.Reason),
			})
		}
	}
	if dm.Checked() {
		results = append(results, &authres.DMARCResult{
			Value:  authres.ResultValue(dm.Value),
			Reason: clip(dm.Reason),
			From:   dm.FromDomain,
		})
	}

	if len(results) == 0 {
		return ""
	}
	// authres.Format writes "method=value " and then the parameters, so a
	// result with none — "dkim=none" — comes out with a trailing space before
	// the next semicolon. Legal (RFC 7601 allows CFWS there) but untidy in a
	// header an operator reads, and it would make the golden file depend on the
	// library's whitespace rather than on our own.
	return strings.TrimSpace(strings.ReplaceAll(authres.Format(authservID, results), " ;", ";"))
}

// FormatReceivedSPF renders the Received-SPF value (RFC 7208 §9.1).
func FormatReceivedSPF(res SPFResult, ip netip.Addr, hostname string) string {
	var b strings.Builder
	b.WriteString(string(res.Value))

	comment := fmt.Sprintf("%s: domain of %s", hostname, res.Domain)
	switch res.Value {
	case ResultPass:
		comment += fmt.Sprintf(" designates %s as permitted sender", ip.Unmap())
	case ResultFail:
		comment += fmt.Sprintf(" does not designate %s as permitted sender", ip.Unmap())
	default:
		comment += fmt.Sprintf(" is inconclusive for %s", ip.Unmap())
	}
	// A comment ends at the first unbalanced ")", so a hostile domain or
	// explanation must not be able to close it early. The session's
	// sanitizeHeaderValue removes CR and LF; parentheses are this header's
	// problem alone.
	fmt.Fprintf(&b, " (%s)", strings.NewReplacer("(", "", ")", "").Replace(comment))

	fmt.Fprintf(&b, " client-ip=%s;", ip.Unmap())
	if res.Helo != "" {
		fmt.Fprintf(&b, " helo=%s;", res.Helo)
	}
	if res.MailFrom != "" {
		fmt.Fprintf(&b, " envelope-from=%s;", res.MailFrom)
	} else {
		b.WriteString(" envelope-from=<>;")
	}
	return b.String()
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxReason {
		return s
	}
	return s[:maxReason] + "…"
}

// StripAuthResults removes inbound Authentication-Results header fields that
// claim this gateway's own authserv-id.
//
// RFC 7601 §5 requires it of any system that adds the header: without it, a
// sender can simply assert "dkim=pass" under our name and a downstream reader
// that trusts our identity has no way to tell the forgery from the real thing.
//
// It is deliberately narrow. A field bearing somebody ELSE's authserv-id is
// left alone — a gateway sitting behind an upstream that legitimately verifies
// mail should not destroy that upstream's results, and a rule reading
// header.authentication-results can still see them.
//
// Only the header block is examined; every byte from the blank line onward is
// copied through untouched. A message carrying no matching field comes out
// byte-identical, which is what makes it safe to install unconditionally
// whenever this gateway is going to add a field of its own.
func StripAuthResults(r io.Reader, authservID string) io.Reader {
	id := strings.ToLower(strings.TrimSpace(authservID))
	if id == "" {
		return r
	}
	return &stripReader{br: bufio.NewReader(r), id: id}
}

// stripReader is a synchronous filter, deliberately not an io.Pipe with a
// goroutine behind it: this sits directly in the DATA path, and a reader that
// is abandoned part-way — which is exactly what happens when the spool write
// fails — would leak that goroutine for the life of the process.
type stripReader struct {
	br   *bufio.Reader
	id   string
	out  bytes.Buffer
	body bool // the header block has ended; everything left is body
	err  error
}

func (s *stripReader) Read(p []byte) (int, error) {
	for s.out.Len() == 0 && !s.body && s.err == nil {
		s.fill()
	}
	if s.out.Len() > 0 {
		return s.out.Read(p)
	}
	if s.err != nil {
		return 0, s.err
	}
	// Past the header block the filter is out of the way entirely.
	return s.br.Read(p)
}

// fill consumes one logical header field — its first line plus any folded
// continuations — and keeps it unless it is ours.
func (s *stripReader) fill() {
	line, err := s.br.ReadBytes('\n')
	if len(line) == 0 {
		s.err = err
		return
	}

	if len(bytes.TrimRight(line, "\r\n")) == 0 {
		// The blank line that ends the header block. It stays, obviously.
		s.out.Write(line)
		s.body = true
		return
	}

	field := line
	for err == nil {
		b, perr := s.br.Peek(1)
		if perr != nil || (b[0] != ' ' && b[0] != '\t') {
			break
		}
		var cont []byte
		cont, err = s.br.ReadBytes('\n')
		field = append(field, cont...)
	}
	if err != nil {
		s.err = err
	}

	if !ours(field, s.id) {
		s.out.Write(field)
	}
}

// ours reports whether a complete header field is an Authentication-Results
// bearing our authserv-id.
func ours(field []byte, id string) bool {
	name, value, ok := bytes.Cut(field, []byte(":"))
	if !ok || !strings.EqualFold(strings.TrimSpace(string(name)), HeaderAuthResults) {
		return false
	}
	// The authserv-id is the first token of the value, ending at the first
	// semicolon — or at the end of the field, which is the "authservid; none"
	// shape written without its semicolon. Continuations are already folded in,
	// so an id pushed onto the next line is found here too.
	head, _, _ := strings.Cut(string(value), ";")
	head = strings.TrimSpace(head)
	// RFC 7601 permits an optional version number after the id.
	if i := strings.IndexAny(head, " \t\r\n"); i >= 0 {
		head = head[:i]
	}
	return strings.EqualFold(strings.TrimSuffix(head, "."), strings.TrimSuffix(id, "."))
}
