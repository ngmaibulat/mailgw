// Package events ships audit records to the logservice API.
//
// The payload shapes here are a contract with logservice/src/routes/api.ts and
// logservice/src/validation/delivery.ts. Two of the three endpoints do no
// validation at all and default missing fields, but /api/delivery is strictly
// validated with zod and answers 400 on any deviation — so the field names,
// casing, and JSON types below are load-bearing and covered by golden tests.
package events

import (
	"strings"
	"time"
)

// Kind identifies which endpoint a payload belongs to.
type Kind string

const (
	KindConnection Kind = "connection"
	KindQueue      Kind = "queue"
	KindDelivery   Kind = "delivery"
)

// Millis renders t as epoch milliseconds. Every dt field is in milliseconds:
// logservice stores them with FROM_UNIXTIME(dt / 1000).
func Millis(t time.Time) int64 { return t.UnixMilli() }

// Connection is the body of POST /api/connection.
//
// The mixed casing is deliberate and required: logservice's toConnectionRow
// (api.ts:44-62) reads camelCase remoteAddr/remotePort but snake_case
// hello_name/remote_host/remote_is_local/using_tls. Renaming any of these
// silently produces a row of nulls, because that handler defaults every absent
// field instead of rejecting it.
//
// Haraka also sends `state` and `pipelining`; logservice has no column for
// either and drops them, so they are omitted here.
type Connection struct {
	UUID       string `json:"uuid"`
	DT         int64  `json:"dt"` // epoch milliseconds
	Encoding   string `json:"encoding"`
	HelloName  string `json:"hello_name"`
	RemoteAddr string `json:"remoteAddr"`
	RemotePort int    `json:"remotePort"`
	RemoteHost string `json:"remote_host"`
	RemoteInfo string `json:"remote_info"`

	RemoteIsLocal   bool `json:"remote_is_local"`
	RemoteIsPrivate bool `json:"remote_is_private"`
	UsingTLS        bool `json:"using_tls"`

	TranCount         int `json:"tran_count"`
	RcptCountAccept   int `json:"rcpt_count_accept"`
	RcptCountTempfail int `json:"rcpt_count_tempfail"`
	RcptCountReject   int `json:"rcpt_count_reject"`

	// Gateway names the box that wrote this row. omitempty so a build that
	// cannot resolve a label produces the same JSON it always did, and so
	// existing golden tests keep their field counts.
	Gateway string `json:"gateway,omitempty"`
}

// Queue is the body of POST /api/queue, which logservice stores as a
// Transaction row.
//
// Note RcptList is a comma-joined string here — unlike the delivery payload,
// where the same-named field must hold exactly one address.
type Queue struct {
	UUID     string `json:"uuid"`
	DT       int64  `json:"dt"` // epoch milliseconds
	Action   string `json:"action"`
	Encoding string `json:"encoding"`
	Sender   string `json:"sender"`
	RcptList string `json:"rcpt_list"`

	RcptCountAccept   int `json:"rcpt_count_accept"`
	RcptCountTempfail int `json:"rcpt_count_tempfail"`
	RcptCountReject   int `json:"rcpt_count_reject"`

	DelayDataPost float64 `json:"delay_data_post"`
	DataBytes     int64   `json:"data_bytes"`
	MimePartCount int     `json:"mime_part_count"`

	Gateway string `json:"gateway,omitempty"`
}

// Delivery is the body of POST /api/delivery — the only strictly validated
// endpoint. Every field is required; there are no optionals and no defaults.
//
// Two fields fix defects in the Haraka plugin:
//
//   - RcptList and RcptAccepted carry exactly ONE address. npLogDelivery.js:37
//     sends getAddrList(ok_recips), a comma-joined list, which fails the
//     single-email zod check — so today every multi-recipient delivery is
//     rejected with a 400 and lost, unnoticed because the POST is
//     fire-and-forget with no response check. We emit one event per recipient.
//
//   - Port is a JSON string. portSchema is z.string().regex(/^[0-9]+$/), so a
//     numeric port is a 400.
type Delivery struct {
	UUID         string  `json:"uuid"`
	DT           int64   `json:"dt"` // epoch milliseconds
	Sender       string  `json:"sender"`
	RcptDomain   string  `json:"rcpt_domain"`
	RcptList     string  `json:"rcpt_list"`     // exactly one address
	RcptAccepted string  `json:"rcpt_accepted"` // exactly one address
	TLSForced    bool    `json:"tls_forced"`
	TLS          bool    `json:"tls"`
	Auth         bool    `json:"auth"`
	Host         string  `json:"host"`
	IP           string  `json:"ip"`
	Port         string  `json:"port"` // string, not number
	Response     string  `json:"response"`
	Delay        float64 `json:"delay"` // seconds

	Gateway string `json:"gateway,omitempty"`
	// RouteRule names the rule that chose this recipient's relay group, so the
	// log tables can answer "why did this go there?".
	//
	// It is per RECIPIENT rather than per transaction: routing is evaluated per
	// recipient, and two recipients of one message can be sent to the same
	// group by different rules. A single value on the Transaction row would be
	// either ambiguous or lossy, so it lives only here.
	RouteRule string `json:"route_rule,omitempty"`
}

// Envelope pairs a payload with its destination endpoint.
type Envelope struct {
	Kind Kind
	URL  string
	Body any
}

// JoinAddrs comma-joins addresses the way mailgw/plugins/functions.js:134
// (getAddrList) does: no spaces, no trailing comma, "" for an empty list.
func JoinAddrs(addrs []string) string { return strings.Join(addrs, ",") }

// Addr renders an address the way functions.js:128 (getAddr) does: a null
// sender becomes the empty string rather than "null" or "<>".
func Addr(local, domain string) string {
	if local == "" && domain == "" {
		return ""
	}
	return local + "@" + domain
}

// DomainOf returns the part after the last "@", or "" when there is none.
func DomainOf(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return ""
	}
	return addr[i+1:]
}
