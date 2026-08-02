// Package queue implements the outbound spool: a crash-safe, on-disk queue of
// messages awaiting delivery.
package queue

import (
	"fmt"
	"strings"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/dsn"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/uuidx"
)

// EnvelopeVersion is the on-disk schema version, so a future format change can
// be detected rather than misread.
const EnvelopeVersion = 1

// Recipient statuses.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusPermfail  = "permfail"
	StatusExpired   = "expired"
)

// Header is a header to prepend to the delivered copy.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Recipient is one delivery target within an envelope.
type Recipient struct {
	Addr   string `json:"addr"`
	Status string `json:"status"`
	// RouteRule is the rule that chose this recipient's relay group. Recorded
	// per recipient because an envelope groups by relay group, so recipients
	// routed here by different rules share one.
	RouteRule string `json:"route_rule,omitempty"`
	LastCode  int    `json:"last_code,omitempty"`
	LastMsg   string `json:"last_msg,omitempty"`
	DSNSent   bool   `json:"dsn_sent,omitempty"`

	// OrigRcpt and OrigRcptType carry the sender's RFC 3461 ORCPT parameter
	// through to Original-Recipient in any notification about this recipient.
	// Both empty is the normal case, and means the sender did not send one.
	OrigRcpt     string `json:"orig_rcpt,omitempty"`
	OrigRcptType string `json:"orig_rcpt_type,omitempty"`

	// Notify holds the sender's RFC 3461 NOTIFY keywords, upper-cased. Nil
	// means the sender said nothing, which is deliberately NOT the same as
	// NOTIFY=FAILURE — see WantsDelayDSN.
	//
	// Kept as the sender's own words rather than resolved to a pair of booleans
	// at enqueue time, because an envelope buried in dead/ is the only surviving
	// record of what happened to a message: "the sender asked not to be told"
	// and "we decided not to tell them" must stay distinguishable there.
	Notify []string `json:"notify,omitempty"`
}

// RFC 3461 NOTIFY keywords.
const (
	NotifyNever   = "NEVER"
	NotifyDelay   = "DELAY"
	NotifyFailure = "FAILURE"
	NotifySuccess = "SUCCESS"
)

// bodyOwnedBy reports whether an envelope with this uuid may reference this
// body file.
//
// A body is named "<owner>.eml", and an envelope may reference it when it IS
// that owner or descends from it. The second case is the ordinary one — a
// transaction's body shared by the envelopes it split into — and the first is
// what lets a notification own a body of its own: two notifications about the
// same failed envelope (a failure and a relayed report in one attempt, or a
// delay warning and a later failure) would otherwise both be named for their
// parent and the second would overwrite the first's bytes on disk.
//
// Shared by Envelope.validate and Spool.bodyReferenced deliberately: the second
// uses this as a shortcut to avoid opening every candidate file, and if the two
// ever disagreed the disagreement would delete a live body.
func bodyOwnedBy(body, uuid string) bool {
	owner := strings.TrimSuffix(body, ".eml")
	return uuid == owner || strings.HasPrefix(uuid, owner+".")
}

// Pending reports whether this recipient still needs a delivery attempt.
func (r Recipient) Pending() bool { return r.Status == StatusPending || r.Status == "" }

func (r Recipient) notifyHas(k string) bool {
	for _, n := range r.Notify {
		if strings.EqualFold(n, k) {
			return true
		}
	}
	return false
}

// WantsFailureDSN reports whether a permanent failure may be reported to the
// sender.
//
// RFC 3461 §4.1: with no NOTIFY parameter the default is FAILURE, so silence
// means yes. NEVER needs no special case — it may not co-occur with another
// keyword, so the FAILURE test already excludes it.
func (r Recipient) WantsFailureDSN() bool {
	return len(r.Notify) == 0 || r.notifyHas(NotifyFailure)
}

// WantsDelayDSN reports whether a delay warning may be sent.
//
// RFC 3461 §4.1 says the absence of DELAY "in a NOTIFY parameter" forbids a
// delayed DSN. Read together with the FAILURE default that would switch off
// delay_warning_after for essentially all mail, since almost nothing sends
// NOTIFY at all — a large invisible behaviour change dressed as conformance.
//
// So the sender is taken at its word only when it said something: no NOTIFY
// keeps this gateway's configured behaviour, and a NOTIFY that names its
// keywords and leaves DELAY out suppresses the warning. Same reading Postfix
// applies.
func (r Recipient) WantsDelayDSN() bool {
	return len(r.Notify) == 0 || r.notifyHas(NotifyDelay)
}

// WantsSuccessDSN reports whether the sender asked to be told when this
// recipient was handed to the next hop. Never the default — a success
// notification nobody asked for is unsolicited mail.
func (r Recipient) WantsSuccessDSN() bool { return r.notifyHas(NotifySuccess) }

// WantsDSN answers for a whole report kind, so the collection loops do not each
// re-derive which predicate goes with which notification.
func (r Recipient) WantsDSN(k dsn.Kind) bool {
	switch k {
	case dsn.KindDelayed:
		return r.WantsDelayDSN()
	case dsn.KindRelayed:
		return r.WantsSuccessDSN()
	}
	return r.WantsFailureDSN()
}

// Envelope is one unit of delivery work: a message body plus the recipients
// that share a relay group and header set.
//
// A message with recipients routed to different relay groups is split into
// several envelopes at enqueue time, each with its own UUID (txn.1, txn.2, ...).
// This is what fixes Haraka's rcpt_to[0] limitation, where the whole message
// followed the route of its first recipient (mailgw/plugins/npRoute.js:55).
type Envelope struct {
	Version int `json:"v"`

	UUID     string `json:"uuid"`      // X.1.1 — the Delivery row id
	TxnUUID  string `json:"txn_uuid"`  // X.1   — the Transaction row id
	ConnUUID string `json:"conn_uuid"` // X     — the Connection row id

	Body     string `json:"body"` // filename under data/
	BodySize int64  `json:"body_size"`

	MailFrom string            `json:"mail_from"` // "" is a null sender
	Rcpts    []Recipient       `json:"rcpts"`
	RelayGrp string            `json:"relay_group"`
	Prepend  []Header          `json:"prepend_headers,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`

	QueuedAt int64 `json:"queued_at_ms"`
	Attempts int   `json:"attempts"`
	NextAt   int64 `json:"next_at_ms"`

	LastErr string `json:"last_err,omitempty"`

	IsDSN       bool `json:"is_dsn,omitempty"`
	SMTPUTF8    bool `json:"smtputf8,omitempty"`
	Body8Bit    bool `json:"body_8bit,omitempty"`
	DelayWarned bool `json:"delay_warned,omitempty"`

	// DSNRet is the sender's RFC 3461 RET= request: "FULL", "HDRS", or empty
	// for "the sender did not say", which falls back to dsn.return.
	DSNRet string `json:"dsn_ret,omitempty"`
	// DSNEnvID is the sender's ENVID=, already xtext-decoded by go-smtp. It is
	// the only value Original-Envelope-Id may carry.
	DSNEnvID string `json:"dsn_envid,omitempty"`

	// DSNSeq counts the notifications this envelope has produced, and numbers
	// the next one. An envelope can bounce more than once — a delay warning at
	// four hours and a failure at four days — and both would otherwise be
	// numbered .1, giving them the same uuid and therefore the same queue
	// filename, where the second would silently overwrite the first.
	//
	// New fields here must be omitempty additions at version 1. Bumping
	// EnvelopeVersion makes validate reject every envelope already on disk, and
	// Claim answers that by moving them to dead/ — an upgrade would park the
	// entire live queue.
	DSNSeq int `json:"dsn_seq,omitempty"`
}

// PrependBlock renders the headers an `add_header` action asked for, ready to
// be written ahead of the stored body. Empty when there are none.
//
// The headers land above the message's own, which is where a trace header
// belongs and keeps the body itself immutable and shareable between the
// envelopes of one transaction.
func (e *Envelope) PrependBlock() string {
	if len(e.Prepend) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range e.Prepend {
		// Values are already sanitised when the envelope is built; this is the
		// second line of defence against a header-injection payload reaching
		// the wire.
		b.WriteString(stripCRLF(h.Name))
		b.WriteString(": ")
		b.WriteString(stripCRLF(h.Value))
		b.WriteString("\r\n")
	}
	return b.String()
}

func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// PendingRcpts returns the recipients still awaiting delivery.
func (e *Envelope) PendingRcpts() []Recipient {
	var out []Recipient
	for _, r := range e.Rcpts {
		if r.Pending() {
			out = append(out, r)
		}
	}
	return out
}

// Done reports whether every recipient has reached a terminal state.
func (e *Envelope) Done() bool { return len(e.PendingRcpts()) == 0 }

// RcptAddrs lists every recipient address, in order.
func (e *Envelope) RcptAddrs() []string {
	out := make([]string, 0, len(e.Rcpts))
	for _, r := range e.Rcpts {
		out = append(out, r.Addr)
	}
	return out
}

// SetStatus records the outcome for one recipient.
func (e *Envelope) SetStatus(addr, status string, code int, msg string) {
	for i := range e.Rcpts {
		if strings.EqualFold(e.Rcpts[i].Addr, addr) {
			e.Rcpts[i].Status = status
			e.Rcpts[i].LastCode = code
			e.Rcpts[i].LastMsg = msg
			return
		}
	}
}

// validate checks the invariants that the spool relies on. The UUID checks
// matter beyond tidiness: these values become filenames and are interpolated
// into SMTP replies.
func (e *Envelope) validate() error {
	if e.Version != EnvelopeVersion {
		return fmt.Errorf("envelope %q: unsupported version %d (want %d)", e.UUID, e.Version, EnvelopeVersion)
	}
	for _, f := range []struct {
		name string
		id   string
	}{
		{"uuid", e.UUID},
		{"txn_uuid", e.TxnUUID},
		{"conn_uuid", e.ConnUUID},
	} {
		if !uuidx.ID(f.id).Valid() {
			return fmt.Errorf("envelope %q: %s %q is not a valid identifier", e.UUID, f.name, f.id)
		}
	}
	if !strings.HasPrefix(e.UUID, e.TxnUUID+".") {
		return fmt.Errorf("envelope %q: uuid must extend txn_uuid %q", e.UUID, e.TxnUUID)
	}
	if !strings.HasPrefix(e.TxnUUID, e.ConnUUID+".") {
		return fmt.Errorf("envelope %q: txn_uuid %q must extend conn_uuid %q", e.UUID, e.TxnUUID, e.ConnUUID)
	}
	if e.Body == "" {
		return fmt.Errorf("envelope %q: missing body reference", e.UUID)
	}
	if strings.ContainsAny(e.Body, `/\`) || strings.Contains(e.Body, "..") {
		return fmt.Errorf("envelope %q: body reference %q must be a bare filename", e.UUID, e.Body)
	}
	// An envelope may only reference a body named for itself or for an ancestor.
	// That is what lets Spool.gcBody rule an envelope out as a referrer from its
	// FILENAME; without the invariant that shortcut is unsound in the direction
	// that deletes a live body, which loses mail.
	if !bodyOwnedBy(e.Body, e.UUID) {
		return fmt.Errorf("envelope %q: body %q belongs to another transaction",
			e.UUID, e.Body)
	}
	if len(e.Rcpts) == 0 {
		return fmt.Errorf("envelope %q: no recipients", e.UUID)
	}
	if e.RelayGrp == "" {
		return fmt.Errorf("envelope %q: no relay group", e.UUID)
	}
	return nil
}
