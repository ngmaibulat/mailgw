package smtpsrv

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/dsn"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/uuidx"
)

// rcptState is one accepted recipient and whatever the ruleset has already
// decided about it.
type rcptState struct {
	addr  string
	index int

	// The sender's RFC 3461 per-recipient parameters, carried through to the
	// queued envelope so any notification about this recipient can honour them.
	orcpt     string
	orcptType string
	notify    []string

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

	// tags is the tag map as it stood after this recipient's RCPT-stage
	// evaluation. It is kept so the data-stage pass can carry those tags
	// forward: the per-recipient env is a copy, so without this a `tag` set at
	// RCPT would be invisible to a rule reading `tag.*` at DATA.
	tags map[string]string
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

	// startedTLS records whether this session was already encrypted when it was
	// created. It exists only to tell an ordinary logout from the one go-smtp
	// performs on the pre-upgrade session after STARTTLS — see Logout.
	startedTLS bool
	// connPosted records that a Connection event has already been sent for this
	// session, so Logout can post for a connection that never reached DATA
	// without duplicating one that did.
	connPosted bool

	// authUser and authMech record a successful AUTH. They live on the session
	// rather than on env.Helo because Mail rebuilds env.Helo from heloEnv() —
	// writing the HeloEnv directly would be silently discarded at MAIL. They are
	// connection-scoped: resetTxn deliberately leaves them alone, matching
	// go-smtp, which keeps c.didAuth across RSET and clears it only on a STARTTLS
	// upgrade (which discards this whole session anyway).
	authUser string
	authMech string

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
	// The sender's RFC 3461 message-scoped parameters, cleared by resetTxn like
	// every other transaction-scoped field.
	//
	// go-smtp always hands Mail a non-nil *MailOptions (conn.go:346), so both
	// are in fact rewritten on every MAIL and the clearing is belt and braces —
	// the same position smtputf8 and body8bit are in. It is kept because the
	// invariant that matters is "these describe the current transaction", and
	// resting that on a library's habit of never passing nil is a worse bet
	// than two assignments.
	dsnRet   string
	dsnEnvID string

	// Message-authentication results for this transaction. Kept beside the env
	// copies because the headers and the DMARC evaluation need the whole result
	// — the domain SPF was checked for, the reason a signature failed — where
	// the rule fields only need the verdict. Cleared by resetTxn: SPF is
	// evaluated per MAIL, so carrying one transaction's answer into the next
	// would report a result for a sender that was never checked.
	spf   msgauth.SPFResult
	dkim  msgauth.DKIMResult
	dmarc msgauth.DMARCResult

	// connAccept and txnAccept record an `accept` action, which stops policy
	// evaluation without stopping routing. They are separate because the scope
	// of an accept follows the stage it fired at: one at connect or helo
	// covers the whole connection, one at mail or data covers only that
	// message.
	connAccept bool
	txnAccept  bool

	// msgDrop records a message-scoped `discard` or `quarantine` decided
	// before DATA. Data seeds its own decision from it, so a rule that fires
	// at MAIL has the same effect as one that fires at DATA.
	msgDrop string

	rcptAccept   int
	rcptReject   int
	rcptTempfail int

	// The same three counts for the whole connection. resetTxn clears the
	// per-transaction ones — which the queue event wants, since it describes one
	// message — but a connection whose recipients were all rejected and then RSET
	// would otherwise report zeroes, or nothing at all. The connection event
	// reports these instead.
	connRcptAccept   int
	connRcptReject   int
	connRcptTempfail int
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

	// Sampled at construction, because usingTLS() reads the shared *smtp.Conn
	// live and the answer changes underneath this session when STARTTLS upgrades
	// it. Logout is the only reader.
	s.startedTLS = s.usingTLS()

	s.env = &ruleset.Env{Stage: ruleset.StageConnect, Conn: conn, Helo: s.heloEnv()}

	s.log = log.With("conn_uuid", s.connUUID.String(), "remote", s.remoteIP.String())
	s.log.Debug("session opened", "helo", s.helo())
	return s
}

// heloEnv rebuilds the connection-scoped facts a rule can read.
//
// Mail calls it again on every transaction, because STARTTLS may have upgraded
// the connection since the session was created — which is exactly why the
// authenticated identity has to be read from the session here rather than
// written into a HeloEnv once.
func (s *session) heloEnv() *ruleset.HeloEnv {
	h := &ruleset.HeloEnv{Name: s.helo(), AuthUser: s.authUser, AuthMechanism: s.authMech}
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
// reply.
//
// Every terminal kind has to be handled here. `discard` and `quarantine` are
// compile-rejected only *before* MAIL (internal/ruleset/eval.go), so a rule
// carrying `stage: mail` reaches this switch with one — and before M6 it fell
// through to `return nil`, which accepted the message and relayed it. That is
// the opposite of what the rule asked for, so they are recorded on the session
// and acted on in Data.
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
		return s.refuse(*a)
	case ruleset.ActDiscard, ruleset.ActQuarantine:
		// The message is still accepted — there is nothing to refuse yet, and
		// the client is told 250 either way. Data honours msgDrop.
		s.msgDrop = a.Kind
		s.countDrop(a.Kind)
		s.log.Info("policy dropped message", "rule", rule, "stage", at.String(), "action", a.Kind)
		return nil
	}
	return nil
}

// refuse records a message-scoped refusal and renders it as the reply.
//
// It is the only place msg_rejected and msg_tempfailed move, so the three sites
// that turn a policy action into a transaction's final answer cannot drift
// apart or double-count — each of them returns immediately afterwards.
//
// smtpError stays separate because rcptTerminal calls it for the *recipient*
// unit, which is counted elsewhere and must not land here.
func (s *session) refuse(a ruleset.Action) *smtp.SMTPError {
	switch a.Kind {
	case ruleset.ActReject:
		s.b.obs().MsgRejected.Add(1)
	case ruleset.ActTempfail:
		s.b.obs().MsgTempfailed.Add(1)
	}
	return smtpError(a)
}

// spoolRefusal turns a failed WriteBody into the transaction's final answer.
//
// Every failure used to collapse into one 451 4.3.0, which tells the sender to
// try again — so a message over max.bytes was retried for its whole queue
// lifetime, could never succeed, and eventually earned its sender a delay-expiry
// bounce that said nothing about size. A limit is a permanent refusal and has to
// answer 5xx.
//
// go-smtp's dataReader returns *SMTPError 552 5.3.4 mid-read (data.go:44,:74)
// and Spool.WriteBody wraps whatever io.Copy gave it with %w, so errors.Is
// reaches both sentinels here intact. Anything else really is a spool failure —
// no space, no permission — and keeps the 451 it always had.
//
// WriteBody's own defer removes the temp file on every error path, so there is
// nothing to clean up here.
func (s *session) spoolRefusal(err error) error {
	switch {
	case errors.Is(err, smtp.ErrDataTooLarge):
		s.log.Info("message refused: over max.bytes",
			"txn_uuid", s.txnUUID.String(), "max_bytes", s.b.Cfg.Server.Max.Bytes)
		return s.refuse(ruleset.Action{
			Kind: ruleset.ActReject, Code: 552, Enhanced: "5.3.4",
			Message: fmt.Sprintf("Error: message exceeds the %d byte limit (max.bytes)",
				s.b.Cfg.Server.Max.Bytes),
		})

	case errors.Is(err, smtp.ErrTooLongLine):
		// M10 assumed this could never reach us, because go-smtp answers it
		// itself — but that is only true of COMMAND lines (server.go:192). During
		// DATA the line-limit reader sits under the reader handed to us, so it
		// arrives here and used to become a 451: a permanent client-side format
		// violation that the sender then retried for its whole queue lifetime.
		// Same defect as the size limit, same fix. 500 5.5.2 matches Postfix.
		s.log.Info("message refused: over max.line_length",
			"txn_uuid", s.txnUUID.String(), "max_line_length", s.b.Cfg.Server.Max.LineLength)
		return s.refuse(ruleset.Action{
			Kind: ruleset.ActReject, Code: 500, Enhanced: "5.5.2",
			Message: fmt.Sprintf("Error: line longer than %d bytes (max.line_length)",
				s.b.Cfg.Server.Max.LineLength),
		})

	case errors.Is(err, errTooManyHeaderLines):
		s.log.Info("message refused: over max.header_lines",
			"txn_uuid", s.txnUUID.String(), "max_header_lines", s.b.Cfg.Server.Max.HeaderLines)
		return s.refuse(ruleset.Action{
			Kind: ruleset.ActReject, Code: 552, Enhanced: "5.3.4",
			Message: fmt.Sprintf("Error: message header exceeds %d lines (max.header_lines)",
				s.b.Cfg.Server.Max.HeaderLines),
		})

	default:
		s.log.Error("cannot spool message", "txn_uuid", s.txnUUID.String(), "err", err)
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "Error: cannot queue message",
		}
	}
}

// countDrop records a message-scoped discard or quarantine.
func (s *session) countDrop(kind string) {
	switch kind {
	case ruleset.ActDiscard:
		s.b.obs().MsgDiscarded.Add(1)
	case ruleset.ActQuarantine:
		s.b.obs().MsgQuarantined.Add(1)
	}
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
		s.dsnRet = string(opts.Return)
		s.dsnEnvID = opts.EnvelopeID
	}

	// The helo env is rebuilt because STARTTLS may have been negotiated since
	// the session was created.
	s.env.Helo = s.heloEnv()
	s.env.Mail = mail
	s.env.Stage = ruleset.StageMail

	s.log.Debug("MAIL FROM", "txn_uuid", s.txnUUID.String(), "from", from)

	// Before the SPF lookup and before policy, because a message that is being
	// refused for rate should not first cost a DNS walk — and before the
	// `accepted()` short-circuit below, because an `accept` rule says this
	// sender is welcome, not that it may send without limit. A rate limit is a
	// resource ceiling, not a policy decision, which is why it is the one thing
	// `accept` does not override.
	if err := s.checkMailRate(from); err != nil {
		return err
	}

	// Facts before rules, the ordering M8 established for the attachment walk:
	// a rule reading spf.result at this stage must see the answer, not a blank.
	// Run even when a connect- or helo-stage `accept` has already short-circuited
	// policy, because the result still goes in the header and still feeds DMARC.
	s.checkSPF(from)

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
func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !s.haveMail {
		return &smtp.SMTPError{
			Code:         503,
			EnhancedCode: smtp.EnhancedCode{5, 5, 1},
			Message:      "Error: need MAIL command",
		}
	}

	// Before the per-recipient policy pass and before any routing, on the same
	// reasoning as checkMailRate at MAIL: a recipient being refused for rate
	// should not first cost a rule walk, and `accept` does not buy an exemption
	// from a resource ceiling.
	//
	// Refused per recipient, so the rest of the transaction continues — which is
	// why this is checked here rather than at DATA. The counters that follow are
	// untouched: a recipient refused here was never accepted.
	if err := s.checkRcptRate(to); err != nil {
		return err
	}

	st := rcptState{addr: to, index: len(s.rcpts) + 1}
	if opts != nil {
		// RFC 3461, parsed and validated by go-smtp when EnableDSN is set —
		// ORCPT arrives xtext-decoded and its type upper-cased, and NOTIFY has
		// already been checked for the NEVER-with-anything-else combination.
		st.orcpt = opts.OriginalRecipient
		st.orcptType = string(opts.OriginalRecipientType)
		st.notify = notifyKeywords(opts.Notify)
	}

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

	// Carried into the data-stage pass; see rcptState.tags. Captured after both
	// the policy walk and Route, so it holds whatever either of them set.
	st.tags = renv.Tags

	s.rcpts = append(s.rcpts, st)
	// obs mirrors the per-transaction counters rather than deriving from them.
	// s.rcpt* are zeroed by resetTxn and only ever reach logservice through a
	// Queue event, so a transaction ended by RSET destroys its counts unseen —
	// a process-lifetime total cannot be reconstructed from the audit stream.
	s.rcptAccept++
	s.connRcptAccept++
	s.b.obs().RcptAccepted.Add(1)
	s.log.Debug("RCPT TO", "txn_uuid", s.txnUUID.String(), "to", to)
	return nil
}

// dataEnvFor builds the data-stage environment for one accepted recipient.
//
// It is built for every recipient, including those whose route already resolved
// at RCPT, because the data-stage *policy* rules still have to run — see
// split(). Tags from the RCPT-stage evaluation are overlaid on top of the
// session's, which is the precedence a single pass at DATA would produce:
// message-scoped rules run before recipient-scoped ones.
func (s *session) dataEnvFor(rs rcptState) *ruleset.Env {
	renv := s.env.ForRcpt(&ruleset.RcptEnv{
		To:    rs.addr,
		Index: rs.index,
		// Rcpt() passes s.rcptAccept, which at that point counts the
		// recipients before this one — and index is 1-based, so index-1 is
		// that same number. Recomputing it keeps a `txn.rcpt_count_so_far`
		// rule matching identically whether it is evaluated at RCPT or here.
		CountSoFar: rs.index - 1,
	})
	renv.Stage = ruleset.StageData
	for k, v := range rs.tags {
		renv.SetTag(k, v)
	}
	return renv
}

// rcptTerminal handles a terminal action scoped to one recipient. It returns a
// drop reason when the recipient stays accepted but must not be relayed.
func (s *session) rcptTerminal(a ruleset.Action, addr, rule string, at ruleset.Stage) (string, error) {
	switch a.Kind {
	case ruleset.ActReject:
		s.rcptReject++
		s.connRcptReject++
		s.b.obs().RcptRejected.Add(1)
		s.log.Info("recipient rejected", "rcpt", addr, "rule", rule, "stage", at.String(), "code", a.Code)
		return "", smtpError(a)
	case ruleset.ActTempfail:
		s.rcptTempfail++
		s.connRcptTempfail++
		s.b.obs().RcptTempfailed.Add(1)
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
	//
	// stripForgedAuthResults sits between them and is a no-op unless this
	// gateway will add an Authentication-Results field of its own — in which
	// case RFC 7601 §5 obliges it to remove inbound fields forging that same
	// authserv-id, or the field it adds means nothing. It removes only fields
	// bearing OUR id, so a third party's results survive and a message carrying
	// neither is spooled byte-for-byte as received.
	scan := newBodyScan(
		io.MultiReader(strings.NewReader(s.receivedHeader()), s.stripForgedAuthResults(r)),
		s.b.Cfg.Server.Max.HeaderLines,
	)
	name, size, err := s.b.Spool.WriteBody(s.txnUUID.String(), scan)
	if err != nil {
		return s.spoolRefusal(err)
	}
	// Counted once the body is on disk, and it includes the Received header we
	// prepended — so this is bytes spooled, not bytes off the wire.
	s.b.obs().BytesIn.Add(size)

	headers := scan.headers()
	s.env.Msg = &ruleset.MsgEnv{
		Size:          size,
		LineCount:     scan.lines,
		ReceivedCount: len(headers["received"]),
		Headers:       headers,
		RcptCount:     len(s.rcpts),
		RcptDomains:   s.rcptDomains(),
		// MimePartCount and Attachments are filled by inspectBody below, but only
		// when something will read them — see Ruleset.NeedsMIME.
	}
	s.env.Stage = ruleset.StageData

	// The hop limit RFC 5321 §6.3 asks for. max.received_headers has been
	// configured and defaulted since the first release and read by nothing, so
	// the one key that looks like a mail-loop guard did not guard anything —
	// a loop was caught only if an operator hand-wrote a rule on
	// msg.received_count.
	//
	// The count includes the trace header this hop prepended, which is what a
	// hop limit means. Zero leaves it unlimited.
	if max := s.b.Cfg.Server.Max.ReceivedHeaders; max > 0 && s.env.Msg.ReceivedCount > max {
		s.log.Info("message refused: routing loop",
			"txn_uuid", s.txnUUID.String(),
			"received_count", s.env.Msg.ReceivedCount, "max_received_headers", max)
		s.b.obs().MsgLoopRejected.Add(1)
		// Spooled to be scanned; past this point nothing will reference it.
		if err := s.b.Spool.RemoveBody(name); err != nil {
			s.log.Warn("cannot remove rejected body", "body", name, "err", err)
		}
		return s.refuse(ruleset.Action{
			Kind: ruleset.ActReject, Code: 554, Enhanced: "5.4.6",
			Message: "Error: routing loop detected (too many Received headers)",
		})
	}

	// Before any data-stage rule runs, so `attachment.*`, `msg.mime_part_count`
	// and `tag.attach_scan` are all readable by the pass below. Every
	// per-recipient env shares this *MsgEnv by pointer, so split() sees it too.
	inspected := s.inspectBody(name)

	// Same placement and the same reason: DKIM needs the body and DMARC needs
	// the From header, so both are DATA facts, and both have to be established
	// before the pass below can read dkim.*, dmarc.* or tag.msgauth. This is
	// also where the Authentication-Results and Received-SPF headers are queued
	// onto txnHeaders — see addAuthHeaders for why not receivedHeader().
	s.inspectMsgAuth(name)

	// Seeded from a decision an earlier stage already made — a `stage: mail`
	// discard has the same effect as one at DATA.
	msgDrop := s.msgDrop
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
				return s.refuse(*a)
			case ruleset.ActDiscard, ruleset.ActQuarantine:
				msgDrop = a.Kind
				s.countDrop(a.Kind)
				s.log.Info("policy dropped message", "rule", res.Rule, "action", a.Kind)
			}
		}
	}

	// The blocklist verdict lands after the rules, and only when no rule reached
	// a terminal decision of its own: attach.on_block is the DEFAULT answer, not
	// an override. An `accept` rule is the whitelist, and a rule that already
	// chose quarantine has said where the message goes. Same shape as the block
	// above otherwise — a refusal drops the body and answers, a drop feeds
	// msgDrop.
	if !s.accepted() && msgDrop == "" {
		if a := s.attachAction(inspected); a != nil {
			switch a.Kind {
			case ruleset.ActReject, ruleset.ActTempfail:
				if err := s.b.Spool.RemoveBody(name); err != nil {
					s.log.Warn("cannot remove rejected body", "body", name, "err", err)
				}
				return s.refuse(*a)
			case ruleset.ActDiscard, ruleset.ActQuarantine:
				msgDrop = a.Kind
				s.countDrop(a.Kind)
			}
		}
	}

	envelopes, failed, dropped := s.split(name, size, msgDrop)
	// split's `dropped` is the one place that sees every way a recipient can be
	// accepted and then not relayed: a message-scoped discard, an RCPT-stage
	// one, a data-stage one, and a route that resolved to discard.
	s.b.obs().RcptDiscarded.Add(int64(dropped))

	if len(envelopes) == 0 {
		if err := s.b.Spool.RemoveBody(name); err != nil {
			s.log.Warn("cannot remove unused body", "body", name, "err", err)
		}
		if len(failed) > 0 {
			// Parity with mailgw/plugins/npRoute.js:65, whose DENYSOFT
			// "No route found" is the default_action of a converted table.
			s.log.Warn("no destination for any recipient",
				"txn_uuid", s.txnUUID.String(), "rcpts", failedAddrs(failed))
			return s.refuse(failed[0].action)
		}
		// Everything was discarded: the message is accepted and goes nowhere.
		s.tranCount++
		s.postQueue(dataStart, size)
		s.log.Info("accepted and dropped", "txn_uuid", s.txnUUID.String(), "dropped", dropped)
		return s.queuedReply()
	}

	// Some recipients resolved to a refusal only after the message was accepted,
	// so there is no reply left to put them in. The permanent ones are told to
	// the sender instead; see bounceRefused.
	s.bounceRefused(scan.rawHeaders(), size, failed)

	for _, e := range envelopes {
		enqueue := s.b.Spool.Enqueue
		if e.Quarantined {
			enqueue = s.b.Spool.Quarantine
		}
		if err := enqueue(e.env); err == nil {
			// Counted where the spool file actually exists, so the counter and
			// the mailgw_queue_* gauges are always describing the same thing.
			if e.Quarantined {
				s.b.obs().EnvelopesQuarantined.Add(1)
			} else {
				s.b.obs().EnvelopesQueued.Add(1)
			}
		} else {
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
	// The single place a transaction is answered 250, so one increment covers
	// both callers exactly once. Note this is a SUPERSET of msg_discarded and
	// msg_quarantined, not a disjoint bucket: a message every rule dropped is
	// still accepted.
	s.b.obs().MsgAccepted.Add(1)
	return &smtp.SMTPError{
		Code:         250,
		EnhancedCode: smtp.EnhancedCode{2, 0, 0},
		Message:      fmt.Sprintf("Message queued (%s)", s.txnUUID),
	}
}

// bounceRefused tells the sender about recipients that were refused after the
// message had already been accepted.
//
// Only PERMANENT refusals become a notification. A recipient can also end up
// here on a 4xx — most often the default action, a 451 "No route found", which
// preserves the DENYSOFT at npRoute.js:65 for a recipient no rule routed.
// Bouncing that would turn a gap in the routing configuration into permanent
// rejections for mail that would be delivered fine once the gap was noticed.
// Temporary refusals keep the warning they have always had; the message is gone,
// but the sender's own retry is the correct recovery.
//
// The headers come from the scan that already streamed past on the way to disk,
// rather than from re-reading the spooled file: this runs while the body is
// still being decided about, and half of the callers' paths delete it moments
// later.
func (s *session) bounceRefused(headers string, size int64, failed []failedRcpt) {
	if len(failed) == 0 {
		return
	}

	var permanent, temporary []failedRcpt
	for _, f := range failed {
		if f.permanent() {
			permanent = append(permanent, f)
		} else {
			temporary = append(temporary, f)
		}
	}

	if len(temporary) > 0 {
		s.log.Warn("recipients temporarily refused after acceptance; no notification sent",
			"txn_uuid", s.txnUUID.String(), "rcpts", failedAddrs(temporary))
	}
	if len(permanent) == 0 {
		return
	}
	if s.b.Bounce == nil {
		s.log.Warn("recipients refused after acceptance and bounces are not configured",
			"txn_uuid", s.txnUUID.String(), "rcpts", failedAddrs(permanent))
		return
	}

	rcpts := make([]dsn.Rcpt, 0, len(permanent))
	for _, f := range permanent {
		// The sender's NOTIFY applies here exactly as it does in the queue: a
		// recipient refused by a data-stage rule is still a recipient whose
		// sender may have asked not to be told.
		asked := queue.Recipient{Notify: f.notify}
		if !asked.WantsFailureDSN() {
			s.b.obs().DSNNotifySuppressed.Add(1)
			continue
		}
		rcpts = append(rcpts, dsn.Rcpt{
			Addr:        f.addr,
			OrigAddr:    f.orcpt,
			OrigType:    f.orcptType,
			Status:      enhancedOf(f.action),
			Diagnostic:  refusalText(f),
			LastAttempt: time.Now(),
		})
	}
	if len(rcpts) == 0 {
		// Every refused recipient declined. Without this guard the code below
		// would build the envelope, get a silent (false, nil) out of Bounce —
		// which returns early on an empty recipient list — and then log that the
		// sender had been notified, which would simply be false.
		s.b.obs().DSNSuppressed.Add(1)
		s.log.Info("recipients refused after acceptance, but every sender asked not to be notified",
			"txn_uuid", s.txnUUID.String(), "rcpts", failedAddrs(permanent))
		return
	}

	// A synthetic envelope: nothing was queued for these recipients, but the
	// notification still needs the message's identity to hang off and the
	// sender to go back to. Its uuid is the transaction's first unused child, so
	// it cannot collide with an envelope split() produced.
	env := &queue.Envelope{
		Version:  queue.EnvelopeVersion,
		UUID:     s.txnUUID.Child(len(s.rcpts) + 1).String(),
		TxnUUID:  s.txnUUID.String(),
		ConnUUID: s.connUUID.String(),
		MailFrom: s.mailFrom,
		QueuedAt: time.Now().UnixMilli(),
		BodySize: size,
		DSNEnvID: s.dsnEnvID,
		// Records what this notification actually quotes, not what the sender
		// asked for. Only the header block is available here — the body is
		// still being decided about and half this function's callers delete it
		// moments later — so honouring RET=FULL would label headers as a
		// message/rfc822 part. A sender's RET is honoured on every queued
		// envelope, which is every message that was not refused outright.
		DSNRet: "HDRS",
	}

	if _, err := s.b.Bounce(queue.Request{
		Env:      env,
		Rcpts:    rcpts,
		Kind:     dsn.KindFailed,
		Original: strings.NewReader(headers),
	}); err != nil {
		s.log.Error("cannot notify the sender of refused recipients",
			"txn_uuid", s.txnUUID.String(), "err", err)
		return
	}
	s.log.Info("notified the sender of recipients refused after acceptance",
		"txn_uuid", s.txnUUID.String(), "rcpts", failedAddrs(permanent))
}

// enhancedOf renders a refusal's enhanced status code for a notification,
// falling back to the class of its reply code.
func enhancedOf(a ruleset.Action) string {
	if a.Enhanced != "" {
		return a.Enhanced
	}
	if a.Code >= 500 {
		return "5.7.1"
	}
	return "4.7.1"
}

// refusalText renders why a recipient was refused, naming the rule when one is
// known. "rejected by rule block-finance" is actionable; "rejected" is not.
func refusalText(f failedRcpt) string {
	msg := f.action.Message
	if msg == "" {
		msg = "refused by policy"
	}
	if f.rule != "" {
		msg = fmt.Sprintf("%s (rule %s)", msg, f.rule)
	}
	if f.action.Code > 0 {
		return fmt.Sprintf("%d %s", f.action.Code, msg)
	}
	return msg
}

// outEnvelope pairs a built envelope with where it should be filed.
type outEnvelope struct {
	env         *queue.Envelope
	Quarantined bool
}

// bucketRcpt is a recipient assigned to an envelope, with the rule that put it
// there. The rule travels per recipient because one envelope can hold
// recipients routed to the same group by different rules.
type bucketRcpt struct {
	addr string
	rule string
	// The sender's RFC 3461 per-recipient parameters. Deliberately NOT part of
	// bucketKey: bucketing groups recipients that are indistinguishable for
	// delivery, and these travel on the queued recipient rather than changing
	// where or how the message is sent. Keying on them would fragment envelopes
	// and multiply relay transactions for no gain.
	orcpt     string
	orcptType string
	notify    []string
}

// failedRcpt is a recipient whose evaluation ended in a refusal.
type failedRcpt struct {
	addr   string
	action ruleset.Action
	// Carried for the same reason bucketRcpt carries them: the post-acceptance
	// notification is built from these, and the synthetic envelope it hangs off
	// has no recipients of its own to read them from.
	orcpt     string
	orcptType string
	notify    []string
	// rule names what refused it. Carried so a notification can say which rule
	// stopped the message rather than only that something did — the difference
	// between an operator finding the rule and guessing at it.
	rule string
}

// newFailedRcpt records a refusal along with the DSN parameters its sender
// supplied, so the notification honours them.
//
// A constructor rather than three literals because split refuses a recipient in
// three places, and one of them forgetting to carry NOTIFY would mean bouncing
// to a sender who asked not to be told — a mistake nothing else in the system
// would report.
func newFailedRcpt(rs rcptState, a ruleset.Action, rule string) failedRcpt {
	return failedRcpt{
		addr: rs.addr, action: a, rule: rule,
		orcpt: rs.orcpt, orcptType: rs.orcptType, notify: rs.notify,
	}
}

// notifyKeywords normalises go-smtp's NOTIFY values for the spool.
//
// Strings rather than smtp.DSNNotify because internal/queue does not import
// go-smtp and should not start: the envelope is a persisted document, and its
// vocabulary belongs to RFC 3461 rather than to whichever library parsed it.
func notifyKeywords(ns []smtp.DSNNotify) []string {
	if len(ns) == 0 {
		return nil
	}
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, strings.ToUpper(string(n)))
	}
	return out
}

// permanent reports whether this refusal is final.
//
// Only a 5xx may become a bounce. The default action for "no rule routed this
// recipient" is a 451 (ruleset.DefaultAction), preserving Haraka's DENYSOFT at
// npRoute.js:65 — turning that into a permanent bounce would mean a routing
// configuration gap permanently rejects mail that would have been delivered
// once the gap was noticed.
func (f failedRcpt) permanent() bool { return f.action.Kind == ruleset.ActReject }

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
		rcpts      []bucketRcpt
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

		// The data-stage policy pass runs for every recipient, whether or not
		// routing already resolved at RCPT. These are two separate questions:
		// a cached *route* is provably the one DATA would produce (Route stops
		// at the first rule needing a later stage), but no policy rule was
		// evaluated early, so caching the decision must not skip them. Gating
		// both on one condition silently disabled every recipient-scoped
		// data-stage policy rule — which is most of what M8's attachment and
		// header rules will be.
		renv := s.dataEnvFor(rs)

		rcptHeaders := rs.headers
		if !s.accepted() {
			res := s.rules.EvalPolicyRcpt(renv, ruleset.StageData)
			rcptHeaders = append(rcptHeaders, res.Headers...)
			if a := res.Action; a != nil {
				switch a.Kind {
				case ruleset.ActReject, ruleset.ActTempfail:
					failed = append(failed, newFailedRcpt(rs, *a, res.Rule))
					continue
				case ruleset.ActDiscard:
					dropped++
					continue
				case ruleset.ActQuarantine:
					drop = ruleset.ActQuarantine
				}
			}
		}

		decision := rs.decision
		if decision == nil {
			// At DATA every rule is evaluable, so this always decides.
			d, ok := s.rules.Route(renv, ruleset.StageData)
			if !ok {
				failed = append(failed, newFailedRcpt(rs, ruleset.DefaultAction(), ""))
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
				failed = append(failed, newFailedRcpt(rs, decision.Action, decision.Rule))
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
			// renv.Tags rather than decision.Tags: when the route came from the
			// RCPT-stage cache, decision.Tags predates the data-stage policy
			// pass above. In the uncached path Route returns env.Tags, so the
			// two are the same map and nothing changes.
			b = &bucket{group: group, headers: headers, quarantine: quarantine, tags: renv.Tags}
			buckets[key] = b
			order = append(order, key)
		}
		// decision.Rule is empty when the default action applied, which is
		// exactly what "no rule chose this" should record.
		b.rcpts = append(b.rcpts, bucketRcpt{
			addr: rs.addr, rule: decision.Rule,
			orcpt: rs.orcpt, orcptType: rs.orcptType, notify: rs.notify,
		})
	}

	// Deterministic envelope numbering, so the same message always produces
	// the same uuids — which is what makes the e2e assertions stable.
	sort.Strings(order)

	now := time.Now()
	for i, key := range order {
		b := buckets[key]
		rcpts := make([]queue.Recipient, 0, len(b.rcpts))
		for _, r := range b.rcpts {
			rcpts = append(rcpts, queue.Recipient{
				Addr: r.addr, Status: queue.StatusPending, RouteRule: r.rule,
				OrigRcpt: r.orcpt, OrigRcptType: r.orcptType, Notify: r.notify,
			})
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
				DSNRet:   s.dsnRet,
				DSNEnvID: s.dsnEnvID,
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

// postConnection sends the Connection event for this session.
//
// The recipient counts are the CONNECTION's, not the current transaction's:
// resetTxn zeroes the per-transaction ones, so a pipelining client that RSETs
// after a run of rejections would otherwise report zeroes for exactly the
// traffic an operator opened the log viewer to find.
func (s *session) postConnection() {
	if s.b.Events == nil {
		return
	}
	s.connPosted = true
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
			RcptCountAccept:   s.connRcptAccept,
			RcptCountReject:   s.connRcptReject,
			RcptCountTempfail: s.connRcptTempfail,
			Gateway:           s.b.Gateway,
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
			// Zero unless the MIME walk ran, which is honest: the column has been
			// on this payload since the Haraka days (functions.js:171) and has
			// always been sent as 0 by this gateway. It is a count, not a flag —
			// a message nothing walked has no known part count.
			MimePartCount: s.mimePartCount(),
			Gateway:       s.b.Gateway,
		},
	})
}

// mimePartCount reports what the walk found, guarding the nil MsgEnv a
// transaction refused before DATA leaves behind.
func (s *session) mimePartCount() int {
	if s.env == nil || s.env.Msg == nil {
		return 0
	}
	return s.env.Msg.MimePartCount
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
	// Both are MAIL parameters and therefore transaction-scoped. See the field
	// declarations: Mail rewrites them unconditionally today, so this is
	// defensive rather than load-bearing.
	s.dsnRet = ""
	s.dsnEnvID = ""
	// SPF is evaluated per MAIL and DKIM per message, so carrying either into
	// the next transaction on the same connection would report a result for a
	// sender that was never checked.
	s.spf = msgauth.SPFResult{}
	s.dkim = msgauth.DKIMResult{}
	s.dmarc = msgauth.DMARCResult{}
	s.rcptAccept = 0
	s.rcptReject = 0
	s.rcptTempfail = 0
	s.txnHeaders = nil
	s.txnAccept = false
	s.msgDrop = ""

	if s.env != nil {
		s.env.Mail = nil
		s.env.Rcpt = nil
		s.env.Msg = nil
		s.env.Stage = ruleset.StageHelo
	}
}

// Logout ends the session, and is the last chance to report a connection that
// never reached DATA.
//
// Data was the only caller of postConnection, so a connection that was greeted
// and dropped, RSET, or had every recipient rejected produced no Connection row
// at all — precisely the traffic an operator opens the log viewer to
// investigate. Connections that did reach DATA are unaffected: they have already
// posted, and connPosted stops a second row.
//
// The STARTTLS exception is not optional. go-smtp logs out the pre-upgrade
// session and then calls NewSession again (conn.go:948), and every session mints
// its own connUUID — so posting unconditionally here would write two Connection
// rows, under two different uuids, for one TCP connection. A session that began
// in the clear and finds itself on a TLS conn can only have been superseded by
// that upgrade, and its successor reports for the connection.
func (s *session) Logout() error {
	s.log.Debug("session closed", "transactions", s.tranCount)
	if !s.connPosted && !(s.usingTLS() && !s.startedTLS) {
		s.postConnection()
	}
	return nil
}
