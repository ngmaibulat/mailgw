package central

import (
	"errors"
	"fmt"
	"strings"
)

// The console answers every authentication failure with HTTP 401 and puts the
// actual reason in the JSON body, so the status code alone cannot tell a bad
// key from a skewed clock. These sentinels are what callers switch on.
//
// The reason strings below are matched against
// webui-fastify/src/agent/verify.ts. That coupling is deliberate and safe: a
// wording change on the console degrades the classification to
// ErrUnauthorized, which only makes a log line less specific — it never
// changes a decision.
var (
	// ErrNotApproved is a 403 from /agent/config: registered, waiting for an
	// operator. Expected, not a fault; back off quietly.
	ErrNotApproved = errors.New("central: gateway is not approved yet")
	// ErrNoConfig is a 404 from /agent/config: approved, but nothing has been
	// deployed to this gateway yet. Also expected.
	ErrNoConfig = errors.New("central: no configuration has been deployed")
	// ErrUnknownGateway means the console has forgotten this gateway. The fix
	// is to register again with the same key rather than to wipe the data dir.
	ErrUnknownGateway = errors.New("central: the console does not know this gateway")
	// ErrClockSkew means the signature was well-formed but the timestamp fell
	// outside the ±300s window. "Fix the gateway's clock" is the action, and it
	// is otherwise indistinguishable from a bad key.
	ErrClockSkew = errors.New("central: timestamp rejected — check the gateway clock (NTP)")
	// ErrUnauthorized is any other rejected signature.
	ErrUnauthorized = errors.New("central: signature rejected")
	// ErrResponseTooLarge means the console's answer exceeded maxResponseBody.
	//
	// Deliberately NOT an HTTPError, so IsUnreachable reports true and the poll
	// loop treats it exactly as it treats an outage: keep the last-good
	// configuration, retry on the next tick. The console answering with
	// something this size is a fault on its side or an attack, and neither is a
	// reason to stop relaying mail on a configuration that already works.
	ErrResponseTooLarge = errors.New("central: response body is too large")
)

// HTTPError means the console answered and said no. Its *absence* from an
// error chain means we never reached the console at all — that is the whole
// reachable-versus-rejected distinction, and it is the one the poll loop needs
// in order to decide between "keep running quietly" and "something is wrong".
type HTTPError struct {
	Op     string // register | status | config | report
	Status int
	Method string
	Target string
	// Reason is the JSON "message" field when the body had one, else a
	// truncated copy of the body.
	Reason string
}

func (e *HTTPError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("central: %s: %s %s: HTTP %d", e.Op, e.Method, e.Target, e.Status)
	}
	return fmt.Sprintf("central: %s: %s %s: HTTP %d: %s", e.Op, e.Method, e.Target, e.Status, e.Reason)
}

// Unwrap maps the wire answer onto a sentinel, so a caller can use errors.Is
// for control flow while errors.As still yields the status and body for the
// log line. Both are usually wanted at once.
func (e *HTTPError) Unwrap() error {
	switch {
	case e.Status == 403:
		return ErrNotApproved
	case e.Status == 404:
		return ErrNoConfig
	case e.Status != 401:
		return nil
	// The remaining cases are all 401; see the note above about matching on
	// the console's wording. Matched case-insensitively because the console
	// has three timestamp rejections and they do not agree on capitalisation:
	// "missing X-GW-Timestamp", "malformed X-GW-Timestamp" and "timestamp
	// outside the accepted window". All three point at the same header, and a
	// wrong clock is overwhelmingly the likeliest cause in production.
	case strings.Contains(strings.ToLower(e.Reason), "timestamp"):
		return ErrClockSkew
	case strings.Contains(strings.ToLower(e.Reason), "unknown gateway"):
		return ErrUnknownGateway
	default:
		return ErrUnauthorized
	}
}
