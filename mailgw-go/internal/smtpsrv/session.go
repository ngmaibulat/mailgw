package smtpsrv

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/uuidx"
)

// rcptState is one accepted recipient and whatever the ruleset has already
// decided about it.
type rcptState struct {
	addr  string
	index int

	// decision is set when routing resolved at RCPT. The walk only resolves
	// early if no higher-priority rule needed a later stage, so a cached
	// decision is guaranteed to equal the one DATA would produce.
	decision *ruleset.Decision

	// drop records a policy action that keeps the recipient accepted but stops
	// it being relayed.
	drop string

	// headers come from recipient-scoped policy rules and belong to this
	// recipient alone — putting them on the transaction would give every other
	// recipient a header its own rules never asked for.
	headers []ruleset.Header
}

// session holds per-connection state.
type session struct {
	b    *Backend
	conn *smtp.Conn
	log  *slog.Logger

	connUUID   uuidx.ID
	startedAt  time.Time
	remoteIP   netip.Addr
	remotePort int

	txnSeq    int
	tranCount int

	// rules is snapshotted once, so a SIGHUP mid-session cannot change the
	// policy a message is being evaluated against.
	rules *ruleset.Ruleset
	env   *ruleset.Env

	// connHeaders come from connect/helo policy and apply to every message on
	// this connection; txnHeaders are reset with each transaction.
	connHeaders []ruleset.Header
	txnHeaders  []ruleset.Header

	// per-transaction state, cleared by Reset
	txnUUID  uuidx.ID
	mailFrom string
	haveMail bool
	rcpts    []rcptState
	smtputf8 bool
	body8bit bool

	// connAccept and txnAccept record an `accept` action, which stops policy
	// evaluation without stopping routing. They are separate because the scope
	// of an accept follows the stage it fired at: one at connect or helo
	// covers the whole connection, one at mail or data covers only that
	// message.
	connAccept bool
	txnAccept  bool

	rcptAccept   int
	rcptReject   int
	rcptTempfail int
}

var _ smtp.Session = (*session)(nil)

func newSession(b *Backend, c *smtp.Conn, log *slog.Logger) *session {
	s := &session{
		b:         b,
		conn:      c,
		connUUID:  uuidx.New(),
		startedAt: time.Now(),
	}

	if b.Rules != nil {
		s.rules = b.Rules()
	}

	conn := &ruleset.ConnEnv{}
	if c != nil && c.Conn() != nil {
		if ta, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
			s.remoteIP = ta.AddrPort().Addr().Unmap()
			s.remotePort = ta.Port
		}
		if ta, ok := c.Conn().LocalAddr().(*net.TCPAddr); ok {
			conn.LocalIP = ta.AddrPort().Addr().Unmap()
			conn.LocalPort = ta.Port
		}
	}
	conn.RemoteIP, conn.RemotePort = s.remoteIP, s.remotePort

	s.env = &ruleset.Env{Stage: ruleset.StageConnect, Conn: conn, Helo: s.heloEnv()}

	s.log = log.With("conn_uuid", s.connUUID.String(), "remote", s.remoteIP.String())
	s.log.Debug("session opened", "helo", s.helo())
	return s
}

func (s *session) heloEnv() *ruleset.HeloEnv {
	h := &ruleset.HeloEnv{Name: s.helo()}
	if s.conn != nil {
		if st, ok := s.conn.TLSConnectionState(); ok {
			h.TLS = true
			h.TLSVersion = tlsVersionName(st.Version)
			h.TLSCipher = tls.CipherSuiteName(st.CipherSuite)
		}
	}
	return h
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS1.0"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	}
	return fmt.Sprintf("0x%04x", v)
}

// accepted reports whether policy evaluation has been short-circuited for the
// message in progress.
func (s *session) accepted() bool { return s.connAccept || s.txnAccept }

func (s *session) helo() string {
	if s.conn == nil {
		return ""
	}
	return s.conn.Hostname()
}

func (s *session) usingTLS() bool {
	if s.conn == nil {
		return false
	}
	_, ok := s.conn.TLSConnectionState()
	return ok
}

// greetPolicy runs the connect- and helo-stage policy rules. Returning an error
// from here answers EHLO with that code and closes the transaction before a
// sender is even named.
func (s *session) greetPolicy() error {
	for _, st := range []ruleset.Stage{ruleset.StageConnect, ruleset.StageHelo} {
		if s.connAccept {
			return nil
		}
		s.env.Stage = st
		res := s.rules.EvalPolicy(s.env, st)
		s.connHeaders = append(s.connHeaders, res.Headers...)
		if err := s.applyTerminal(res.Action, res.Rule, st); err != nil {
			return err
		}
	}
	return nil
}

// applyTerminal converts a message-scoped terminal policy action into an SMTP
// reply. `discard` and `quarantine` are rejected at compile time before MAIL,
// so only reject/tempfail/accept can arrive here.
func (s *session) applyTerminal(a *ruleset.Action, rule string, at ruleset.Stage) error {
	if a == nil {
		return nil
	}
	switch a.Kind {
	case ruleset.ActAccept:
		if at <= ruleset.StageHelo {
			s.connAccept = true
		} else {
			s.txnAccept = true
		}
		s.log.Debug("policy accept", "rule", rule, "stage", at.String())
		return nil
	case ruleset.ActReject, ruleset.ActTempfail:
		s.log.Info("policy refused message",
			"rule", rule, "stage", at.String(), "action", a.Kind, "code", a.Code)
		return smtpError(*a)
	}
	return nil
}

// smtpError renders a reject/tempfail action as a go-smtp reply.
func smtpError(a ruleset.Action) *smtp.SMTPError {
	enh, err := ruleset.ParseEnhanced(a.Enhanced)
	if err != nil {
		enh = [3]int{a.Code / 100, 0, 0}
	}
	return &smtp.SMTPError{
		Code:         a.Code,
		EnhancedCode: smtp.EnhancedCode{enh[0], enh[1], enh[2]},
		Message:      a.Message,
	}
}

// Mail begins a transaction. A null sender arrives as "".
func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	s.resetTxn()

	s.txnSeq++
	s.txnUUID = s.connUUID.Child(s.txnSeq)
	s.mailFrom = from
	s.haveMail = true

	mail := &ruleset.MailEnv{From: from}
	if opts != nil {
		s.smtputf8 = opts.UTF8
		s.body8bit = opts.Body == smtp.Body8BitMIME || opts.Body == smtp.BodyBinaryMIME
		mail.SizeDeclared = opts.Size
		mail.Body = string(opts.Body)
		mail.SMTPUTF8 = opts.UTF8
		mail.RequireTLS = opts.RequireTLS
	}

	// The helo env is rebuilt because STARTTLS may have been negotiated since
	// the session was created.
	s.env.Helo = s.heloEnv()
	s.env.Mail = mail
	s.env.Stage = ruleset.StageMail

	s.log.Debug("MAIL FROM", "txn_uuid", s.txnUUID.String(), "from", from)

	if s.accepted() {
		return nil
	}
	res := s.rules.EvalPolicy(s.env, ruleset.StageMail)
	s.txnHeaders = append(s.txnHeaders, res.Headers...)
	return s.applyTerminal(res.Action, res.Rule, ruleset.StageMail)
}

// Rcpt evaluates one recipient.
//
// mailgw/plugins/npFilter.js:73 returns OK for every recipient, so Haraka
// accepted any address and relied entirely on the connect-stage IP allowlist.
// That remains the behaviour when no rule says otherwise — but a rule now can,
// and it fires here rather than at the end of DATA.
func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.haveMail {
		return &smtp.SMTPError{
			Code:         503,
			EnhancedCode: smtp.EnhancedCode{5, 5, 1},
			Message:      "Error: need MAIL command",
		}
	}

	st := rcptState{addr: to, index: len(s.rcpts) + 1}

	// A per-recipient copy, so tags set while evaluating one recipient do not
	// leak into the next.
	renv := s.env.ForRcpt(&ruleset.RcptEnv{
		To: to, Index: st.index, CountSoFar: s.rcptAccept,
	})
	renv.Stage = ruleset.StageRcpt

	if !s.accepted() {
		res := s.rules.EvalPolicyRcpt(renv, ruleset.StageRcpt)
		st.headers = append(st.headers, res.Headers...)
		if res.Action != nil {
			drop, err := s.rcptTerminal(*res.Action, to, res.Rule, ruleset.StageRcpt)
			if err != nil {
				return err
			}
			st.drop = drop
		}
	}

	// Bind the route now if the rule walk can resolve without DATA-stage
	// facts. When it cannot, Route reports "undecided" and DATA decides.
	if d, ok := s.rules.Route(renv, ruleset.StageRcpt); ok {
		if st.drop == "" {
			drop, err := s.rcptTerminal(d.Action, to, d.Rule, ruleset.StageRcpt)
			if err != nil {
				return err
			}
			st.drop = drop
		}
		bound := d
		st.decision = &bound
	}

	s.rcpts = append(s.rcpts, st)
	s.rcptAccept++
	s.log.Debug("RCPT TO", "txn_uuid", s.txnUUID.String(), "to", to)
	return nil
}

// rcptTerminal handles a terminal action scoped to one recipient. It returns a
// drop reason when the recipient stays accepted but must not be relayed.
func (s *session) rcptTerminal(a ruleset.Action, addr, rule string, at ruleset.Stage) (string, error) {
	switch a.Kind {
	case ruleset.ActReject:
		s.rcptReject++
		s.log.Info("recipient rejected", "rcpt", addr, "rule", rule, "stage", at.String(), "code", a.Code)
		return "", smtpError(a)
	case ruleset.ActTempfail:
		s.rcptTempfail++
		s.log.Info("recipient deferred", "rcpt", addr, "rule", rule, "stage", at.String(), "code", a.Code)
		return "", smtpError(a)
	case ruleset.ActDiscard:
		s.log.Info("recipient discarded", "rcpt", addr, "rule", rule, "stage", at.String())
		return ruleset.ActDiscard, nil
	case ruleset.ActQuarantine:
		s.log.Info("recipient quarantined", "rcpt", addr, "rule", rule, "stage", at.String())
		return ruleset.ActQuarantine, nil
	}
	return "", nil
}

// Data spools the message, evaluates the data-stage rules, splits it per
// destination, and queues it.
//
// The returned *smtp.SMTPError carries code 250: go-smtp's dataErrorToStatus
// (conn.go:1257) passes an SMTPError's code through verbatim, which is the only
// way to control the success reply text. The text must contain "queued" and
// embed the transaction id in parentheses, because tests/smtp/src/smtp.ts:143
// scrapes it with /\(([0-9A-F-]+)(?:\.\d+)?\)/i.
func (s *session) Data(r io.Reader) error {
	if !s.haveMail {
		return &smtp.SMTPError{
			Code:         503,
			EnhancedCode: smtp.EnhancedCode{5, 5, 1},
			Message:      "Error: need MAIL command",
		}
	}
	if len(s.rcpts) == 0 {
		return &smtp.SMTPError{
			Code:         554,
			EnhancedCode: smtp.EnhancedCode{5, 5, 1},
			Message:      "Error: no valid recipients",
		}
	}

	dataStart := time.Now()

	// Matches the timing of mailgw/plugins/npData.js:9, which posts connection
	// info at hook_data.
	s.postConnection()

	// One pass: the trace header we prepend is part of what gets scanned, so
	// msg.received_count includes this hop.
	scan := newBodyScan(io.MultiReader(strings.NewReader(s.receivedHeader()), r))
	name, size, err := s.b.Spool.WriteBody(s.txnUUID.String(), scan)
	if err != nil {
		s.log.Error("cannot spool message", "txn_uuid", s.txnUUID.String(), "err", err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "Error: cannot queue message",
		}
	}

	headers := scan.headers()
	s.env.Msg = &ruleset.MsgEnv{
		Size:          size,
		LineCount:     scan.lines,
		ReceivedCount: len(headers["received"]),
		Headers:       headers,
		RcptCount:     len(s.rcpts),
		RcptDomains:   s.rcptDomains(),
		// Attachment facts stay empty until the MIME walk lands with the
		// attachment scanner. `check` warns about rules that read them.
	}
	s.env.Stage = ruleset.StageData

	msgDrop := ""
	if !s.accepted() {
		res := s.rules.EvalPolicy(s.env, ruleset.StageData)
		s.txnHeaders = append(s.txnHeaders, res.Headers...)
		if a := res.Action; a != nil {
			switch a.Kind {
			case ruleset.ActReject, ruleset.ActTempfail:
				s.log.Info("policy refused message at data",
					"rule", res.Rule, "action", a.Kind, "code", a.Code)
				// The body was spooled to be scanned; nothing will reference it.
				if err := s.b.Spool.RemoveBody(name); err != nil {
					s.log.Warn("cannot remove rejected body", "body", name, "err", err)
				}
				return smtpError(*a)
			case ruleset.ActDiscard, ruleset.ActQuarantine:
				msgDrop = a.Kind
				s.log.Info("policy dropped message", "rule", res.Rule, "action", a.Kind)
			}
		}
	}

	envelopes, failed, dropped := s.split(name, size, msgDrop)

	if len(envelopes) == 0 {
		if err := s.b.Spool.RemoveBody(name); err != nil {
			s.log.Warn("cannot remove unused body", "body", name, "err", err)
		}
		if len(failed) > 0 {
			// Parity with mailgw/plugins/npRoute.js:65, whose DENYSOFT
			// "No route found" is the default_action of a converted table.
			s.log.Warn("no destination for any recipient",
				"txn_uuid", s.txnUUID.String(), "rcpts", failedAddrs(failed))
			return smtpError(failed[0].action)
		}
		// Everything was discarded: the message is accepted and goes nowhere.
		s.tranCount++
		s.postQueue(dataStart, size)
		s.log.Info("accepted and dropped", "txn_uuid", s.txnUUID.String(), "dropped", dropped)
		return s.queuedReply()
	}

	if len(failed) > 0 {
		// Some recipients resolved to a refusal only after the message was
		// accepted, so there is no reply left to put them in. They are dropped
		// with a warning; generating a DSN for them is M5.
		s.log.Warn("recipients refused after acceptance, no DSN generated yet",
			"txn_uuid", s.txnUUID.String(), "rcpts", failedAddrs(failed))
	}

	for _, e := range envelopes {
		enqueue := s.b.Spool.Enqueue
		if e.Quarantined {
			enqueue = s.b.Spool.Quarantine
		}
		if err := enqueue(e.env); err != nil {
			s.log.Error("cannot enqueue", "uuid", e.env.UUID, "err", err)
			return &smtp.SMTPError{
				Code:         451,
				EnhancedCode: smtp.EnhancedCode{4, 3, 0},
				Message:      "Error: cannot queue message",
			}
		}
	}

	s.tranCount++
	s.postQueue(dataStart, size)

	if s.b.OnQueued != nil {
		queued := make([]*queue.Envelope, 0, len(envelopes))
		for _, e := range envelopes {
			if !e.Quarantined {
				queued = append(queued, e.env)
			}
		}
		if len(queued) > 0 {
			s.b.OnQueued(queued)
		}
	}

	s.log.Info("queued",
		"txn_uuid", s.txnUUID.String(),
		"envelopes", len(envelopes),
		"rcpts", len(s.rcpts),
		"bytes", size)

	return s.queuedReply()
}

func (s *session) queuedReply() error {
	return &smtp.SMTPError{
		Code:         250,
		EnhancedCode: smtp.EnhancedCode{2, 0, 0},
		Message:      fmt.Sprintf("Message queued (%s)", s.txnUUID),
	}
}

// outEnvelope pairs a built envelope with where it should be filed.
type outEnvelope struct {
	env         *queue.Envelope
	Quarantined bool
}

// failedRcpt is a recipient whose evaluation ended in a refusal.
type failedRcpt struct {
	addr   string
	action ruleset.Action
}

func failedAddrs(f []failedRcpt) []string {
	out := make([]string, 0, len(f))
	for _, x := range f {
		out = append(out, x.addr)
	}
	return out
}

// split evaluates each recipient to completion and groups the survivors into
// envelopes.
//
// Recipients that share a destination and the same set of prepended headers
// travel together; everything else becomes its own envelope. Haraka could not
// do this at all: npRoute.js:55 routed the whole message by rcpt_to[0], so mail
// addressed to two domains with different routes went entirely to the first
// recipient's relay.
func (s *session) split(bodyName string, size int64, msgDrop string) (out []outEnvelope, failed []failedRcpt, dropped int) {
	type bucket struct {
		group      string
		headers    []ruleset.Header
		quarantine bool
		tags       map[string]string
		rcpts      []string
	}
	buckets := map[string]*bucket{}
	var order []string

	for _, rs := range s.rcpts {
		drop := rs.drop
		if msgDrop != "" {
			drop = msgDrop
		}
		if drop == ruleset.ActDiscard {
			dropped++
			continue
		}

		rcptHeaders := rs.headers
		decision := rs.decision
		if decision == nil {
			renv := s.env.ForRcpt(&ruleset.RcptEnv{
				To: rs.addr, Index: rs.index, CountSoFar: len(s.rcpts),
			})
			renv.Stage = ruleset.StageData

			if !s.accepted() {
				res := s.rules.EvalPolicyRcpt(renv, ruleset.StageData)
				rcptHeaders = append(rcptHeaders, res.Headers...)
				if a := res.Action; a != nil {
					switch a.Kind {
					case ruleset.ActReject, ruleset.ActTempfail:
						failed = append(failed, failedRcpt{addr: rs.addr, action: *a})
						continue
					case ruleset.ActDiscard:
						dropped++
						continue
					case ruleset.ActQuarantine:
						drop = ruleset.ActQuarantine
					}
				}
			}

			// At DATA every rule is evaluable, so this always decides.
			d, ok := s.rules.Route(renv, ruleset.StageData)
			if !ok {
				failed = append(failed, failedRcpt{addr: rs.addr, action: ruleset.DefaultAction()})
				continue
			}
			decision = &d
		}

		group, isRelay := decision.Relay()
		if !isRelay {
			switch decision.Action.Kind {
			case ruleset.ActDiscard:
				dropped++
			case ruleset.ActQuarantine:
				// A quarantine route still needs somewhere to go if it is ever
				// released, but no rule named one; treat it as a drop.
				dropped++
			default:
				failed = append(failed, failedRcpt{addr: rs.addr, action: decision.Action})
			}
			continue
		}

		headers := make([]ruleset.Header, 0,
			len(s.connHeaders)+len(s.txnHeaders)+len(rcptHeaders)+len(decision.Headers))
		headers = append(headers, s.connHeaders...)
		headers = append(headers, s.txnHeaders...)
		headers = append(headers, rcptHeaders...)
		headers = append(headers, decision.Headers...)

		quarantine := drop == ruleset.ActQuarantine
		key := bucketKey(group, quarantine, headers)
		b, ok := buckets[key]
		if !ok {
			b = &bucket{group: group, headers: headers, quarantine: quarantine, tags: decision.Tags}
			buckets[key] = b
			order = append(order, key)
		}
		b.rcpts = append(b.rcpts, rs.addr)
	}

	// Deterministic envelope numbering, so the same message always produces
	// the same uuids — which is what makes the e2e assertions stable.
	sort.Strings(order)

	now := time.Now()
	for i, key := range order {
		b := buckets[key]
		rcpts := make([]queue.Recipient, 0, len(b.rcpts))
		for _, addr := range b.rcpts {
			rcpts = append(rcpts, queue.Recipient{Addr: addr, Status: queue.StatusPending})
		}
		out = append(out, outEnvelope{
			Quarantined: b.quarantine,
			env: &queue.Envelope{
				Version:  queue.EnvelopeVersion,
				UUID:     s.txnUUID.Child(i + 1).String(),
				TxnUUID:  s.txnUUID.String(),
				ConnUUID: s.connUUID.String(),
				Body:     bodyName,
				BodySize: size,
				MailFrom: s.mailFrom,
				Rcpts:    rcpts,
				RelayGrp: b.group,
				Prepend:  toQueueHeaders(b.headers),
				Tags:     b.tags,
				QueuedAt: now.UnixMilli(),
				NextAt:   now.UnixMilli(),
				SMTPUTF8: s.smtputf8,
				Body8Bit: s.body8bit,
			},
		})
	}
	return out, failed, dropped
}

// bucketKey groups recipients that are indistinguishable for delivery.
func bucketKey(group string, quarantine bool, headers []ruleset.Header) string {
	var b strings.Builder
	b.WriteString(group)
	b.WriteByte(0)
	if quarantine {
		b.WriteByte('q')
	}
	for _, h := range headers {
		b.WriteByte(0)
		b.WriteString(h.Name)
		b.WriteByte(':')
		b.WriteString(h.Value)
	}
	return b.String()
}

func toQueueHeaders(hs []ruleset.Header) []queue.Header {
	if len(hs) == 0 {
		return nil
	}
	out := make([]queue.Header, 0, len(hs))
	for _, h := range hs {
		out = append(out, queue.Header{
			Name:  sanitizeHeaderValue(h.Name),
			Value: sanitizeHeaderValue(h.Value),
		})
	}
	return out
}

func (s *session) rcptAddrs() []string {
	out := make([]string, 0, len(s.rcpts))
	for _, r := range s.rcpts {
		out = append(out, r.addr)
	}
	return out
}

// rcptDomains lists the distinct recipient domains, in first-seen order.
func (s *session) rcptDomains() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range s.rcpts {
		d := strings.ToLower(ruleset.Domain(r.addr))
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// receivedHeader is the trace header we add. go-smtp does not add one, and a
// message with no Received chain is both non-compliant and impossible to
// loop-detect.
func (s *session) receivedHeader() string {
	helo := s.helo()
	if helo == "" {
		helo = "unknown"
	}
	with := "ESMTP"
	if s.usingTLS() {
		with = "ESMTPS"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Received: from %s (%s)\r\n", sanitizeHeaderValue(helo), s.remoteIP.String())
	fmt.Fprintf(&b, "\tby %s (mailgw-go) with %s id %s\r\n", s.b.Cfg.Server.Hostname, with, s.txnUUID)
	fmt.Fprintf(&b, "\t; %s\r\n", time.Now().Format(time.RFC1123Z))
	// Marks which gateway produced the row, so Haraka and mailgw-go traffic can
	// be told apart while both run against one database.
	b.WriteString("X-NGM-Gateway: go\r\n")
	return b.String()
}

// sanitizeHeaderValue strips CR/LF so a hostile EHLO name cannot inject
// headers into the message we generate.
func sanitizeHeaderValue(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func (s *session) postConnection() {
	if s.b.Events == nil {
		return
	}
	s.b.Events.Send(events.Envelope{
		Kind: events.KindConnection,
		URL:  s.b.Cfg.Logging.URLConn,
		Body: events.Connection{
			UUID:              s.connUUID.String(),
			DT:                events.Millis(s.startedAt),
			HelloName:         s.helo(),
			RemoteAddr:        s.remoteIP.String(),
			RemotePort:        s.remotePort,
			RemoteIsLocal:     s.remoteIP.IsLoopback(),
			RemoteIsPrivate:   s.remoteIP.IsPrivate(),
			UsingTLS:          s.usingTLS(),
			TranCount:         s.tranCount,
			RcptCountAccept:   s.rcptAccept,
			RcptCountReject:   s.rcptReject,
			RcptCountTempfail: s.rcptTempfail,
		},
	})
}

func (s *session) postQueue(dataStart time.Time, size int64) {
	if s.b.Events == nil {
		return
	}
	s.b.Events.Send(events.Envelope{
		Kind: events.KindQueue,
		URL:  s.b.Cfg.Logging.URLQueue,
		Body: events.Queue{
			UUID:              s.txnUUID.String(),
			DT:                events.Millis(dataStart),
			Action:            "queue",
			Sender:            s.mailFrom,
			RcptList:          events.JoinAddrs(s.rcptAddrs()),
			RcptCountAccept:   s.rcptAccept,
			RcptCountReject:   s.rcptReject,
			RcptCountTempfail: s.rcptTempfail,
			DelayDataPost:     time.Since(dataStart).Seconds(),
			DataBytes:         size,
		},
	})
}

// Reset discards the current transaction. go-smtp then requires a fresh MAIL
// before RCPT, which is what the contract suite asserts.
func (s *session) Reset() { s.resetTxn() }

func (s *session) resetTxn() {
	s.txnUUID = ""
	s.mailFrom = ""
	s.haveMail = false
	s.rcpts = nil
	s.smtputf8 = false
	s.body8bit = false
	s.rcptAccept = 0
	s.rcptReject = 0
	s.rcptTempfail = 0
	s.txnHeaders = nil
	s.txnAccept = false

	if s.env != nil {
		s.env.Mail = nil
		s.env.Rcpt = nil
		s.env.Msg = nil
		s.env.Stage = ruleset.StageHelo
	}
}

func (s *session) Logout() error {
	s.log.Debug("session closed", "transactions", s.tranCount)
	return nil
}
