package smtpsrv

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// These tests drive the rule engine over a real SMTP conversation. They cover
// what the four-field Haraka table could not express at all, so there is no
// legacy behaviour to compare against — only the behaviour the DSL promises.

func rules(t *testing.T, f *ruleset.File) *ruleset.Ruleset {
	t.Helper()
	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Exchange":  {{Name: "e", Exchange: "127.0.0.1", Port: 25}},
		"Outbound":  {{Name: "o", Exchange: "127.0.0.1", Port: 25}},
		"BulkPool":  {{Name: "b", Exchange: "127.0.0.1", Port: 25}},
		"Quarantin": {{Name: "q", Exchange: "127.0.0.1", Port: 25}},
	})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	rs, err := ruleset.Compile(f, tbl, ruleset.DefaultSchema())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return rs
}

func relayTo(name string, prio int, m ruleset.Pred, group string) ruleset.Rule {
	return ruleset.Rule{Name: name, Priority: prio, Match: m,
		Then: []ruleset.Action{{Kind: ruleset.ActRelay, Relay: group}}}
}

func catchAll(group string) ruleset.Rule {
	return relayTo("catch all", 1000, ruleset.Pred{All: []ruleset.Pred{}}, group)
}

// send runs one full transaction and returns the end-of-DATA reply.
func (c *client) send(from string, rcpts []string, body string) (int, string) {
	c.t.Helper()
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<%s>", from)
	for _, r := range rcpts {
		c.cmd("RCPT TO:<%s>", r)
	}
	c.cmd("DATA")
	return c.sendBody(body)
}

// waitQueued collects the envelopes produced by one message.
func (h *harness) waitQueued(t *testing.T) []*queueEnvelopeView {
	t.Helper()
	select {
	case es := <-h.queued:
		out := make([]*queueEnvelopeView, 0, len(es))
		for _, e := range es {
			v := &queueEnvelopeView{UUID: e.UUID, Group: e.RelayGrp}
			for _, r := range e.Rcpts {
				v.Rcpts = append(v.Rcpts, r.Addr)
			}
			for _, hd := range e.Prepend {
				v.Headers = append(v.Headers, hd.Name+": "+hd.Value)
			}
			sort.Strings(v.Rcpts)
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for envelopes")
		return nil
	}
}

type queueEnvelopeView struct {
	UUID    string
	Group   string
	Rcpts   []string
	Headers []string
}

// --- per-recipient routing, the headline capability ------------------------

// Haraka routed the whole message by rcpt_to[0] (npRoute.js:55), so this
// message would have gone entirely to Exchange.
func TestPolicy_RecipientsSplitAcrossRelayGroups(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		relayTo("internal", 10, ruleset.Pred{Field: "rcpt.domain", Op: ruleset.OpEq, Value: "ngm.dev"}, "Exchange"),
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	code, raw := c.send("me@ngm.dev",
		[]string{"inside@ngm.dev", "outside@elsewhere.com", "also@ngm.dev"},
		"Subject: split\r\n\r\nhi")
	if code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 2 {
		t.Fatalf("expected 2 envelopes, got %d: %+v", len(envs), envs)
	}

	byGroup := map[string][]string{}
	for _, e := range envs {
		byGroup[e.Group] = e.Rcpts
	}
	if got := byGroup["Exchange"]; len(got) != 2 || got[0] != "also@ngm.dev" || got[1] != "inside@ngm.dev" {
		t.Errorf("Exchange envelope: got %v", got)
	}
	if got := byGroup["Outbound"]; len(got) != 1 || got[0] != "outside@elsewhere.com" {
		t.Errorf("Outbound envelope: got %v", got)
	}
}

// A domain glob with a dot separator: `*.partner.com` must catch the subdomain
// and leave the parent alone. `rcpt_domain: "partner.com"` could express
// neither.
func TestPolicy_SubdomainGlobRouting(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		relayTo("partner subdomains", 10,
			ruleset.Pred{Field: "rcpt.domain", Op: ruleset.OpGlob, Value: "*.partner.com"}, "Exchange"),
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("me@ngm.dev",
		[]string{"a@mx.partner.com", "b@partner.com"}, "Subject: g\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 2 {
		t.Fatalf("expected 2 envelopes, got %d: %+v", len(envs), envs)
	}
	for _, e := range envs {
		switch e.Rcpts[0] {
		case "a@mx.partner.com":
			if e.Group != "Exchange" {
				t.Errorf("mx.partner.com should match *.partner.com, went to %q", e.Group)
			}
		case "b@partner.com":
			if e.Group != "Outbound" {
				t.Errorf("partner.com must NOT match *.partner.com, went to %q", e.Group)
			}
		}
	}
}

// --- policy at the right stage ---------------------------------------------

func TestPolicy_RecipientRejectionHappensAtRCPT(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "no root", Match: ruleset.Pred{Field: "rcpt.local", Op: ruleset.OpEq, Value: "root"},
			Then: []ruleset.Action{{Kind: ruleset.ActReject, Code: 550, Enhanced: "5.1.1", Message: "No such user"}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")

	code, raw := c.cmd("RCPT TO:<root@ngm.dev>")
	if code != 550 {
		t.Errorf("the rejection must arrive at RCPT, not after DATA: got %d (%s)", code, raw)
	}
	if !strings.Contains(raw, "No such user") {
		t.Errorf("the rule's message must reach the client: %q", raw)
	}

	// The other recipients on the same transaction are unaffected.
	if code, raw := c.cmd("RCPT TO:<ops@ngm.dev>"); code != 250 {
		t.Errorf("a rejected recipient must not poison the transaction: got %d (%s)", code, raw)
	}
	c.cmd("DATA")
	if code, raw := c.sendBody("Subject: t\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 1 || len(envs[0].Rcpts) != 1 || envs[0].Rcpts[0] != "ops@ngm.dev" {
		t.Errorf("only the accepted recipient should be queued, got %+v", envs)
	}
}

func TestPolicy_SenderRejectionHappensAtMAIL(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "blocklist",
			Match: ruleset.Pred{Field: "mail.from_domain", Op: ruleset.OpEq, Value: "spam.example"},
			Then:  []ruleset.Action{{Kind: ruleset.ActReject, Code: 550, Message: "Sender not accepted"}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	if code, raw := c.cmd("MAIL FROM:<x@spam.example>"); code != 550 {
		t.Errorf("got %d (%s), want 550 at MAIL", code, raw)
	}
}

func TestPolicy_ConnectStageRejectionAnswersEHLO(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "not from loopback",
			Match: ruleset.Pred{Field: "conn.is_loopback", Op: ruleset.OpEq, Value: true},
			Then:  []ruleset.Action{{Kind: ruleset.ActReject, Code: 554, Message: "Access denied"}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	c.greet()
	code, raw := c.cmd("EHLO test.example.com")
	if code != 554 {
		t.Errorf("a connect-stage rule must refuse at EHLO: got %d (%s)", code, raw)
	}
	if !strings.Contains(raw, "Access denied") {
		t.Errorf("reply %q should carry the rule's message", raw)
	}
}

func TestPolicy_DataStageRejectionRemovesTheSpooledBody(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "no loops",
			Match: ruleset.Pred{Field: "msg.received_count", Op: ruleset.OpGt, Value: 1},
			Then:  []ruleset.Action{{Kind: ruleset.ActReject, Code: 554, Enhanced: "5.4.6", Message: "Too many hops"}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	// Two Received headers of our own plus the one we add exceeds the limit.
	code, raw := c.send("me@ngm.dev", []string{"you@ngm.dev"},
		"Received: from a\r\nReceived: from b\r\nSubject: loop\r\n\r\nhi")
	if code != 554 {
		t.Fatalf("got %d (%s), want 554", code, raw)
	}
	if !strings.Contains(raw, "Too many hops") {
		t.Errorf("reply %q should carry the rule's message", raw)
	}

	// The body is written before the data-stage rules can see its headers, so
	// a rejection has to clean up after itself or the spool grows forever.
	entries, err := os.ReadDir(filepath.Join(h.spool.Root(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a rejected message must not leave a body behind, found %d", len(entries))
	}
}

// --- actions ---------------------------------------------------------------

func TestPolicy_AddHeaderReachesTheEnvelope(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		{Name: "tag partners", Priority: 10,
			Match: ruleset.Pred{Field: "rcpt.domain", Op: ruleset.OpEq, Value: "partner.com"},
			Then: []ruleset.Action{
				{Kind: ruleset.ActAddHeader, Name: "X-NGM-Route", Value: "partner"},
				{Kind: ruleset.ActRelay, Relay: "Exchange"},
			}},
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("me@ngm.dev",
		[]string{"a@partner.com", "b@elsewhere.com"}, "Subject: h\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(envs))
	}
	for _, e := range envs {
		if e.Group == "Exchange" {
			if len(e.Headers) != 1 || e.Headers[0] != "X-NGM-Route: partner" {
				t.Errorf("partner envelope headers: got %v", e.Headers)
			}
		} else if len(e.Headers) != 0 {
			t.Errorf("the other envelope must not carry the header: got %v", e.Headers)
		}
	}
}

// Recipients that share a destination but got different headers must not be
// merged, or one of them receives a header its rule never asked for.
func TestPolicy_SameGroupDifferentHeadersStaySeparate(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		{Name: "vip", Priority: 10,
			Match: ruleset.Pred{Field: "rcpt.local", Op: ruleset.OpEq, Value: "vip"},
			Then: []ruleset.Action{
				{Kind: ruleset.ActAddHeader, Name: "X-Priority", Value: "1"},
				{Kind: ruleset.ActRelay, Relay: "Outbound"},
			}},
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("me@ngm.dev",
		[]string{"vip@ngm.dev", "normal@ngm.dev"}, "Subject: v\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 2 {
		t.Fatalf("recipients with different headers must not share an envelope, got %d: %+v", len(envs), envs)
	}
	for _, e := range envs {
		if e.Group != "Outbound" {
			t.Errorf("both go to Outbound, got %q", e.Group)
		}
	}
}

// A header added by a recipient-scoped policy rule belongs to that recipient.
// Putting it on the transaction would stamp it on everyone else's copy too.
func TestPolicy_RecipientHeadersDoNotLeakToOtherRecipients(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "mark vips",
			Match: ruleset.Pred{Field: "rcpt.local", Op: ruleset.OpEq, Value: "vip"},
			Then:  []ruleset.Action{{Kind: ruleset.ActAddHeader, Name: "X-VIP", Value: "yes"}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("me@ngm.dev",
		[]string{"vip@ngm.dev", "normal@ngm.dev"}, "Subject: h\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 2 {
		t.Fatalf("different headers must mean different envelopes, got %d: %+v", len(envs), envs)
	}
	for _, e := range envs {
		switch e.Rcpts[0] {
		case "vip@ngm.dev":
			if len(e.Headers) != 1 || e.Headers[0] != "X-VIP: yes" {
				t.Errorf("vip envelope headers: got %v", e.Headers)
			}
		case "normal@ngm.dev":
			if len(e.Headers) != 0 {
				t.Errorf("the header leaked onto normal@: got %v", e.Headers)
			}
		}
	}
}

func TestPolicy_DiscardAcceptsAndDropsTheRecipient(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "blackhole", Stage: "rcpt",
			Match: ruleset.Pred{Field: "rcpt.local", Op: ruleset.OpEq, Value: "devnull"},
			Then:  []ruleset.Action{{Kind: ruleset.ActDiscard}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	if code, raw := c.cmd("RCPT TO:<devnull@ngm.dev>"); code != 250 {
		t.Errorf("a discarded recipient is still accepted: got %d (%s)", code, raw)
	}
	c.cmd("RCPT TO:<real@ngm.dev>")
	c.cmd("DATA")
	if code, raw := c.sendBody("Subject: d\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 1 || len(envs[0].Rcpts) != 1 || envs[0].Rcpts[0] != "real@ngm.dev" {
		t.Errorf("only the real recipient should be queued, got %+v", envs)
	}
}

func TestPolicy_QuarantineIsFiledAndNotDelivered(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{{Name: "hold suspicious", Stage: "data",
			Match: ruleset.Pred{Field: "header.x-suspect", Op: ruleset.OpExists},
			Then:  []ruleset.Action{{Kind: ruleset.ActQuarantine}}}},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	code, raw := c.send("me@ngm.dev", []string{"you@ngm.dev"},
		"X-Suspect: yes\r\nSubject: q\r\n\r\nhi")
	if code != 250 {
		t.Fatalf("a quarantined message is still accepted: got %d (%s)", code, raw)
	}

	// Nothing may reach the delivery runner.
	select {
	case es := <-h.queued:
		t.Fatalf("a quarantined message must not be queued for delivery, got %d envelopes", len(es))
	case <-time.After(200 * time.Millisecond):
	}

	entries, err := os.ReadDir(filepath.Join(h.spool.Root(), "quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected one envelope in quarantine/, found %d", len(entries))
	}
	ready, _, err := h.spool.Len()
	if err != nil {
		t.Fatal(err)
	}
	if ready != 0 {
		t.Errorf("the ready queue must stay empty, found %d", ready)
	}
}

func TestPolicy_AcceptStopsFurtherPolicy(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{
		Policy: []ruleset.Rule{
			{Name: "trust the build VLAN", Priority: 10,
				Match: ruleset.Pred{Field: "conn.is_loopback", Op: ruleset.OpEq, Value: true},
				Then:  []ruleset.Action{{Kind: ruleset.ActAccept}}},
			{Name: "otherwise block this sender", Priority: 20,
				Match: ruleset.Pred{Field: "mail.from", Op: ruleset.OpEq, Value: "me@ngm.dev"},
				Then:  []ruleset.Action{{Kind: ruleset.ActReject, Code: 550, Message: "Blocked"}}},
		},
		Routes: []ruleset.Rule{catchAll("Outbound")},
	}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("me@ngm.dev", []string{"you@ngm.dev"}, "Subject: a\r\n\r\nhi"); code != 250 {
		t.Fatalf("an accepted connection must skip the later reject: got %d (%s)", code, raw)
	}
	if envs := h.waitQueued(t); len(envs) != 1 {
		t.Errorf("expected the message to be queued, got %+v", envs)
	}
}

// --- routing on facts Haraka never had -------------------------------------

func TestPolicy_RoutingOnAHeaderAndSize(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		{Name: "bulk campaigns", Priority: 20,
			Match: ruleset.Pred{All: []ruleset.Pred{
				{Field: "header.x-ngm-campaign", Op: ruleset.OpExists},
				{Field: "msg.size", Op: ruleset.OpGe, Value: 32},
			}},
			Then: []ruleset.Action{
				{Kind: ruleset.ActTag, Key: "class", Value: "bulk"},
				{Kind: ruleset.ActRelay, Relay: "BulkPool"},
			}},
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("me@ngm.dev", []string{"you@ngm.dev"},
		"X-NGM-Campaign: spring\r\nSubject: bulk\r\n\r\n"+strings.Repeat("x", 200)); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}
	envs := h.waitQueued(t)
	if len(envs) != 1 || envs[0].Group != "BulkPool" {
		t.Fatalf("expected the bulk pool, got %+v", envs)
	}

	c2 := dialClient(t, h.addr)
	if code, _ := c2.send("me@ngm.dev", []string{"you@ngm.dev"}, "Subject: normal\r\n\r\nhi"); code != 250 {
		t.Fatal("second message should be accepted")
	}
	envs2 := h.waitQueued(t)
	if len(envs2) != 1 || envs2[0].Group != "Outbound" {
		t.Fatalf("without the header it must fall through, got %+v", envs2)
	}
}

func TestPolicy_NullSenderRoutesToItsOwnPool(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		relayTo("bounces", 10,
			ruleset.Pred{Field: "mail.is_null_sender", Op: ruleset.OpEq, Value: true}, "BulkPool"),
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("", []string{"you@ngm.dev"}, "Subject: bounce\r\n\r\nhi"); code != 250 {
		t.Fatalf("a bounce must be accepted: got %d (%s)", code, raw)
	}
	envs := h.waitQueued(t)
	if len(envs) != 1 || envs[0].Group != "BulkPool" {
		t.Errorf("the null sender should route to its own pool, got %+v", envs)
	}
}

func TestPolicy_RoutingOnSourceCIDR(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		relayTo("loopback traffic", 10,
			ruleset.Pred{Field: "conn.remote_ip", Op: ruleset.OpInCIDR, Values: []any{"127.0.0.0/8", "::1/128"}}, "Exchange"),
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	if code, raw := c.send("me@ngm.dev", []string{"you@elsewhere.com"}, "Subject: c\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}
	envs := h.waitQueued(t)
	if len(envs) != 1 || envs[0].Group != "Exchange" {
		t.Errorf("the test client connects from loopback, so the CIDR rule must win: %+v", envs)
	}
}

// The default action is what Haraka's DENYSOFT "No route found" became.
func TestPolicy_UnroutableMailTempfailsAfterData(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		relayTo("ngm only", 10, ruleset.Pred{Field: "rcpt.domain", Op: ruleset.OpEq, Value: "ngm.dev"}, "Exchange"),
	}}))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	// The recipient stays accepted; the refusal comes after DATA, matching
	// hook_get_mx timing.
	if code, raw := c.cmd("RCPT TO:<nobody@elsewhere.com>"); code != 250 {
		t.Fatalf("RCPT should be accepted: got %d (%s)", code, raw)
	}
	c.cmd("DATA")
	code, raw := c.sendBody("Subject: t\r\n\r\nhi")
	if code < 400 || code >= 500 {
		t.Errorf("unroutable mail must tempfail, got %d (%s)", code, raw)
	}
	if !strings.Contains(raw, "No route found") {
		t.Errorf("reply %q should say why", raw)
	}
}

// A tag set while evaluating one recipient must not change the answer for the
// next, or routing would depend on the order recipients happened to arrive in.
func TestPolicy_TagsDoNotLeakBetweenRecipients(t *testing.T) {
	h := startServerWithRules(t, rules(t, &ruleset.File{Routes: []ruleset.Rule{
		{Name: "mark vips", Priority: 10,
			Match: ruleset.Pred{Field: "rcpt.local", Op: ruleset.OpEq, Value: "vip"},
			Then:  []ruleset.Action{{Kind: ruleset.ActTag, Key: "vip", Value: "yes"}}},
		relayTo("vip route", 20, ruleset.Pred{Field: "tag.vip", Op: ruleset.OpEq, Value: "yes"}, "Exchange"),
		catchAll("Outbound"),
	}}))

	c := dialClient(t, h.addr)
	// vip first, so a leaking tag would drag the second recipient with it.
	if code, raw := c.send("me@ngm.dev",
		[]string{"vip@ngm.dev", "normal@ngm.dev"}, "Subject: t\r\n\r\nhi"); code != 250 {
		t.Fatalf("DATA: got %d (%s)", code, raw)
	}

	envs := h.waitQueued(t)
	if len(envs) != 2 {
		t.Fatalf("expected 2 envelopes, got %d: %+v", len(envs), envs)
	}
	for _, e := range envs {
		switch e.Rcpts[0] {
		case "vip@ngm.dev":
			if e.Group != "Exchange" {
				t.Errorf("vip should take the tagged route, got %q", e.Group)
			}
		case "normal@ngm.dev":
			if e.Group != "Outbound" {
				t.Errorf("the tag leaked: normal@ went to %q", e.Group)
			}
		}
	}
}
