// Package validate checks the one request body this service validates.
//
// /api/delivery is strictly validated and answers 400 on any deviation;
// /api/connection and /api/queue validate nothing and default every absent
// field. That asymmetry is not an oversight to be tidied up — it is the contract
// every gateway in the field was built against, and tightening either of the
// other two would start rejecting events that are currently stored.
//
// This is a deliberate, literal port of logservice/src/validation/delivery.ts,
// regexes included. A dependency was considered and rejected: the whole schema
// is four patterns and a dozen type checks, and a validation library expressive
// enough to reproduce zod's exact edge cases would be larger than the code it
// replaced.
//
// # The 400 here is load-bearing in an unusual way
//
// mailgw-go's event client treats ANY 4xx as terminal: the event is written to
// the gateway's failed-events spool and never retried, on the reasoning that an
// identical body cannot start being acceptable. So a 400 from here is a decision
// that the row is unrepresentable, not a "try again". Never widen a 4xx to cover
// a transient condition, and never narrow the schema without knowing which
// gateways currently satisfy it.
//
// # Exported, not internal, because logservice-fiber shares it
//
// M23 put a second implementation beside this one. The two differ in their HTTP
// layer and in nothing else, which is the only reason comparing them means
// anything — so everything below HTTP lives here and is imported by both.
// internal/api is what stays internal. Do not add an HTTP type to this package.
package validate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Delivery is the validated body of POST /api/delivery.
//
// Pointers on every required field so "absent" is distinguishable from "present
// and zero". `false` is a legitimate value for tls/auth/tls_forced and 0 is a
// legitimate delay, so a plain bool or float64 would silently accept a body that
// omitted them — which is exactly what the strict schema exists to refuse.
type Delivery struct {
	UUID         *string  `json:"uuid"`
	DT           *float64 `json:"dt"`
	Sender       *string  `json:"sender"`
	RcptDomain   *string  `json:"rcpt_domain"`
	RcptList     *string  `json:"rcpt_list"`
	RcptAccepted *string  `json:"rcpt_accepted"`
	TLSForced    *bool    `json:"tls_forced"`
	TLS          *bool    `json:"tls"`
	Auth         *bool    `json:"auth"`
	Host         *string  `json:"host"`
	IP           *string  `json:"ip"`
	Port         *string  `json:"port"`
	Response     *string  `json:"response"`
	Delay        *float64 `json:"delay"`

	// Optional since migration 023. They must stay optional: this is the one
	// strictly validated endpoint, so requiring them would 400 every delivery
	// event from a gateway older than that migration — invisibly, because the
	// POST is asynchronous and the row is simply lost.
	Gateway   *string `json:"gateway"`
	RouteRule *string `json:"route_rule"`
}

// hostPattern accepts a DNS name — FQDN or single label — or an IP literal,
// optionally bracketed.
//
// Single labels are accepted DELIBERATELY. Real relay targets include
// `localhost`, container names like `dev-mailhog`, and bare IPs; an FQDN-only
// rule rejects the delivery event for a perfectly successful delivery, and
// because the gateway posts asynchronously that rejection is invisible.
//
// Copied character-for-character from delivery.ts:14-21.
var hostPattern = regexp.MustCompile(
	`^(?:\[?[0-9a-fA-F:.]+\]?|[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*)$`)

// ipv4Pattern is a strict dotted quad.
var ipv4Pattern = regexp.MustCompile(
	`^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$`)

// ipv6Pattern is deliberately permissive, including the IPv4-mapped form.
// This is an audit field: rejecting a well-formed address we failed to
// anticipate would discard the whole record.
var ipv6Pattern = regexp.MustCompile(`^[0-9a-fA-F:]+(?::(?:\d{1,3}\.){3}\d{1,3})?$`)

// portPattern matches zod's z.string().regex(/^[0-9]+$/) — the port is a STRING
// of digits on the wire, into an INT column. mailgw-go sends it as a string for
// this reason; a JSON number here is a 400.
var portPattern = regexp.MustCompile(`^[0-9]+$`)

// emailPattern approximates zod v3's z.string().email().
//
// Exactly reproducing zod's email regex is neither possible nor useful — it is
// an implementation detail that has changed between zod versions. What matters
// is the set of addresses this service currently accepts and rejects, which the
// ported tests pin: one @, a non-empty local part, a dotted or single-label
// domain, no spaces, no commas. The comma is the one that carries weight: the
// legacy Haraka plugin sends a comma-joined recipient list here, and rejecting
// it is the behaviour that made mailgw-go emit one event per recipient.
var emailPattern = regexp.MustCompile(`^[^\s@,]+@[^\s@,]+\.?[^\s@,]*$`)

// Error reports why a body was refused. The message is for the server log, not
// for the client: the route answers a bare {"status":"Fail"}, matching the Bun
// service, so a caller learns nothing about the schema from probing it.
type Error struct {
	Field  string
	Reason string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Reason) }

func fail(field, reason string) error { return &Error{Field: field, Reason: reason} }

// ParseDelivery decodes and validates a delivery event body.
//
// Unknown keys are ignored rather than rejected, matching zod's default
// stripping behaviour: gateways have sent fields this schema does not know
// before (Haraka's `state` and `pipelining` on other endpoints) and will again.
func ParseDelivery(body []byte) (*Delivery, error) {
	var d Delivery
	if err := json.Unmarshal(body, &d); err != nil {
		// A type mismatch on a known field — `"tls": "yes"` — lands here too,
		// which is the same 400 zod would have produced for it.
		return nil, fail("body", "not valid JSON for a delivery event: "+err.Error())
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

func (d *Delivery) validate() error {
	if d.UUID == nil {
		return fail("uuid", "required")
	}
	if d.DT == nil {
		return fail("dt", "required, epoch milliseconds")
	}

	// The envelope sender. Empty is the null sender, MAIL FROM:<>, which every
	// bounce and delivery-status notification uses — requiring a valid address
	// here would reject the gateway's own DSN traffic.
	if d.Sender == nil {
		return fail("sender", "required")
	}
	if *d.Sender != "" && !emailPattern.MatchString(*d.Sender) {
		return fail("sender", "must be an email address or empty (the null sender)")
	}

	if d.RcptDomain == nil || !hostPattern.MatchString(*d.RcptDomain) {
		return fail("rcpt_domain", "must be a hostname or IP literal")
	}

	// One address, not a list. The gateway emits one delivery event per
	// recipient so a multi-recipient message produces one row each.
	if d.RcptList == nil || !emailPattern.MatchString(*d.RcptList) {
		return fail("rcpt_list", "must be exactly one email address")
	}
	if d.RcptAccepted == nil || !emailPattern.MatchString(*d.RcptAccepted) {
		return fail("rcpt_accepted", "must be exactly one email address")
	}

	if d.TLSForced == nil {
		return fail("tls_forced", "required, must be a boolean")
	}
	if d.TLS == nil {
		return fail("tls", "required, must be a boolean")
	}
	if d.Auth == nil {
		return fail("auth", "required, must be a boolean")
	}

	if d.Host == nil || !hostPattern.MatchString(*d.Host) {
		return fail("host", "must be a hostname or IP literal")
	}
	if len(*d.Host) < 1 || len(*d.Host) > 255 {
		return fail("host", "must be between 1 and 255 characters")
	}

	// Empty is allowed: a delivery can fail before a peer address is known, and
	// that outcome still needs recording.
	if d.IP == nil {
		return fail("ip", "required, may be empty")
	}
	if !validIP(*d.IP) {
		return fail("ip", "must be an IPv4 or IPv6 address, or empty")
	}

	if d.Port == nil || !portPattern.MatchString(*d.Port) {
		return fail("port", "must be a string of digits")
	}
	if d.Response == nil {
		return fail("response", "required")
	}
	if d.Delay == nil {
		return fail("delay", "required, seconds")
	}

	if d.Gateway != nil && len(*d.Gateway) > 64 {
		return fail("gateway", "must be at most 64 characters")
	}
	if d.RouteRule != nil && len(*d.RouteRule) > 255 {
		return fail("route_rule", "must be at most 255 characters")
	}
	return nil
}

// validIP mirrors delivery.ts:39-43: empty, or IPv4, or — only when it contains
// a colon — the permissive IPv6 form.
//
// The colon test is what stops the loose IPv6 pattern from accepting things like
// "999" as an address.
func validIP(v string) bool {
	if v == "" {
		return true
	}
	if ipv4Pattern.MatchString(v) {
		return true
	}
	return strings.Contains(v, ":") && ipv6Pattern.MatchString(v)
}

// Str dereferences an optional string, yielding "" when absent. Used by the
// store to turn the optional columns into their NULL-or-value form.
func Str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
