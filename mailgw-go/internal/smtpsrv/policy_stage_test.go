package smtpsrv

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// Regression tests for M9.1: a recipient-scoped, data-stage POLICY rule was
// never evaluated for a recipient whose ROUTE had already resolved at RCPT,
// because split() gated the policy pass on `decision == nil`.
//
// Both directions matter. The bypass test alone can be made to pass by
// disabling early routing altogether, which would throw away the RCPT-timing
// property M2 exists for — the control test is what catches that.

func compileRulesYAML(t *testing.T, y string) *ruleset.Ruleset {
	t.Helper()

	p := filepath.Join(t.TempDir(), "routing.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatalf("write rules: %v", err)
	}
	f, err := ruleset.LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Outbound": {{Name: "Default", Exchange: "127.0.0.1", Port: 25}},
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

// send runs one transaction and returns the end-of-DATA reply.
func send(t *testing.T, h *harness, rcpt, body string) (int, string) {
	t.Helper()

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")
	if code, raw := c.cmd("RCPT TO:<%s>", rcpt); code != 250 {
		t.Fatalf("RCPT TO:<%s>: got %d %q, want 250 — the rule under test needs DATA", rcpt, code, raw)
	}
	c.cmd("DATA")
	return c.sendBody(body)
}

// The route resolves at RCPT (`always` is a connect-stage predicate), so the
// cached-decision path is the one exercised.
const rulesEarlyRoute = `
version: 1
policy:
  - name: block-secret-subject-to-finance
    match:
      all:
        - {field: rcpt.domain, op: eq, value: finance.example}
        - {field: header.subject, op: contains, value: secret}
    then:
      - {action: reject, code: 550, message: "blocked by policy"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

func TestDataStagePolicy_RunsWhenRouteResolvedAtRcpt(t *testing.T) {
	rs := compileRulesYAML(t, rulesEarlyRoute)

	// Guard the premise: if this rule ever stops compiling as
	// rcpt-scoped + data-stage, the test is no longer covering the bug.
	var checked bool
	for _, r := range rs.Policy {
		if r.Name != "block-secret-subject-to-finance" {
			continue
		}
		checked = true
		if r.Stage != ruleset.StageData {
			t.Fatalf("rule stage: got %s, want data", r.Stage)
		}
		if !r.RcptScoped {
			t.Fatal("rule should be recipient-scoped (it reads rcpt.domain)")
		}
	}
	if !checked {
		t.Fatal("the policy rule did not compile")
	}

	h := startServerWithRules(t, rs)
	code, raw := send(t, h, "bob@finance.example", "Subject: a secret memo\r\n\r\nbody\r\n")

	if code != 550 {
		t.Errorf("end-of-DATA: got %d %q, want 550 — the data-stage policy rule was skipped", code, raw)
	}
	if !strings.Contains(raw, "blocked by policy") {
		t.Errorf("reply %q does not carry the rule's message", raw)
	}
}

// Control: same policy rule, but a higher-priority route reads a header, so
// Route reports "undecided" at RCPT and split() takes the uncached path. This
// already worked before the fix; it fails if early routing is disabled.
const rulesLateRoute = `
version: 1
policy:
  - name: block-secret-subject-to-finance
    match:
      all:
        - {field: rcpt.domain, op: eq, value: finance.example}
        - {field: header.subject, op: contains, value: secret}
    then:
      - {action: reject, code: 550, message: "blocked by policy"}
routes:
  - name: needs-a-header
    priority: 1
    match: {field: header.x-route-me, op: exists}
    then:
      - {action: relay, relay: Outbound}
  - name: catch-all
    priority: 2
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

func TestDataStagePolicy_RunsWhenRouteDefersToData(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesLateRoute))
	code, raw := send(t, h, "bob@finance.example", "Subject: a secret memo\r\n\r\nbody\r\n")

	if code != 550 {
		t.Errorf("end-of-DATA: got %d %q, want 550", code, raw)
	}
	if !strings.Contains(raw, "blocked by policy") {
		t.Errorf("reply %q does not carry the rule's message", raw)
	}
}

// A non-matching recipient must still be delivered — the fix must not turn the
// policy pass into a blanket refusal.
func TestDataStagePolicy_NonMatchingRecipientStillRelays(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesEarlyRoute))
	code, raw := send(t, h, "bob@other.example", "Subject: a secret memo\r\n\r\nbody\r\n")

	if code != 250 {
		t.Fatalf("end-of-DATA: got %d %q, want 250", code, raw)
	}
	if !strings.Contains(strings.ToLower(raw), "queued") {
		t.Errorf("reply %q should report the message queued", raw)
	}
}

// Early routing must still happen: a rule that can decide at RCPT is what M2's
// stage inference exists for, and the fix must not have made every decision
// wait for DATA. Asserted through observable behaviour — a rcpt-stage reject
// arrives on the RCPT TO line, not at end-of-DATA.
const rulesRcptStageReject = `
version: 1
policy:
  - name: no-mail-to-blocked-domain
    match: {field: rcpt.domain, op: eq, value: blocked.example}
    then:
      - {action: reject, code: 550, message: "recipient refused"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

func TestRcptStagePolicy_StillFiresAtRcpt(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesRcptStageReject))

	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO probe.invalid")
	c.cmd("MAIL FROM:<sender@example.com>")

	code, raw := c.cmd("RCPT TO:<bob@blocked.example>")
	if code != 550 {
		t.Errorf("RCPT TO: got %d %q, want 550 at the RCPT stage", code, raw)
	}
	if !strings.Contains(raw, "recipient refused") {
		t.Errorf("reply %q does not carry the rule's message", raw)
	}
}

// Tags set at RCPT must survive into the data-stage pass: the per-recipient env
// is a copy, so without rcptState.tags a `tag.*` rule at DATA would read an
// empty map. The route resolves at RCPT, so this covers the cached path.
const rulesTagCarriedForward = `
version: 1
policy:
  - name: tag-finance-recipients
    priority: 1
    match: {field: rcpt.domain, op: eq, value: finance.example}
    then:
      - {action: tag, key: dept, value: finance}
  - name: block-tagged-with-secret-subject
    priority: 2
    match:
      all:
        # rcpt.local is what makes this rule recipient-scoped. Without a rcpt.*
        # field it would compile as message-scoped and be evaluated against the
        # session env, which never sees a per-recipient tag at all.
        - {field: rcpt.local, op: exists}
        - {field: tag.dept, op: eq, value: finance}
        - {field: header.subject, op: contains, value: secret}
    then:
      - {action: reject, code: 550, message: "tagged and blocked"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

func TestDataStagePolicy_CarriesRcptStageTagsForward(t *testing.T) {
	h := startServerWithRules(t, compileRulesYAML(t, rulesTagCarriedForward))
	code, raw := send(t, h, "bob@finance.example", "Subject: a secret memo\r\n\r\nbody\r\n")

	if code != 550 {
		t.Errorf("end-of-DATA: got %d %q, want 550 — the RCPT-stage tag did not reach the data-stage rule", code, raw)
	}
	if !strings.Contains(raw, "tagged and blocked") {
		t.Errorf("reply %q does not carry the rule's message", raw)
	}
}

// M9.1's last bullet — `explain -stage data` must report the verdict the
// session actually reaches — was confirmed by hand, and one example is not
// enough: the session runs the data-stage policy in two passes (Data() takes
// the message-scoped rules for the whole message, split() the recipient-scoped
// ones per recipient), while Explain walked all of them in one priority-ordered
// pass. Where the two disagree, `explain` describes something that cannot
// happen, which is worse than no preview at all.
//
// The rules below are built so the orderings differ: the recipient-scoped rule
// has the lower priority number, so a single walk reports it, while the session
// answers with the message-scoped one. The two predicates are independent, so
// the same ruleset also produces the control case — a subject that only the
// recipient-scoped rule cares about.
const rulesBothScopesAtData = `
version: 1
policy:
  - name: rcpt-scoped-block
    priority: 10
    match:
      all:
        - {field: rcpt.domain, op: eq, value: finance.example}
        - {field: header.subject, op: contains, value: secret}
    then:
      - {action: reject, code: 550, message: "recipient-scoped rule"}
  - name: message-scoped-block
    priority: 20
    match: {field: header.subject, op: contains, value: confidential}
    then:
      - {action: reject, code: 551, message: "message-scoped rule"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

// explainEnv builds the fact base the way cmd/mailgw-go's buildEnv does, so
// this pins what `explain` really evaluates rather than a shape private to the
// test.
func explainEnv(rcpt, subject string) *ruleset.Env {
	env := &ruleset.Env{
		Stage: ruleset.StageData,
		Conn:  &ruleset.ConnEnv{RemoteIP: netip.MustParseAddr("127.0.0.1"), RemotePort: 54321},
		Helo:  &ruleset.HeloEnv{Name: "probe.invalid"},
		Mail:  &ruleset.MailEnv{From: "sender@example.com"},
		Rcpt:  &ruleset.RcptEnv{To: rcpt, Index: 1, CountSoFar: 0},
		Msg: &ruleset.MsgEnv{
			RcptCount:   1,
			RcptDomains: []string{strings.ToLower(ruleset.Domain(rcpt))},
			Headers:     map[string][]string{"subject": {subject}},
		},
	}
	return env
}

func TestExplain_AgreesWithTheSessionAtDataStage(t *testing.T) {
	rs := compileRulesYAML(t, rulesBothScopesAtData)

	// Guard the premise: the walk orderings only diverge while one rule is
	// recipient-scoped, the other is not, and the recipient-scoped one sorts
	// first.
	var scopes []bool
	for _, r := range rs.Policy {
		if r.Stage != ruleset.StageData {
			t.Fatalf("rule %q: stage %s, want data", r.Name, r.Stage)
		}
		scopes = append(scopes, r.RcptScoped)
	}
	if len(scopes) != 2 || !scopes[0] || scopes[1] {
		t.Fatalf("policy rules should compile as [rcpt-scoped, message-scoped], got %v", scopes)
	}

	const subject = "a secret and confidential memo" // matches both rules

	h := startServerWithRules(t, rs)
	code, raw := send(t, h, "bob@finance.example", "Subject: "+subject+"\r\n\r\nbody\r\n")

	// The session answers from the message-scoped rule: Data() evaluates those
	// for the whole message before split() ever reaches the per-recipient pass.
	if code != 551 {
		t.Fatalf("end-of-DATA: got %d %q, want 551 from the message-scoped rule", code, raw)
	}

	ex := rs.Explain(explainEnv("bob@finance.example", subject), ruleset.StageData)
	if ex.Policy.Action == nil {
		t.Fatal("explain reported no policy verdict, but the session rejected the message")
	}
	if ex.Policy.Rule != "message-scoped-block" || ex.Policy.Action.Code != code {
		t.Errorf("explain says %q %d, the session answered %d — explain is not a truthful preview",
			ex.Policy.Rule, ex.Policy.Action.Code, code)
	}
}

// The control: a subject only the recipient-scoped rule matches, so that rule is
// genuinely the verdict and `explain` must still say so. Without this, "always
// prefer the message-scoped rule" would pass the test above.
func TestExplain_StillReportsARecipientScopedRule(t *testing.T) {
	rs := compileRulesYAML(t, rulesBothScopesAtData)

	const subject = "a secret memo" // the message-scoped rule wants "confidential"

	h := startServerWithRules(t, rs)
	code, raw := send(t, h, "bob@finance.example", "Subject: "+subject+"\r\n\r\nbody\r\n")

	// Every recipient was refused after the message was accepted, so Data
	// answers with the first failure — see the len(envelopes) == 0 branch.
	if code != 550 {
		t.Fatalf("end-of-DATA: got %d %q, want 550 from the recipient-scoped rule", code, raw)
	}

	ex := rs.Explain(explainEnv("bob@finance.example", subject), ruleset.StageData)
	if ex.Policy.Action == nil {
		t.Fatal("explain reported no policy verdict, but the session refused the recipient")
	}
	if ex.Policy.Rule != "rcpt-scoped-block" || ex.Policy.Action.Code != code {
		t.Errorf("explain says %q %d, the session answered %d",
			ex.Policy.Rule, ex.Policy.Action.Code, code)
	}
}
