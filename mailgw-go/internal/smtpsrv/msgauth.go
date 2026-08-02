package smtpsrv

import (
	"io"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// TagMsgAuth is the tag the combined message-authentication verdict is published
// under, readable from a rule as `tag.msgauth`.
//
// Same purpose as TagAttachScan: it lets an operator branch on the outcome
// without repeating three predicates, and it is set BEFORE the data-stage policy
// pass so the tag and the fields it summarises are readable together. Its value
// is the strongest thing that can be said about the message — "dmarc_pass",
// "dkim_pass", "spf_pass", "fail" or "none".
const TagMsgAuth = "msgauth"

// MsgAuth is the message-authentication checker a session uses.
//
// It is built once at bring-up and is nil when nothing is configured and no rule
// asks — exactly like Backend.Attach, and checked the same way on the mail path
// rather than by re-reading configuration per message.
type MsgAuth struct {
	Resolver   msgauth.Resolver
	AuthservID string

	// SPF, DKIM and DMARC record what the CONFIGURATION asked for. A rule that
	// reads the corresponding fields turns its check on as well — see
	// wantSPF and friends — so a configuration can leave all three off and still
	// get exactly the checks its rules need.
	SPF, DKIM, DMARC bool

	MaxDKIMSignatures int
}

func (m *MsgAuth) wantSPF(rs *ruleset.Ruleset) bool {
	return m != nil && (m.SPF || m.DMARC || rs.NeedsSPF())
}

func (m *MsgAuth) wantDKIM(rs *ruleset.Ruleset) bool {
	return m != nil && (m.DKIM || m.DMARC || rs.NeedsDKIM())
}

func (m *MsgAuth) wantDMARC(rs *ruleset.Ruleset) bool {
	return m != nil && (m.DMARC || rs.NeedsDMARC())
}

// wantsAnything reports whether this session will produce an
// Authentication-Results header. It is also what decides whether an inbound
// forged one is stripped: RFC 7601 §5 makes stripping the obligation of a system
// that ADDS the header, and a gateway that adds nothing has no name to protect.
func (s *session) wantsMsgAuth() bool {
	m := s.b.MsgAuth
	return m.wantSPF(s.rules) || m.wantDKIM(s.rules) || m.wantDMARC(s.rules)
}

// authservID is the name this gateway signs its results with.
func (s *session) authservID() string {
	if s.b.MsgAuth != nil && s.b.MsgAuth.AuthservID != "" {
		return s.b.MsgAuth.AuthservID
	}
	return s.b.Cfg.Server.Hostname
}

// checkSPF evaluates SPF for this transaction.
//
// Called from Mail, after env.Mail is built and BEFORE the mail-stage policy
// pass, so a rule reading spf.result sees a fact rather than a blank — the
// facts-then-rules ordering M8 established for the attachment walk.
//
// SPF is the one message-authentication check answerable this early: it is a DNS
// walk over the sender's domain and the peer's address and never looks at the
// message. That is what lets `spf.result eq "fail"` be refused on the sender's
// own MAIL line instead of after a megabyte of DATA.
func (s *session) checkSPF(from string) {
	if !s.b.MsgAuth.wantSPF(s.rules) {
		return
	}
	// The serve context, not context.Background(): this is DNS inside the MAIL
	// reply, and a black-holed nameserver would otherwise hold the session and
	// its goroutine past the whole shutdown budget. Same defect M8 fixed in
	// events.Client and M10 in the attachment scan.
	s.spf = msgauth.CheckSPF(s.b.ctx(), s.b.MsgAuth.Resolver, s.remoteIP, s.helo(), from)
	s.env.Mail.SPFResult = string(s.spf.Value)
	s.env.Mail.SPFDomain = s.spf.Domain

	s.b.obs().SPFChecked.Add(1)
	if s.spf.Value == msgauth.ResultFail {
		s.b.obs().SPFFailed.Add(1)
	}
}

// inspectMsgAuth verifies DKIM and evaluates DMARC over the spooled message.
//
// Called from Data immediately after inspectBody and before the data-stage
// policy pass, for the same reason and with the same shape: facts first, so
// dkim.*, dmarc.* and tag.msgauth are all readable by the pass that decides what
// happens to the message.
//
// Like the MIME walk it re-reads the spooled body — bodyScan deliberately does
// not buffer — so it only runs when something will read the result.
func (s *session) inspectMsgAuth(bodyName string) {
	if !s.wantsMsgAuth() {
		return
	}

	if s.b.MsgAuth.wantDKIM(s.rules) {
		s.verifyDKIM(bodyName)
	}

	if s.b.MsgAuth.wantDMARC(s.rules) {
		// The From header is already parsed: bodyScan captured it on the way to
		// the spool, so this costs no extra read. FromDomainOf takes the first
		// value because RFC 7489 §6.6.1 makes a message with several From
		// addresses unevaluable anyway — there is no better answer.
		var from string
		if v := s.env.Msg.Headers["from"]; len(v) > 0 {
			from = msgauth.FromDomainOf(v[0])
		}
		dm := msgauth.EvaluateDMARC(s.b.ctx(), s.b.MsgAuth.Resolver, from, s.spf, s.dkim)
		s.dmarc = dm
		s.env.Msg.DMARCResult = string(dm.Value)
		s.env.Msg.DMARCPolicy = dm.Policy
	}

	s.env.SetTag(TagMsgAuth, s.msgAuthVerdict())
	s.addAuthHeaders()
}

func (s *session) verifyDKIM(bodyName string) {
	f, err := s.b.Spool.ReadBody(bodyName)
	if err != nil {
		// The body is on disk and was just written, so this is a real fault
		// rather than a bad message. Nothing is refused on it — a gateway that
		// tempfailed here would be refusing mail because of its own spool.
		s.log.Warn("cannot re-read the spooled body for DKIM verification",
			"txn_uuid", s.txnUUID.String(), "err", err)
		return
	}
	defer f.Close()

	// The spooled body starts with the Received: header this gateway prepended.
	// That is correct and must not be stripped: a DKIM signature never covers a
	// header added after it was made, and go-msgauth selects signed fields by
	// the signature's own h= tag.
	s.dkim = msgauth.VerifyDKIM(s.b.ctx(), s.b.MsgAuth.Resolver, f, s.b.MsgAuth.MaxDKIMSignatures)
	s.env.Msg.DKIMResult = string(s.dkim.Value)
	s.env.Msg.DKIMDomains = s.dkim.Domains
	s.b.obs().DKIMVerified.Add(1)
}

// msgAuthVerdict summarises the checks as one tag value: the strongest claim
// that can be made about the message.
func (s *session) msgAuthVerdict() string {
	switch {
	case s.dmarc.Value == msgauth.ResultPass:
		return "dmarc_pass"
	case s.dkim.Value == msgauth.ResultPass:
		return "dkim_pass"
	case s.spf.Value == msgauth.ResultPass:
		return "spf_pass"
	case s.dmarc.Value == msgauth.ResultFail ||
		s.dkim.Value == msgauth.ResultFail ||
		s.spf.Value == msgauth.ResultFail:
		return "fail"
	default:
		return "none"
	}
}

// addAuthHeaders records the results for whatever reads the message next.
//
// They go into txnHeaders — the same list an `add_header` rule action feeds —
// rather than into receivedHeader(), and that is deliberate. receivedHeader() is
// written BEFORE DATA is read, so the DKIM and DMARC results do not exist when
// it runs. txnHeaders also brings the sanitiser (toQueueHeaders →
// sanitizeHeaderValue) and lands the fields ABOVE this gateway's own Received:
// via Envelope.PrependBlock, which is the ordering RFC 7601 wants.
//
// The cost is that these headers are not in the spooled bytes, so they are not
// counted in msg.size and are not visible as header.authentication-results to a
// rule. Both are correct: our own results are not a fact about the message we
// received, and a rule reading them would be reading its own gateway's output.
func (s *session) addAuthHeaders() {
	ar := msgauth.FormatAuthResults(s.authservID(), s.spf, s.dkim, s.dmarc)
	if ar == "" {
		// Nothing ran. Adding "authservid; none" would assert that this gateway
		// looked and found nothing, which it did not.
		return
	}
	s.txnHeaders = append(s.txnHeaders,
		ruleset.Header{Name: msgauth.HeaderAuthResults, Value: ar})

	if s.spf.Checked() {
		s.txnHeaders = append(s.txnHeaders, ruleset.Header{
			Name:  msgauth.HeaderReceivedSPF,
			Value: msgauth.FormatReceivedSPF(s.spf, s.remoteIP, s.authservID()),
		})
	}
}

// stripForgedAuthResults wraps the DATA reader when this gateway is going to add
// an Authentication-Results field of its own.
//
// RFC 7601 §5 requires it: without this a sender can simply assert "dkim=pass"
// under our authserv-id, and nothing downstream that trusts our name can tell
// the forgery from the real thing.
//
// It is installed only when we will add a field, and it removes only fields
// bearing OUR id — so a gateway with msgauth off spools byte-for-byte what it
// received, and a gateway behind an upstream that legitimately verifies mail
// keeps that upstream's results.
func (s *session) stripForgedAuthResults(r io.Reader) io.Reader {
	if !s.wantsMsgAuth() {
		return r
	}
	return msgauth.StripAuthResults(r, s.authservID())
}
