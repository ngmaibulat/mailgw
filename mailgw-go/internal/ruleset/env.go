package ruleset

import (
	"net/netip"
	"strings"
)

// Value is a field's value at match time.
//
// It is a small tagged union rather than `any` so that operators can be type
// checked when rules are compiled, and so matching allocates nothing on the hot
// path for the common scalar case.
type Value struct {
	// OK reports that the field is known. A field that is simply absent (no
	// such header, no TLS) is not an error — it just fails to match, except
	// under `empty`.
	OK   bool
	Kind Kind
	List bool

	S  string
	I  int64
	B  bool
	IP netip.Addr

	Elems []Value
}

func strVal(s string) Value { return Value{OK: true, Kind: KindString, S: s} }
func intVal(i int64) Value  { return Value{OK: true, Kind: KindInt, I: i} }
func boolVal(b bool) Value  { return Value{OK: true, Kind: KindBool, B: b} }
func ipVal(a netip.Addr) Value {
	return Value{OK: a.IsValid(), Kind: KindIP, IP: a}
}

func strList(ss []string) Value {
	v := Value{OK: true, Kind: KindString, List: true, Elems: make([]Value, 0, len(ss))}
	for _, s := range ss {
		v.Elems = append(v.Elems, strVal(s))
	}
	return v
}

func intList(is []int64) Value {
	v := Value{OK: true, Kind: KindInt, List: true, Elems: make([]Value, 0, len(is))}
	for _, i := range is {
		v.Elems = append(v.Elems, intVal(i))
	}
	return v
}

var missing = Value{}

// nonEmpty reports whether a value counts as present for `exists`.
func (v Value) nonEmpty() bool {
	if !v.OK {
		return false
	}
	if v.List {
		return len(v.Elems) > 0
	}
	switch v.Kind {
	case KindString:
		return v.S != ""
	case KindIP:
		return v.IP.IsValid()
	default:
		// A zero int and a false bool are values, not absences.
		return true
	}
}

// ConnEnv holds connection-stage facts.
type ConnEnv struct {
	RemoteIP   netip.Addr
	RemotePort int
	LocalIP    netip.Addr
	LocalPort  int
}

// HeloEnv holds facts available once the client has greeted and, if it is
// going to, authenticated.
type HeloEnv struct {
	Name          string
	TLS           bool
	TLSVersion    string
	TLSCipher     string
	AuthUser      string
	AuthMechanism string
}

// MailEnv holds the envelope sender and MAIL parameters.
type MailEnv struct {
	From         string
	SizeDeclared int64
	Body         string
	SMTPUTF8     bool
	RequireTLS   bool
	// IsDSN marks a message this gateway generated as a bounce. Inbound mail
	// with a null sender also reads as a DSN, per RFC 3464.
	IsDSN bool
}

// RcptEnv is the recipient currently being evaluated. Routing rules are
// evaluated once per recipient with this rebound, which is the whole point:
// npRoute.js:55 routed an entire message by rcpt_to[0].
type RcptEnv struct {
	To         string
	Index      int
	CountSoFar int
}

// Attachment is one MIME part considered an attachment.
type Attachment struct {
	Filename    string
	ContentType string
	MD5         string
	Disposition string
	Size        int64
}

// MsgEnv holds end-of-DATA facts.
type MsgEnv struct {
	Size          int64
	LineCount     int64
	ReceivedCount int
	MimePartCount int
	Headers       map[string][]string
	Attachments   []Attachment
	RcptCount     int
	RcptDomains   []string
}

// Env is the fact base a rule matches against. Sub-structs are nil until their
// stage is reached, so a rule that reaches past the current stage sees a
// missing field rather than a stale one.
type Env struct {
	Stage Stage
	Conn  *ConnEnv
	Helo  *HeloEnv
	Mail  *MailEnv
	Rcpt  *RcptEnv
	Msg   *MsgEnv

	// Tags accumulate from `tag` actions and are readable as `tag.<key>`.
	Tags map[string]string
}

// ForRcpt returns a shallow copy of the env bound to one recipient, with its
// own tag map.
//
// Rules are evaluated once per recipient, and a tag set while evaluating one
// recipient must not be visible while evaluating the next — otherwise the
// answer would depend on the order recipients happened to arrive in.
func (e *Env) ForRcpt(r *RcptEnv) *Env {
	c := *e
	c.Rcpt = r
	if e.Tags != nil {
		c.Tags = make(map[string]string, len(e.Tags))
		for k, v := range e.Tags {
			c.Tags[k] = v
		}
	}
	return &c
}

// SetTag records a tag, creating the map on first use.
func (e *Env) SetTag(k, v string) {
	if e.Tags == nil {
		e.Tags = map[string]string{}
	}
	e.Tags[k] = v
}

// Lookup resolves a field name to its current value.
//
// Field names are validated against the schema at compile time, so an unknown
// name here can only mean a caller built a predicate by hand.
func (e *Env) Lookup(name string) Value {
	if key, ok := strings.CutPrefix(name, "tag."); ok {
		v, present := e.Tags[key]
		if !present {
			return missing
		}
		return strVal(v)
	}
	if key, ok := strings.CutPrefix(name, "header."); ok {
		if e.Msg == nil {
			return missing
		}
		vals, present := e.Msg.Headers[strings.ToLower(key)]
		if !present {
			return missing
		}
		return strList(vals)
	}
	if key, ok := strings.CutPrefix(name, "header_count."); ok {
		if e.Msg == nil {
			return missing
		}
		return intVal(int64(len(e.Msg.Headers[strings.ToLower(key)])))
	}

	switch name {
	case "conn.remote_ip":
		if e.Conn == nil {
			return missing
		}
		return ipVal(e.Conn.RemoteIP)
	case "conn.remote_port":
		if e.Conn == nil {
			return missing
		}
		return intVal(int64(e.Conn.RemotePort))
	case "conn.local_ip":
		if e.Conn == nil {
			return missing
		}
		return ipVal(e.Conn.LocalIP)
	case "conn.local_port":
		if e.Conn == nil {
			return missing
		}
		return intVal(int64(e.Conn.LocalPort))
	case "conn.is_private":
		if e.Conn == nil {
			return missing
		}
		return boolVal(e.Conn.RemoteIP.IsPrivate())
	case "conn.is_loopback":
		if e.Conn == nil {
			return missing
		}
		return boolVal(e.Conn.RemoteIP.IsLoopback())

	case "helo.name":
		if e.Helo == nil {
			return missing
		}
		return strVal(e.Helo.Name)
	case "conn.tls":
		if e.Helo == nil {
			return missing
		}
		return boolVal(e.Helo.TLS)
	case "conn.tls_version":
		if e.Helo == nil {
			return missing
		}
		return strVal(e.Helo.TLSVersion)
	case "conn.tls_cipher":
		if e.Helo == nil {
			return missing
		}
		return strVal(e.Helo.TLSCipher)
	case "auth.user":
		if e.Helo == nil {
			return missing
		}
		return strVal(e.Helo.AuthUser)
	case "auth.mechanism":
		if e.Helo == nil {
			return missing
		}
		return strVal(e.Helo.AuthMechanism)

	case "mail.from":
		if e.Mail == nil {
			return missing
		}
		return strVal(e.Mail.From)
	case "mail.from_local":
		if e.Mail == nil {
			return missing
		}
		return strVal(LocalPart(e.Mail.From))
	case "mail.from_domain":
		if e.Mail == nil {
			return missing
		}
		return strVal(Domain(e.Mail.From))
	case "mail.is_null_sender":
		if e.Mail == nil {
			return missing
		}
		return boolVal(e.Mail.From == "")
	case "mail.is_dsn":
		if e.Mail == nil {
			return missing
		}
		return boolVal(e.Mail.IsDSN || e.Mail.From == "")
	case "mail.size_declared":
		if e.Mail == nil {
			return missing
		}
		return intVal(e.Mail.SizeDeclared)
	case "mail.body":
		if e.Mail == nil {
			return missing
		}
		return strVal(e.Mail.Body)
	case "mail.smtputf8":
		if e.Mail == nil {
			return missing
		}
		return boolVal(e.Mail.SMTPUTF8)
	case "mail.requiretls":
		if e.Mail == nil {
			return missing
		}
		return boolVal(e.Mail.RequireTLS)

	case "rcpt.to":
		if e.Rcpt == nil {
			return missing
		}
		return strVal(e.Rcpt.To)
	case "rcpt.local":
		if e.Rcpt == nil {
			return missing
		}
		return strVal(LocalPart(e.Rcpt.To))
	case "rcpt.domain":
		if e.Rcpt == nil {
			return missing
		}
		return strVal(Domain(e.Rcpt.To))
	case "rcpt.index":
		if e.Rcpt == nil {
			return missing
		}
		return intVal(int64(e.Rcpt.Index))
	case "txn.rcpt_count_so_far":
		if e.Rcpt == nil {
			return missing
		}
		return intVal(int64(e.Rcpt.CountSoFar))

	case "msg.size":
		if e.Msg == nil {
			return missing
		}
		return intVal(e.Msg.Size)
	case "msg.line_count":
		if e.Msg == nil {
			return missing
		}
		return intVal(e.Msg.LineCount)
	case "msg.received_count":
		if e.Msg == nil {
			return missing
		}
		return intVal(int64(e.Msg.ReceivedCount))
	case "msg.mime_part_count":
		if e.Msg == nil {
			return missing
		}
		return intVal(int64(e.Msg.MimePartCount))
	case "msg.has_attachment":
		if e.Msg == nil {
			return missing
		}
		return boolVal(len(e.Msg.Attachments) > 0)
	case "txn.rcpt_count":
		if e.Msg == nil {
			return missing
		}
		return intVal(int64(e.Msg.RcptCount))
	case "txn.rcpt_domains":
		if e.Msg == nil {
			return missing
		}
		return strList(e.Msg.RcptDomains)

	case "attachment.filename":
		return e.attachStrings(func(a Attachment) string { return a.Filename })
	case "attachment.content_type":
		return e.attachStrings(func(a Attachment) string { return a.ContentType })
	case "attachment.md5":
		return e.attachStrings(func(a Attachment) string { return a.MD5 })
	case "attachment.disposition":
		return e.attachStrings(func(a Attachment) string { return a.Disposition })
	case "attachment.size":
		if e.Msg == nil {
			return missing
		}
		out := make([]int64, 0, len(e.Msg.Attachments))
		for _, a := range e.Msg.Attachments {
			out = append(out, a.Size)
		}
		return intList(out)
	}

	return missing
}

func (e *Env) attachStrings(get func(Attachment) string) Value {
	if e.Msg == nil {
		return missing
	}
	out := make([]string, 0, len(e.Msg.Attachments))
	for _, a := range e.Msg.Attachments {
		out = append(out, get(a))
	}
	return strList(out)
}

// LocalPart returns the part of an address before the last "@", or the whole
// string when there is no "@".
func LocalPart(addr string) string {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return addr
	}
	return addr[:i]
}
