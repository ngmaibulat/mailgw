package main

import (
	"bufio"
	"fmt"
	"net/mail"
	"net/netip"
	"os"
	"strings"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// buildEnv assembles the fact base `explain` evaluates against, filling in only
// what the requested stage would actually know.
func buildEnv(ip, helo, from, rcpt string, tlsOn bool, eml string, at ruleset.Stage) (*ruleset.Env, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return nil, fmt.Errorf("-ip %q: %w", ip, err)
	}

	env := &ruleset.Env{
		Stage: at,
		Conn:  &ruleset.ConnEnv{RemoteIP: addr.Unmap(), RemotePort: 54321},
	}
	if at >= ruleset.StageHelo {
		env.Helo = &ruleset.HeloEnv{Name: helo, TLS: tlsOn}
		if tlsOn {
			env.Helo.TLSVersion = "TLS1.3"
		}
	}
	if at >= ruleset.StageMail {
		env.Mail = &ruleset.MailEnv{From: from}
	}
	if at >= ruleset.StageRcpt {
		env.Rcpt = &ruleset.RcptEnv{To: rcpt, Index: 1, CountSoFar: 0}
	}
	if at < ruleset.StageData {
		return env, nil
	}

	msg := &ruleset.MsgEnv{
		RcptCount:   1,
		RcptDomains: []string{strings.ToLower(ruleset.Domain(rcpt))},
		Headers:     map[string][]string{},
	}
	if eml != "" {
		if err := loadMessage(eml, msg); err != nil {
			return nil, err
		}
	}
	env.Msg = msg
	return env, nil
}

// loadMessage populates the data-stage facts from a message file.
//
// Attachment fields are deliberately left empty: `check` and `serve` both warn
// that they are not populated yet, and `explain` inventing values would make it
// disagree with the live gateway.
func loadMessage(path string, msg *ruleset.MsgEnv) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("-eml: %w", err)
	}
	defer f.Close()

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
