package msgauth

import (
	"bufio"
	"errors"
	"io"
	"net/mail"
	"strings"
)

// maxHeaderPeek bounds how far SigningFromDomain reads looking for the From
// header.
//
// The session already refuses a message with more than max.header_lines, but
// the signer runs at DELIVERY, against a spooled file that could have been
// written by an older build with a different limit. A bound here means a
// pathological header block costs one bounded read rather than the whole
// message.
const maxHeaderPeek = 1 << 20 // 1 MiB, matching smtpsrv's maxHeaderCapture

// ErrNoFrom is returned when a message has no usable From header.
var ErrNoFrom = errors.New("message has no From header with a domain")

// SigningFromDomain reads a message's header block and returns the domain of
// its From header, lower-cased.
//
// This is what picks the DKIM signing key. The From header rather than the
// envelope sender, because d= has to align with RFC5322.From for DMARC to
// credit the signature downstream — a signature keyed off the envelope sender
// is valid, costs the same bytes, and buys nothing whenever the two differ,
// which is every forwarded message and every bounce this gateway generates.
//
// A group address, or a From listing several mailboxes, uses the first: RFC 7489
// §6.6.1 says a message with more than one From address cannot be evaluated for
// DMARC at all, so there is no better answer and no worse one.
func SigningFromDomain(r io.Reader) (string, error) {
	br := bufio.NewReader(io.LimitReader(r, maxHeaderPeek))

	var field strings.Builder
	inFrom := false

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")

		if inFrom && (len(line) == 0 || (line[0] != ' ' && line[0] != '\t')) {
			// The From field ended at the line we just read.
			return FromDomainOf(field.String()), nil
		}
		if len(trimmed) == 0 && len(line) > 0 {
			return "", ErrNoFrom // blank line: header block over, no From
		}

		switch {
		case inFrom:
			field.WriteString(" " + strings.TrimSpace(trimmed))
		case strings.HasPrefix(strings.ToLower(trimmed), "from:"):
			inFrom = true
			field.WriteString(strings.TrimSpace(trimmed[len("from:"):]))
		}

		if err != nil {
			if inFrom {
				return FromDomainOf(field.String()), nil
			}
			return "", ErrNoFrom
		}
	}
}

// FromDomainOf extracts the domain from a From header VALUE.
//
// Falls back to a bare "@domain" scan when the value does not parse as an
// address: a header this gateway did not write may be malformed in ways
// net/mail refuses, and a domain that is visibly there is still the domain a
// receiving DMARC evaluator will use.
func FromDomainOf(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if addrs, err := mail.ParseAddressList(value); err == nil && len(addrs) > 0 {
		return domainOf(addrs[0].Address)
	}
	if a, err := mail.ParseAddress(value); err == nil {
		return domainOf(a.Address)
	}
	// Last resort: the text after the last "@" up to the first delimiter.
	i := strings.LastIndexByte(value, '@')
	if i < 0 {
		return ""
	}
	rest := value[i+1:]
	if j := strings.IndexAny(rest, "> ,;\t\""); j >= 0 {
		rest = rest[:j]
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rest), "."))
}
