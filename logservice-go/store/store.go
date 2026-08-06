// Package store writes the audit rows and reads the attachment blocklist.
//
// Every INSERT here names its columns explicitly and passes every value as a
// placeholder. Two details are carried over from the Bun service and are part of
// the wire contract rather than preferences:
//
//   - `dt` arrives as epoch MILLISECONDS and is stored with
//     FROM_UNIXTIME(dt / 1000). Sending seconds would land every row in 1970.
//   - `createdAt` and `updatedAt` are filled by the application with NOW().
//     The columns are DATETIME NOT NULL with no database default, so omitting
//     them fails the insert.
//
// # Reads are not here
//
// The search path lives in package query, because it builds its SQL from
// caller-supplied JSON and that construction is the thing worth keeping in one
// reviewable place. This package only ever issues statements that are compile-
// time constants.
//
// # Exported, not internal, because logservice-fiber shares it
//
// M23 put a second implementation beside this one. The two differ in their HTTP
// layer and in nothing else, which is the only reason comparing them means
// anything — so everything below HTTP lives here and is imported by both.
// internal/api is what stays internal. Do not add an HTTP type to this package.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/ngmaibulat/mailgw/logservice-go/validate"
)

// Store writes to the log tables.
type Store struct {
	DB *sql.DB
}

// nullable turns an absent-or-empty optional into a SQL NULL.
//
// NULL is the honest record of "the sender did not tell us", and it is what
// every row written before migrations 023 and 025 holds. Writing "" instead
// would make a gateway that omitted the field indistinguishable from one that
// sent an empty string.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Connection is the body of POST /api/connection.
//
// The mixed casing is required, not stylistic: the Bun handler reads camelCase
// remoteAddr/remotePort and snake_case hello_name/remote_host/remote_is_local/
// using_tls, and mailgw-go's payload struct was written against that. Renaming
// any of these silently produces a row of nulls, because this endpoint defaults
// every absent field instead of rejecting it.
//
// Pointers, so an absent field is distinguishable from a present zero and the
// defaults below are applied exactly where the Bun handler's `??` applied them.
// Haraka also sends `state` and `pipelining`; there is no column for either and
// they are ignored, as they always have been.
type Connection struct {
	UUID       *string  `json:"uuid"`
	DT         *float64 `json:"dt"`
	Encoding   *string  `json:"encoding"`
	HelloName  *string  `json:"hello_name"`
	RemoteAddr *string  `json:"remoteAddr"`
	RemotePort *float64 `json:"remotePort"`
	RemoteHost *string  `json:"remote_host"`
	RemoteInfo *string  `json:"remote_info"`

	RemoteIsLocal   *bool `json:"remote_is_local"`
	RemoteIsPrivate *bool `json:"remote_is_private"`
	UsingTLS        *bool `json:"using_tls"`

	TranCount         *float64 `json:"tran_count"`
	RcptCountAccept   *float64 `json:"rcpt_count_accept"`
	RcptCountTempfail *float64 `json:"rcpt_count_tempfail"`
	RcptCountReject   *float64 `json:"rcpt_count_reject"`

	Gateway *string `json:"gateway"`
}

const insertConnection = "INSERT INTO `Connection` " +
	"(`uuid`, `dt`, `encoding`, `hello_name`, `remoteAddr`, `remotePort`, " +
	" `remote_host`, `remote_info`, `remote_is_local`, `remote_is_private`, " +
	" `using_tls`, `tran_count`, `rcpt_count_accept`, `rcpt_count_tempfail`, " +
	" `rcpt_count_reject`, `gateway`, `createdAt`, `updatedAt`) " +
	"VALUES (?, FROM_UNIXTIME(? / 1000), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())"

// InsertConnection writes a connect-stage event.
//
// It must have committed before the handler replies: tests/api/logservice.e2e.test.ts
// searches for the row immediately after the POST returns, and the gateway's
// audit contract assumes a 200 means stored.
func (s Store) InsertConnection(ctx context.Context, c Connection) error {
	_, err := s.DB.ExecContext(ctx, insertConnection,
		strOrNil(c.UUID),
		numOrNil(c.DT),
		strOrNil(c.Encoding),
		strOrNil(c.HelloName),
		strOrNil(c.RemoteAddr),
		numOrNil(c.RemotePort),
		strOrNil(c.RemoteHost),
		strOrNil(c.RemoteInfo),
		boolDefault(c.RemoteIsLocal),
		boolDefault(c.RemoteIsPrivate),
		boolDefault(c.UsingTLS),
		numDefault(c.TranCount),
		numDefault(c.RcptCountAccept),
		numDefault(c.RcptCountTempfail),
		numDefault(c.RcptCountReject),
		strOrNil(c.Gateway),
	)
	if err != nil {
		return fmt.Errorf("insert connection: %w", err)
	}
	return nil
}

// Queue is the body of POST /api/queue, stored as a Transaction row.
//
// Note the Bun handler does NOT also write a Connection row here — the
// connect-stage row is already written by /api/connection, and the double insert
// was removed (the commented-out call at logservice/src/routes/api.ts:33 is the
// scar). Writing one here would duplicate every connection.
//
// RcptList is a comma-joined string on this endpoint, unlike the delivery
// payload where the same-named field must hold exactly one address.
type Queue struct {
	UUID     *string  `json:"uuid"`
	DT       *float64 `json:"dt"`
	Action   *string  `json:"action"`
	Encoding *string  `json:"encoding"`
	Sender   *string  `json:"sender"`
	RcptList *string  `json:"rcpt_list"`

	RcptCountAccept   *float64 `json:"rcpt_count_accept"`
	RcptCountTempfail *float64 `json:"rcpt_count_tempfail"`
	RcptCountReject   *float64 `json:"rcpt_count_reject"`

	DelayDataPost *float64 `json:"delay_data_post"`
	DataBytes     *float64 `json:"data_bytes"`
	MimePartCount *float64 `json:"mime_part_count"`

	Gateway *string `json:"gateway"`
}

const insertTransaction = "INSERT INTO `Transaction` " +
	"(`uuid`, `dt`, `action`, `encoding`, `sender`, `rcpt_list`, " +
	" `rcpt_count_accept`, `rcpt_count_tempfail`, `rcpt_count_reject`, " +
	" `delay_data_post`, `data_bytes`, `mime_part_count`, `gateway`, " +
	" `createdAt`, `updatedAt`) " +
	"VALUES (?, FROM_UNIXTIME(? / 1000), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())"

// InsertTransaction writes a queue event.
func (s Store) InsertTransaction(ctx context.Context, q Queue) error {
	_, err := s.DB.ExecContext(ctx, insertTransaction,
		strOrNil(q.UUID),
		numOrNil(q.DT),
		strOrNil(q.Action),
		strOrNil(q.Encoding),
		strOrNil(q.Sender),
		strOrNil(q.RcptList),
		numDefault(q.RcptCountAccept),
		numDefault(q.RcptCountTempfail),
		numDefault(q.RcptCountReject),
		numOrNil(q.DelayDataPost),
		numOrNil(q.DataBytes),
		numOrNil(q.MimePartCount),
		strOrNil(q.Gateway),
	)
	if err != nil {
		return fmt.Errorf("insert transaction: %w", err)
	}
	return nil
}

const insertDelivery = "INSERT INTO `Delivery` " +
	"(`uuid`, `dt`, `sender`, `rcpt_domain`, `rcpt_list`, `rcpt_accepted`, " +
	" `tls_forced`, `tls`, `auth`, `host`, `ip`, `port`, `response`, `delay`, " +
	" `gateway`, `route_rule`, `createdAt`, `updatedAt`) " +
	"VALUES (?, FROM_UNIXTIME(? / 1000), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())"

// InsertDelivery writes one recipient's delivery outcome.
//
// One row per recipient: the gateway emits one event each, which is what makes
// rcpt_list and rcpt_accepted single addresses here.
func (s Store) InsertDelivery(ctx context.Context, d *validate.Delivery) error {
	// `port` is a digit string on the wire and an INT column. Parsing it here
	// rather than handing MariaDB the string keeps the coercion visible: the
	// validator has already proved it is all digits, so an error is impossible
	// except for a value wider than int64, which is not a port.
	port, err := strconv.ParseInt(*d.Port, 10, 64)
	if err != nil {
		return fmt.Errorf("insert delivery: port %q is not an integer: %w", *d.Port, err)
	}

	_, err = s.DB.ExecContext(ctx, insertDelivery,
		*d.UUID,
		*d.DT,
		*d.Sender,
		*d.RcptDomain,
		*d.RcptList,
		*d.RcptAccepted,
		*d.TLSForced,
		*d.TLS,
		*d.Auth,
		*d.Host,
		*d.IP,
		port,
		*d.Response,
		*d.Delay,
		nullable(validate.Str(d.Gateway)),
		nullable(validate.Str(d.RouteRule)),
	)
	if err != nil {
		return fmt.Errorf("insert delivery: %w", err)
	}
	return nil
}

// Attachment is one descriptor from a POST /filter/md5 body.
//
// Every field is optional. The Haraka plugin and mailgw-go both send all five,
// but a body that omits one must not fail the scan: refusing would become an
// SMTP 451 and defer real mail.
type Attachment struct {
	MD5         *string  `json:"md5"`
	ContentType *string  `json:"contentType"`
	Filename    *string  `json:"filename"`
	Size        *float64 `json:"size"`
	TxnUUID     *string  `json:"txn_uuid"`
}

const insertHashLookup = "INSERT INTO `HashLookups` " +
	"(`txn_uuid`, `md5`, `contentType`, `filename`, `size`, `action`, " +
	" `createdAt`, `updatedAt`) VALUES (?, ?, ?, ?, ?, ?, NOW(), NOW())"

// BlockedMD5s returns which of the given digests are on the blocklist.
//
// The IN list is built from a placeholder per digest, never from the values, so
// an md5 field containing SQL is a parameter and nothing more.
func (s Store) BlockedMD5s(ctx context.Context, md5s []string) (map[string]struct{}, error) {
	blocked := make(map[string]struct{})
	if len(md5s) == 0 {
		return blocked, nil
	}

	placeholders := strings.Repeat("?,", len(md5s))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(md5s))
	for i, m := range md5s {
		args[i] = m
	}

	rs, err := s.DB.QueryContext(ctx,
		"SELECT `md5` FROM `BlockMD5s` WHERE `md5` IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("look up blocked md5s: %w", err)
	}
	defer func() { _ = rs.Close() }()

	for rs.Next() {
		var m string
		if err := rs.Scan(&m); err != nil {
			return nil, fmt.Errorf("look up blocked md5s: %w", err)
		}
		blocked[m] = struct{}{}
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("look up blocked md5s: %w", err)
	}
	return blocked, nil
}

// RecordLookup writes one HashLookups row.
//
// Every attachment is recorded, including the ones that were allowed and the
// ones that carried no digest at all — those land with "" for txn_uuid and md5,
// matching the Bun service's `?? ""`. The table is the record of what was
// scanned, not only of what was blocked.
func (s Store) RecordLookup(ctx context.Context, a Attachment, action string) error {
	_, err := s.DB.ExecContext(ctx, insertHashLookup,
		strOrEmpty(a.TxnUUID),
		strOrEmpty(a.MD5),
		strOrNil(a.ContentType),
		strOrNil(a.Filename),
		numOrNil(a.Size),
		action,
	)
	if err != nil {
		return fmt.Errorf("record hash lookup: %w", err)
	}
	return nil
}

// The `??` defaults from logservice/src/routes/api.ts, one helper each so the
// difference between "NULL when absent" and "0 when absent" stays explicit at
// every call site. The Bun handler defaults the counters to 0 and everything
// else to null, and a grid that shows 0 where the old one showed blank is a
// visible change.

func strOrNil(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func numOrNil(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func numDefault(p *float64) any {
	if p == nil {
		return 0
	}
	return *p
}

func boolDefault(p *bool) any {
	if p == nil {
		return false
	}
	return *p
}
