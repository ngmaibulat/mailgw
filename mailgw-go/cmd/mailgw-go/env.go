package main

import (
	"bufio"
	"fmt"
	"net/mail"
	"net/netip"
	"os"
	"strings"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/attach"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// explainOpts is what `explain` was told to pretend about the session.
//
// A struct rather than a parameter list because the flags outgrew one: with the
// auth pair there are ten, and a call site of ten positional arguments is one
// transposition away from explaining a different message than the operator asked
// about.
type explainOpts struct {
	IP     string
	Helo   string
	From   string
	Rcpt   string
	TLS    bool
	EML    string
	Inline bool
	// AuthUser and AuthMech model a session that authenticated. Without them a
	// rule reading auth.* could be written but never explained, which is the
	// same "you cannot tell whether it works" gap M13 closed for the fields
	// themselves.
	AuthUser string
	AuthMech string

	// SPF, DKIM and DMARC model results a real check would have produced.
	//
	// They are FLAGS rather than live lookups, on the same footing as TLS and
	// AuthUser: `explain` answers "what would my rules do if…", and the useful
	// question is "if DMARC failed", not "what does DNS say right now". It also
	// keeps explain free of network I/O, which is what lets it run on a laptop
	// against a bundle for a gateway on another continent.
	//
	// Empty means the check did not run, which is what the fields themselves
	// mean — see MailEnv.SPFResult.
	SPF   string
	DKIM  string
	DMARC string
	// DMARCPolicy is the p= the From domain published, readable as dmarc.policy.
	DMARCPolicy string

	Stage ruleset.Stage
}

// buildEnv assembles the fact base `explain` evaluates against, filling in only
// what the requested stage would actually know.
func buildEnv(o explainOpts) (*ruleset.Env, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(o.IP))
	if err != nil {
		return nil, fmt.Errorf("-ip %q: %w", o.IP, err)
	}

	env := &ruleset.Env{
		Stage: o.Stage,
		Conn:  &ruleset.ConnEnv{RemoteIP: addr.Unmap(), RemotePort: 54321},
	}
	if o.Stage >= ruleset.StageHelo {
		env.Helo = &ruleset.HeloEnv{Name: o.Helo, TLS: o.TLS}
		if o.TLS {
			env.Helo.TLSVersion = "TLS1.3"
		}
		// AUTH happens after EHLO is answered, which is why the auth.* fields
		// are registered at StageMail — so a connect- or helo-stage explanation
		// must not show an authenticated client. The facts live on HeloEnv
		// because that is the connection scope; the stage is what gates them.
		if o.Stage >= ruleset.StageMail {
			env.Helo.AuthUser = o.AuthUser
			env.Helo.AuthMechanism = o.AuthMech
		}
	}
	if o.Stage >= ruleset.StageMail {
		env.Mail = &ruleset.MailEnv{From: o.From}
		// SPF is a mail-stage fact, so it is available here and not before —
		// the same gating the auth pair above gets, and the reason a rule
		// reading spf.result is inferred to StageMail.
		if o.SPF != "" {
			env.Mail.SPFResult = o.SPF
			env.Mail.SPFDomain = spfDomainFor(o.Helo, o.From)
		}
	}
	if o.Stage >= ruleset.StageRcpt {
		env.Rcpt = &ruleset.RcptEnv{To: o.Rcpt, Index: 1, CountSoFar: 0}
	}
	if o.Stage < ruleset.StageData {
		return env, nil
	}

	msg := &ruleset.MsgEnv{
		RcptCount:   1,
		RcptDomains: []string{strings.ToLower(ruleset.Domain(o.Rcpt))},
		Headers:     map[string][]string{},
	}
	if o.EML != "" {
		if err := loadMessage(o.EML, msg, o.Inline); err != nil {
			return nil, err
		}
	}

	// DKIM and DMARC need the body and the From header respectively, so both are
	// data-stage facts however early SPF was answered.
	if o.DKIM != "" {
		msg.DKIMResult = o.DKIM
		if o.DKIM == "pass" {
			// A passing signature has to name a domain for dkim.domains and for
			// DMARC alignment to be explainable at all. The From header of the
			// -eml, when there is one, is the aligned case an operator is
			// almost always asking about.
			if d := fromDomainOfHeaders(msg.Headers); d != "" {
				msg.DKIMDomains = []string{d}
			}
		}
	}
	if o.DMARC != "" {
		msg.DMARCResult = o.DMARC
		msg.DMARCPolicy = o.DMARCPolicy
	}

	env.Msg = msg
	return env, nil
}

// spfDomainFor mirrors internal/msgauth's identity selection: the sender's
// domain, or the HELO name for a null sender.
func spfDomainFor(helo, from string) string {
	if d := ruleset.Domain(from); d != "" {
		return d
	}
	return strings.TrimSuffix(strings.TrimSpace(helo), ".")
}

func fromDomainOfHeaders(h map[string][]string) string {
	if v := h["from"]; len(v) > 0 {
		return msgauth.FromDomainOf(v[0])
	}
	return ""
}

// loadMessage populates the data-stage facts from a message file.
//
// The MIME walk is the same internal/attach the session runs, deliberately: a
// preview that inferred attachments differently from the gateway would be worse
// than no preview at all, since the whole point of `explain` is to answer "what
// will actually happen to this message".
func loadMessage(path string, msg *ruleset.MsgEnv, inline bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("-eml: %w", err)
	}
	defer f.Close()

	if err := loadMIME(path, msg, inline); err != nil {
		return err
	}

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("-eml: %w", err)
	}
	msg.Size = info.Size()

	br := bufio.NewReader(f)
	parsed, err := mail.ReadMessage(br)
	if err != nil {
		return fmt.Errorf("-eml %s: %w", path, err)
	}
	for k, v := range parsed.Header {
		msg.Headers[strings.ToLower(k)] = v
	}
	msg.ReceivedCount = len(msg.Headers["received"])

	// Line count needs the whole file; the header block has already been
	// consumed, so count what is left and add the headers back.
	sc := bufio.NewScanner(parsed.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		msg.LineCount++
	}
	for _, v := range msg.Headers {
		msg.LineCount += int64(len(v))
	}
	return nil
}

// loadMIME fills the attachment facts, in its own pass over the file because the
// header scan above consumes the reader.
//
// A malformed structure is reported but not fatal: `explain` is a diagnostic,
// and refusing to answer at all about a message the gateway would still evaluate
// (on whatever it managed to parse) would hide the very thing being diagnosed.
func loadMIME(path string, msg *ruleset.MsgEnv, inline bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("-eml: %w", err)
	}
	defer f.Close()

	res, walkErr := attach.Walk(f, attach.Options{IncludeInline: inline})
	msg.MimePartCount = res.PartCount
	msg.Attachments = make([]ruleset.Attachment, 0, len(res.Parts))
	for _, p := range res.Parts {
		msg.Attachments = append(msg.Attachments, ruleset.Attachment{
			Filename:    p.Filename,
			ContentType: p.ContentType,
			MD5:         p.MD5,
			Disposition: p.Disposition,
			Size:        p.Size,
		})
	}
	if walkErr != nil {
		fmt.Fprintf(os.Stderr,
			"  WARNING: the MIME walk stopped early (%v); %d part(s) were read\n",
			walkErr, res.PartCount)
	}
	return nil
}
