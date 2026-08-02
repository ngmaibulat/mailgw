package smtpsrv

import (
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/attach"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// TagAttachScan is the tag the scanner's verdict is published under, readable
// from a rule as `tag.attach_scan`.
//
// It exists so an operator can express "quarantine blocked mail from partners,
// reject it from everyone else" without a second configuration key per case: the
// verdict is a fact, and the rule engine is where facts turn into decisions.
// attach.on_block remains the answer when no rule says anything.
const TagAttachScan = "attach_scan"

// attachOutcome is what inspectBody learned.
type attachOutcome struct {
	// verdict is empty when the blocklist was not consulted — scanning off, or
	// nothing in the message to ask about.
	verdict attach.Verdict
	// err is a *scan* failure: a malformed MIME structure or an unusable answer
	// from logservice. It is only ever set when attach.enabled, because a
	// message that merely fails to answer a rule's question is not a refusal.
	err error
}

// inspectBody walks the spooled message and, when scanning is on, asks the
// blocklist about what it found.
//
// It reads the body back from the spool rather than buffering it on the way
// past. bodyScan explains why it does not buffer, and the answer here is the
// same one in reverse: this costs a re-read, so it only happens when the
// configuration will actually use the result.
func (s *session) inspectBody(bodyName string) attachOutcome {
	enabled := s.b.Attach != nil
	if !enabled && !s.rules.NeedsMIME() {
		return attachOutcome{}
	}

	res, err := s.walk(bodyName)

	// Facts first, and whatever was parsed before a failure still counts: a
	// truncated multipart's first attachment is a real attachment.
	s.env.Msg.MimePartCount = res.PartCount
	s.env.Msg.Attachments = toRulesetAttachments(res.Parts)

	if !enabled {
		if err != nil {
			// Nothing is being refused on this basis, so this is a note, not a
			// verdict: the rules will evaluate against a partial part list.
			s.log.Warn("could not fully walk the message's MIME structure; "+
				"attachment rules see only what was parsed",
				"txn_uuid", s.txnUUID.String(), "parts", res.PartCount, "err", err)
		}
		return attachOutcome{}
	}

	if err != nil {
		// AttachChecker.js:77 turned exactly this into "allow", which made a
		// deliberately malformed multipart a scanner bypass.
		s.setScanTag("error")
		return attachOutcome{err: err}
	}

	// The serve context, not context.Background(): this HTTP call sits inside the
	// DATA reply, so a scanner that never answers would otherwise hold the
	// session — and its goroutine — past the whole shutdown budget. Same defect
	// M8 fixed in events.Client, in a different package.
	verdict, err := s.b.Attach.Check(s.b.ctx(), s.txnUUID.String(), res.Parts)
	if err != nil {
		s.setScanTag("error")
		return attachOutcome{err: err}
	}
	s.setScanTag(string(verdict))
	return attachOutcome{verdict: verdict}
}

func (s *session) walk(bodyName string) (attach.Result, error) {
	f, err := s.b.Spool.ReadBody(bodyName)
	if err != nil {
		return attach.Result{}, err
	}
	defer f.Close()
	return attach.Walk(f, attach.Options{IncludeInline: s.b.Cfg.Server.Attach.IncludeInline})
}

// setScanTag publishes the verdict before any data-stage rule runs, so
// `tag.attach_scan` is readable by the same pass that reads `attachment.md5`.
func (s *session) setScanTag(v string) { s.env.SetTag(TagAttachScan, v) }

// attachAction turns an outcome into the terminal action the message gets, or
// nil to carry on.
//
// Deliberately applied AFTER the data-stage policy pass, and only when that pass
// reached no terminal decision: attach.on_block is the DEFAULT answer to a
// blocked message, not an override of one. `accept` is the whitelist, and a rule
// reading tag.attach_scan that chose quarantine has already said where the
// message goes — a scanner that pre-empted either would leave an operator no way
// to say "this sender's macros are fine".
func (s *session) attachAction(o attachOutcome) *ruleset.Action {
	cfg := s.b.Cfg.Server.Attach

	if o.err != nil {
		if !cfg.FailClosed() {
			// AttachChecker.js:36 and :77 did this unconditionally and silently.
			// Here it is a deliberate setting, and it is loud.
			s.log.Error("attachment scan failed; attach.fail is open, so the message is being relayed unscanned",
				"txn_uuid", s.txnUUID.String(), "err", o.err)
			return nil
		}
		s.log.Warn("attachment scan failed; refusing (attach.fail is closed)",
			"txn_uuid", s.txnUUID.String(), "err", o.err)
		// 4xx, not 5xx: a scanner outage is temporary and is not the sender's
		// doing. Rejecting permanently would turn ten minutes of logservice
		// downtime into mail that never arrives.
		return &ruleset.Action{
			Kind: ruleset.ActTempfail, Code: 451, Enhanced: "4.7.1",
			Message: "Attachment scan unavailable, try again later",
		}
	}

	if o.verdict != attach.Block {
		return nil
	}

	s.log.Info("attachment blocklist match", "txn_uuid", s.txnUUID.String(),
		"action", cfg.BlockAction(), "attachments", len(s.env.Msg.Attachments))

	const blocked = "Blocked: attachment scan"
	switch cfg.BlockAction() {
	case config.AttachBlockTempfail:
		// Literal npFilterAttach.js:45 parity — its DENYSOFT.
		return &ruleset.Action{Kind: ruleset.ActTempfail, Code: 451, Enhanced: "4.7.1", Message: blocked}
	case config.AttachBlockQuarantine:
		return &ruleset.Action{Kind: ruleset.ActQuarantine}
	case config.AttachBlockDiscard:
		return &ruleset.Action{Kind: ruleset.ActDiscard}
	default:
		return &ruleset.Action{Kind: ruleset.ActReject, Code: 550, Enhanced: "5.7.1", Message: blocked}
	}
}

func toRulesetAttachments(parts []attach.Part) []ruleset.Attachment {
	if len(parts) == 0 {
		return nil
	}
	out := make([]ruleset.Attachment, 0, len(parts))
	for _, p := range parts {
		out = append(out, ruleset.Attachment{
			Filename:    p.Filename,
			ContentType: p.ContentType,
			MD5:         p.MD5,
			Disposition: p.Disposition,
			Size:        p.Size,
		})
	}
	return out
}
