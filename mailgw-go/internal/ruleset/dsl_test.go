package ruleset

import (
	"net/netip"
	"strings"
	"testing"
)

// --- helpers ---------------------------------------------------------------

func compileFile(t *testing.T, f *File, groups ...string) *Ruleset {
	t.Helper()
	if len(groups) == 0 {
		groups = []string{"Exchange", "Outbound"}
	}
	rs, err := Compile(f, relayTable(t, groups...), DefaultSchema())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return rs
}

func mustNotCompile(t *testing.T, f *File, wantSubstr string) {
	t.Helper()
	_, err := Compile(f, relayTable(t, "Exchange", "Outbound"), DefaultSchema())
	if err == nil {
		t.Fatalf("expected a compile error mentioning %q, got none", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error %q does not mention %q", err, wantSubstr)
	}
}

func relayRule(name string, prio int, m Pred, group string) Rule {
	return Rule{Name: name, Priority: prio, Match: m, Then: []Action{{Kind: ActRelay, Relay: group}}}
}

// matchOne compiles a single predicate and evaluates it, so operator tests do
// not have to build a whole rule file each time.
func matchOne(t *testing.T, p Pred, env *Env) bool {
	t.Helper()
	n, err := compilePred(&p, DefaultSchema(), "test")
	if err != nil {
		t.Fatalf("compile %s: %v", p.describe(), err)
	}
	return n.Match(env)
}

func fullEnv() *Env {
	return &Env{
		Stage: StageData,
		Conn: &ConnEnv{
			RemoteIP: netip.MustParseAddr("10.20.0.5"), RemotePort: 41234,
			LocalIP: netip.MustParseAddr("10.0.0.1"), LocalPort: 2525,
		},
		Helo: &HeloEnv{Name: "MX.Partner-A.com", TLS: true, TLSVersion: "TLS1.3"},
		Mail: &MailEnv{From: "Build@NGM.dev", SizeDeclared: 2048, Body: "8BITMIME"},
		Rcpt: &RcptEnv{To: "Ops@Mail.NGM.dev", Index: 2, CountSoFar: 1},
		Msg: &MsgEnv{
			Size: 1 << 21, LineCount: 400, ReceivedCount: 3, MimePartCount: 4,
			Headers: map[string][]string{
				"subject":        {"Q3 numbers"},
				"received":       {"a", "b", "c"},
				"x-ngm-campaign": {"spring"},
				"x-spam-status":  {"No"},
				"x-empty":        {""},
			},
			Attachments: []Attachment{
				{Filename: "report.q3.pdf", ContentType: "application/pdf", MD5: "abc", Size: 1000, Disposition: "attachment"},
				{Filename: "notes.TXT", ContentType: "text/plain", MD5: "def", Size: 20, Disposition: "inline"},
			},
			RcptCount: 2, RcptDomains: []string{"ngm.dev", "partner-a.com"},
		},
	}
}

// --- operators × kinds -----------------------------------------------------

func TestOps_StringComparisonsAreCaseInsensitiveByDefault(t *testing.T) {
	env := fullEnv()
	cases := []struct {
		name string
		pred Pred
		want bool
	}{
		{"eq folds case", Pred{Field: "mail.from", Op: OpEq, Value: "build@ngm.dev"}, true},
		{"ne is the negation", Pred{Field: "mail.from", Op: OpNe, Value: "build@ngm.dev"}, false},
		{"contains folds case", Pred{Field: "helo.name", Op: OpContains, Value: "partner-a"}, true},
		{"prefix folds case", Pred{Field: "helo.name", Op: OpPrefix, Value: "mx."}, true},
		{"suffix folds case", Pred{Field: "rcpt.domain", Op: OpSuffix, Value: "NGM.DEV"}, true},
		{"in folds case", Pred{Field: "mail.from_domain", Op: OpIn, Values: []any{"other.com", "NGM.dev"}}, true},
		{"regex folds case", Pred{Field: "mail.from", Op: OpRegex, Value: "^build@"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchOne(t, c.pred, env); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestOps_CIOverrideMakesComparisonExact(t *testing.T) {
	env := fullEnv()
	no := false
	if matchOne(t, Pred{Field: "mail.from", Op: OpEq, Value: "build@ngm.dev", CI: &no}, env) {
		t.Error("with ci:false, 'Build@NGM.dev' must not equal 'build@ngm.dev'")
	}
	if !matchOne(t, Pred{Field: "mail.from", Op: OpEq, Value: "Build@NGM.dev", CI: &no}, env) {
		t.Error("with ci:false, the exact string must still match")
	}
}

func TestOps_NumericComparisons(t *testing.T) {
	env := fullEnv() // msg.size == 2097152
	cases := []struct {
		op   Op
		val  any
		want bool
	}{
		{OpGt, 1048576, true},
		{OpGe, 2097152, true},
		{OpLt, 2097152, false},
		{OpLe, 2097152, true},
		{OpEq, 2097152, true},
	}
	for _, c := range cases {
		if got := matchOne(t, Pred{Field: "msg.size", Op: c.op, Value: c.val}, env); got != c.want {
			t.Errorf("msg.size %s %v: got %v, want %v", c.op, c.val, got, c.want)
		}
	}
}

func TestOps_IPAndCIDR(t *testing.T) {
	env := fullEnv() // conn.remote_ip == 10.20.0.5
	if !matchOne(t, Pred{Field: "conn.remote_ip", Op: OpInCIDR, Values: []any{"10.20.0.0/16"}}, env) {
		t.Error("10.20.0.5 must be inside 10.20.0.0/16")
	}
	if matchOne(t, Pred{Field: "conn.remote_ip", Op: OpInCIDR, Values: []any{"10.21.0.0/16"}}, env) {
		t.Error("10.20.0.5 must not be inside 10.21.0.0/16")
	}
	if !matchOne(t, Pred{Field: "conn.remote_ip", Op: OpEq, Value: "10.20.0.5"}, env) {
		t.Error("exact IP equality must work")
	}
	if !matchOne(t, Pred{Field: "conn.remote_ip", Op: OpIn, Values: []any{"1.2.3.4", "10.20.0.5"}}, env) {
		t.Error("`in` over IPs must work")
	}
}

// A v4-mapped v6 address and its v4 form are the same host, and an allowlist
// that treats them differently is a bypass waiting to happen.
func TestOps_IPv4MappedIsEquivalent(t *testing.T) {
	env := fullEnv()
	env.Conn.RemoteIP = netip.MustParseAddr("::ffff:10.20.0.5")
	if !matchOne(t, Pred{Field: "conn.remote_ip", Op: OpInCIDR, Values: []any{"10.20.0.0/16"}}, env) {
		t.Error("::ffff:10.20.0.5 must be treated as 10.20.0.5")
	}
	if !matchOne(t, Pred{Field: "conn.remote_ip", Op: OpEq, Value: "10.20.0.5"}, env) {
		t.Error("::ffff:10.20.0.5 must equal 10.20.0.5")
	}
}

func TestOps_ExistsAndEmpty(t *testing.T) {
	env := fullEnv()
	cases := []struct {
		name string
		pred Pred
		want bool
	}{
		{"present header exists", Pred{Field: "header.x-ngm-campaign", Op: OpExists}, true},
		{"absent header does not exist", Pred{Field: "header.x-nope", Op: OpExists}, false},
		{"absent header is empty", Pred{Field: "header.x-nope", Op: OpEmpty}, true},
		{"header with an empty value is empty", Pred{Field: "header.x-empty", Op: OpEmpty}, true},
		{"header_count of an absent header is zero", Pred{Field: "header_count.x-nope", Op: OpEq, Value: 0}, true},
		{"header_count counts repeats", Pred{Field: "header_count.received", Op: OpEq, Value: 3}, true},
		{"unset auth user is empty", Pred{Field: "auth.user", Op: OpEmpty}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchOne(t, c.pred, env); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A field that is simply not known yet must never match — otherwise a rule
// would fire against a stage that has not happened.
func TestOps_MissingFieldMatchesNothingButEmpty(t *testing.T) {
	env := &Env{Stage: StageConnect, Conn: &ConnEnv{RemoteIP: netip.MustParseAddr("10.0.0.1")}}
	if matchOne(t, Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}, env) {
		t.Error("a rcpt field must not match before RCPT")
	}
	if matchOne(t, Pred{Field: "rcpt.domain", Op: OpNe, Value: "ngm.dev"}, env) {
		t.Error("`ne` against a missing field must be false, not vacuously true")
	}
	if !matchOne(t, Pred{Field: "rcpt.domain", Op: OpEmpty}, env) {
		t.Error("a missing field is empty")
	}
}

// --- list fields: existential by default, universal under `every` ----------

func TestLists_LeafIsExistential(t *testing.T) {
	env := fullEnv()
	if !matchOne(t, Pred{Field: "attachment.filename", Op: OpGlob, Value: "*.pdf"}, env) {
		t.Error("a leaf over a list must match if any element does")
	}
	if !matchOne(t, Pred{Field: "attachment.size", Op: OpGt, Value: 500}, env) {
		t.Error("numeric list fields are existential too")
	}
}

func TestLists_NotOfALeafMeansNoElementMatches(t *testing.T) {
	env := fullEnv()
	// This is the shape an operator writes for a blocklist, and the reason
	// leaves are existential: `not(some is .exe)` == `no attachment is .exe`.
	inner := Pred{Field: "attachment.filename", Op: OpGlob, Value: "*.exe"}
	if !matchOne(t, Pred{Not: &inner}, env) {
		t.Error("no attachment is a .exe, so not(...) must hold")
	}

	env.Msg.Attachments = append(env.Msg.Attachments, Attachment{Filename: "setup.exe"})
	if matchOne(t, Pred{Not: &inner}, env) {
		t.Error("once an .exe is present, not(...) must fail")
	}
}

func TestLists_EveryIsUniversal(t *testing.T) {
	env := fullEnv()
	inner := Pred{Field: "attachment.disposition", Op: OpEq, Value: "attachment"}
	if matchOne(t, Pred{Every: &inner}, env) {
		t.Error("one part is inline, so `every` must fail")
	}

	env.Msg.Attachments = []Attachment{{Disposition: "attachment"}, {Disposition: "Attachment"}}
	if !matchOne(t, Pred{Every: &inner}, env) {
		t.Error("with every part an attachment, `every` must hold")
	}
}

func TestLists_EveryIsVacuouslyTrueOnAnEmptyList(t *testing.T) {
	env := fullEnv()
	env.Msg.Attachments = nil
	inner := Pred{Field: "attachment.filename", Op: OpGlob, Value: "*.exe"}
	if !matchOne(t, Pred{Every: &inner}, env) {
		t.Error("`every` over no elements is vacuously true")
	}
	if matchOne(t, inner, env) {
		t.Error("an existential leaf over no elements is false")
	}
}

func TestLists_EveryRejectsScalarFieldsAndCombinators(t *testing.T) {
	sc := DefaultSchema()

	scalar := Pred{Field: "mail.from", Op: OpEq, Value: "a@b.com"}
	if _, err := compilePred(&Pred{Every: &scalar}, sc, "t"); err == nil {
		t.Error("`every` over a single-valued field must be a config error")
	}
	combi := Pred{All: []Pred{scalar}}
	if _, err := compilePred(&Pred{Every: &combi}, sc, "t"); err == nil {
		t.Error("`every` over a combinator must be a config error")
	}
}

// --- combinators -----------------------------------------------------------

func TestCombinators_EmptyAllIsTrueEmptyAnyIsFalse(t *testing.T) {
	env := fullEnv()
	if !matchOne(t, Pred{All: []Pred{}}, env) {
		t.Error("`all: []` is the catch-all and must always match")
	}
	if matchOne(t, Pred{Any: []Pred{}}, env) {
		t.Error("`any: []` must never match")
	}
}

func TestCombinators_RejectMultipleBranchKeys(t *testing.T) {
	inner := Pred{Field: "mail.from", Op: OpExists}
	p := Pred{All: []Pred{inner}, Any: []Pred{inner}}
	if _, err := compilePred(&p, DefaultSchema(), "t"); err == nil {
		t.Error("a node with two branch keys must be rejected, not silently resolved")
	}
}

func TestCombinators_Nested(t *testing.T) {
	env := fullEnv()
	// The "block executables leaving the build VLAN" shape from the design.
	notFrom := Pred{Field: "mail.from", Op: OpIn, Values: []any{"build@ngm.dev", "ci@ngm.dev"}}
	p := Pred{All: []Pred{
		{Field: "conn.remote_ip", Op: OpInCIDR, Values: []any{"10.20.0.0/16"}},
		{Any: []Pred{
			{Field: "attachment.filename", Op: OpGlob, Value: "*.{exe,scr}"},
			{Field: "attachment.content_type", Op: OpEq, Value: "application/pdf"},
		}},
		{Not: &notFrom},
	}}
	if matchOne(t, p, env) {
		t.Error("the sender is on the exempt list, so the rule must not fire")
	}

	env.Mail.From = "someone@ngm.dev"
	if !matchOne(t, p, env) {
		t.Error("a non-exempt sender from the build VLAN with a matching part must fire")
	}
}

// --- stage inference -------------------------------------------------------

func TestStages_InferredFromTheFieldsUsed(t *testing.T) {
	cases := []struct {
		pred Pred
		want Stage
	}{
		{Pred{Field: "conn.remote_ip", Op: OpExists}, StageConnect},
		{Pred{Field: "helo.name", Op: OpExists}, StageHelo},
		{Pred{Field: "mail.from", Op: OpExists}, StageMail},
		{Pred{Field: "rcpt.domain", Op: OpExists}, StageRcpt},
		{Pred{Field: "header.subject", Op: OpExists}, StageData},
		// A combination sits at the latest stage any of its fields needs.
		{Pred{All: []Pred{
			{Field: "conn.remote_ip", Op: OpExists},
			{Field: "rcpt.domain", Op: OpExists},
		}}, StageRcpt},
	}
	for _, c := range cases {
		n, err := compilePred(&c.pred, DefaultSchema(), "t")
		if err != nil {
			t.Fatalf("%s: %v", c.pred.describe(), err)
		}
		if n.stage != c.want {
			t.Errorf("%s: stage %s, want %s", c.pred.describe(), n.stage, c.want)
		}
	}
}

func TestStages_ExplicitStageMayDelayButNotAdvance(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		{Name: "late", Stage: "data", Match: Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"},
			Then: []Action{{Kind: ActRelay, Relay: "Exchange"}}},
	}})
	if got := rs.Routes[0].Stage; got != StageData {
		t.Errorf("explicit stage must be honoured: got %s", got)
	}

	mustNotCompile(t, &File{Routes: []Rule{
		{Name: "too early", Stage: "connect", Match: Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"},
			Then: []Action{{Kind: ActRelay, Relay: "Exchange"}}},
	}}, "earlier than")
}

func TestStages_RcptScopedRulesAreFlagged(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("by rcpt", 10, Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}, "Exchange"),
		relayRule("by size", 20, Pred{Field: "msg.size", Op: OpGt, Value: 10}, "Outbound"),
	}})
	if !rs.Routes[0].RcptScoped {
		t.Error("a rule reading rcpt.domain must be evaluated per recipient")
	}
	if rs.Routes[1].RcptScoped {
		t.Error("a rule reading only msg.size is message-scoped")
	}
}

// --- rule ordering and routing --------------------------------------------

func TestRoute_PriorityWinsThenFileOrder(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("late but low priority", 10, Pred{All: []Pred{}}, "Exchange"),
		relayRule("first but high priority", 5, Pred{All: []Pred{}}, "Outbound"),
	}})
	if rs.Routes[0].Name != "first but high priority" {
		t.Fatalf("rules must sort by ascending priority, got %q first", rs.Routes[0].Name)
	}

	d, ok := rs.Route(EnvForPair("a@x.com", "b@y.com"), StageData)
	if !ok {
		t.Fatal("routing must decide at data stage")
	}
	if g, _ := d.Relay(); g != "Outbound" {
		t.Errorf("the lowest priority number must win, got %q", g)
	}
}

func TestRoute_TiesKeepFileOrder(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("first", 0, Pred{All: []Pred{}}, "Exchange"),
		relayRule("second", 0, Pred{All: []Pred{}}, "Outbound"),
	}})
	d, _ := rs.Route(EnvForPair("a@x.com", "b@y.com"), StageData)
	if g, _ := d.Relay(); g != "Exchange" {
		t.Errorf("equal priorities must keep file order, got %q", g)
	}
}

func TestRoute_DefaultActionAppliesWhenNothingMatches(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("ngm only", 10, Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}, "Exchange"),
	}})
	d, ok := rs.Route(EnvForPair("a@x.com", "b@elsewhere.com"), StageData)
	if !ok {
		t.Fatal("data stage must always decide")
	}
	// Parity with npRoute.js:65 — a missing route holds mail rather than
	// bouncing it.
	if d.Action.Kind != ActTempfail || d.Action.Code != 451 {
		t.Errorf("default must be a 451 tempfail, got %+v", d.Action)
	}
	if d.Rule != "" {
		t.Errorf("the default is not a rule, got %q", d.Rule)
	}
}

// The prefix-stability property: a decision made early must equal the decision
// the final stage would make.
func TestRoute_DefersWhenAHigherPriorityRuleNeedsALaterStage(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("big mail goes bulk", 10, Pred{Field: "msg.size", Op: OpGt, Value: 100}, "Outbound"),
		relayRule("ngm.dev to exchange", 20, Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}, "Exchange"),
	}})

	env := EnvForPair("a@x.com", "b@ngm.dev")
	if _, ok := rs.Route(env, StageRcpt); ok {
		t.Fatal("a data-stage rule outranks the rcpt-stage one, so RCPT must not decide")
	}

	env.Msg = &MsgEnv{Size: 1000}
	d, ok := rs.Route(env, StageData)
	if !ok {
		t.Fatal("data must decide")
	}
	if g, _ := d.Relay(); g != "Outbound" {
		t.Errorf("the higher-priority data rule must win, got %q", g)
	}
}

func TestRoute_DecidesEarlyWhenNothingOutranksIt(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("ngm.dev to exchange", 10, Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}, "Exchange"),
		relayRule("big mail goes bulk", 20, Pred{Field: "msg.size", Op: OpGt, Value: 100}, "Outbound"),
	}})

	env := EnvForPair("a@x.com", "b@ngm.dev")
	d, ok := rs.Route(env, StageRcpt)
	if !ok {
		t.Fatal("nothing above it needs DATA, so RCPT must decide")
	}
	if g, _ := d.Relay(); g != "Exchange" {
		t.Errorf("got %q", g)
	}

	// And the same answer at DATA, which is what makes caching the early
	// decision sound.
	env.Msg = &MsgEnv{Size: 1000}
	later, _ := rs.Route(env, StageData)
	if lg, _ := later.Relay(); lg != "Exchange" {
		t.Errorf("the decision must be stable across stages, got %q at data", lg)
	}
}

func TestRoute_DisabledRulesAreIgnored(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		{Name: "off", Priority: 1, Disabled: true, Match: Pred{All: []Pred{}},
			Then: []Action{{Kind: ActRelay, Relay: "Exchange"}}},
		relayRule("on", 2, Pred{All: []Pred{}}, "Outbound"),
	}})
	if len(rs.Routes) != 1 {
		t.Fatalf("a disabled rule must not be compiled in, got %d rules", len(rs.Routes))
	}
	d, _ := rs.Route(EnvForPair("a@x.com", "b@y.com"), StageData)
	if g, _ := d.Relay(); g != "Outbound" {
		t.Errorf("got %q", g)
	}
}

// --- actions ---------------------------------------------------------------

func TestActions_TagsAndHeadersAccumulateThenTheTerminalWins(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		{Name: "bulk", Priority: 10,
			Match: Pred{Field: "header.x-ngm-campaign", Op: OpExists},
			Then: []Action{
				{Kind: ActTag, Key: "class", Value: "bulk"},
				{Kind: ActAddHeader, Name: "X-NGM-Route", Value: "bulk"},
				{Kind: ActRelay, Relay: "Outbound"},
			}},
	}})

	env := EnvForPair("a@x.com", "b@y.com")
	env.Msg = &MsgEnv{Headers: map[string][]string{"x-ngm-campaign": {"spring"}}}

	d, ok := rs.Route(env, StageData)
	if !ok {
		t.Fatal("must decide")
	}
	if g, _ := d.Relay(); g != "Outbound" {
		t.Errorf("relay: got %q", g)
	}
	if len(d.Headers) != 1 || d.Headers[0].Name != "X-NGM-Route" {
		t.Errorf("headers: got %+v", d.Headers)
	}
	if d.Tags["class"] != "bulk" {
		t.Errorf("tags: got %+v", d.Tags)
	}
}

func TestActions_TagsAreVisibleToLaterRules(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		{Name: "classify", Priority: 10,
			Match: Pred{Field: "mail.from_domain", Op: OpEq, Value: "ngm.dev"},
			Then:  []Action{{Kind: ActTag, Key: "class", Value: "internal"}}},
		relayRule("route internal", 20, Pred{Field: "tag.class", Op: OpEq, Value: "internal"}, "Exchange"),
		relayRule("everything else", 30, Pred{All: []Pred{}}, "Outbound"),
	}})

	d, _ := rs.Route(EnvForPair("me@ngm.dev", "you@elsewhere.com"), StageData)
	if g, _ := d.Relay(); g != "Exchange" {
		t.Errorf("a tag set by an earlier rule must be readable by a later one, got %q", g)
	}

	d2, _ := rs.Route(EnvForPair("me@other.com", "you@elsewhere.com"), StageData)
	if g, _ := d2.Relay(); g != "Outbound" {
		t.Errorf("without the tag the fallback must win, got %q", g)
	}
}

func TestActions_PolicyFiresAtItsOwnStage(t *testing.T) {
	rs := compileFile(t, &File{Policy: []Rule{
		{Name: "no loops", Match: Pred{Field: "msg.received_count", Op: OpGt, Value: 100},
			Then: []Action{{Kind: ActReject, Code: 554, Enhanced: "5.4.6", Message: "Too many hops"}}},
		{Name: "block a sender", Match: Pred{Field: "mail.from", Op: OpEq, Value: "spam@bad.com"},
			Then: []Action{{Kind: ActReject, Code: 550, Message: "Go away"}}},
	}})

	env := EnvForPair("spam@bad.com", "b@y.com")

	// The sender rule is a mail-stage rule and must not wait for DATA.
	res := rs.EvalPolicy(env, StageMail)
	if res.Action == nil || res.Action.Code != 550 {
		t.Fatalf("mail-stage policy must fire at MAIL, got %+v", res.Action)
	}

	// The hop-count rule needs the message, so it cannot fire before DATA.
	if got := rs.EvalPolicy(env, StageRcpt); got.Action != nil {
		t.Errorf("a data-stage rule must not fire at RCPT, got %+v", got.Action)
	}
}

func TestActions_RecipientScopedPolicyIsSeparate(t *testing.T) {
	rs := compileFile(t, &File{Policy: []Rule{
		{Name: "no postmaster", Match: Pred{Field: "rcpt.local", Op: OpEq, Value: "postmaster"},
			Then: []Action{{Kind: ActReject, Code: 550, Message: "No"}}},
	}})

	env := EnvForPair("a@x.com", "postmaster@ngm.dev")
	if got := rs.EvalPolicy(env, StageRcpt); got.Action != nil {
		t.Error("a rcpt-scoped rule must not run in the message-scoped pass")
	}
	got := rs.EvalPolicyRcpt(env, StageRcpt)
	if got.Action == nil || got.Action.Code != 550 {
		t.Fatalf("the recipient pass must fire it, got %+v", got.Action)
	}
}

// --- validation ------------------------------------------------------------

func TestValidation_RejectsBadRules(t *testing.T) {
	cases := []struct {
		name string
		file *File
		want string
	}{
		{"unknown field",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "rcpt.doman", Op: OpEq, Value: "x"}, "Exchange")}},
			"unknown field"},
		{"unknown relay group",
			&File{Routes: []Rule{relayRule("r", 0, Pred{All: []Pred{}}, "Nope")}},
			"not defined in relays.json"},
		{"prototype-chain relay name",
			&File{Routes: []Rule{relayRule("r", 0, Pred{All: []Pred{}}, "toString")}},
			"not defined in relays.json"},
		{"numeric operator on a string field",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "mail.from", Op: OpGt, Value: 5}, "Exchange")}},
			"needs a numeric field"},
		{"string operator on a numeric field",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "msg.size", Op: OpContains, Value: "x"}, "Exchange")}},
			"needs a string field"},
		{"in_cidr on a non-ip field",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "helo.name", Op: OpInCIDR, Values: []any{"10.0.0.0/8"}}, "Exchange")}},
			"needs an ip field"},
		{"bad cidr",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "conn.remote_ip", Op: OpInCIDR, Values: []any{"10.0.0.0/99"}}, "Exchange")}},
			"not a CIDR prefix"},
		{"bad regex",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "mail.from", Op: OpRegex, Value: "a(b"}, "Exchange")}},
			"bad regex"},
		{"unknown operator",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "mail.from", Op: "matches", Value: "x"}, "Exchange")}},
			"unknown operator"},
		{"value where values belongs",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "mail.from", Op: OpIn, Value: "x"}, "Exchange")}},
			"needs a non-empty `values`"},
		{"missing value",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "mail.from", Op: OpEq}, "Exchange")}},
			"needs a `value`"},
		{"exists with a value",
			&File{Routes: []Rule{relayRule("r", 0, Pred{Field: "mail.from", Op: OpExists, Value: "x"}, "Exchange")}},
			"takes no value"},
		{"empty predicate",
			&File{Routes: []Rule{relayRule("r", 0, Pred{}, "Exchange")}},
			"empty predicate"},
		{"no actions",
			&File{Routes: []Rule{{Name: "r", Match: Pred{All: []Pred{}}}}},
			"no actions"},
		{"unknown action",
			&File{Routes: []Rule{{Name: "r", Match: Pred{All: []Pred{}}, Then: []Action{{Kind: "explode"}}}}},
			"unknown action"},
		{"relay in policy",
			&File{Policy: []Rule{{Name: "r", Match: Pred{All: []Pred{}}, Then: []Action{{Kind: ActRelay, Relay: "Exchange"}}}}},
			"belongs in `routes`"},
		{"accept in routes",
			&File{Routes: []Rule{{Name: "r", Match: Pred{All: []Pred{}}, Then: []Action{{Kind: ActAccept}}}}},
			"belongs in `policy`"},
		{"unreachable action after a terminal one",
			&File{Routes: []Rule{{Name: "r", Match: Pred{All: []Pred{}}, Then: []Action{
				{Kind: ActRelay, Relay: "Exchange"}, {Kind: ActTag, Key: "k", Value: "v"}}}}},
			"unreachable"},
		{"duplicate rule names",
			&File{Routes: []Rule{
				relayRule("same", 0, Pred{All: []Pred{}}, "Exchange"),
				relayRule("same", 1, Pred{All: []Pred{}}, "Outbound"),
			}},
			"duplicate rule name"},
		{"reject with a 4xx code",
			&File{Policy: []Rule{{Name: "r", Match: Pred{All: []Pred{}}, Then: []Action{{Kind: ActReject, Code: 451}}}}},
			"needs a 5xx code"},
		{"tempfail with a 5xx code",
			&File{Policy: []Rule{{Name: "r", Match: Pred{All: []Pred{}}, Then: []Action{{Kind: ActTempfail, Code: 550}}}}},
			"needs a 4xx code"},
		{"discard before a message exists",
			&File{Policy: []Rule{{Name: "r", Match: Pred{Field: "conn.remote_ip", Op: OpExists},
				Then: []Action{{Kind: ActDiscard}}}}},
			"needs a message"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustNotCompile(t, c.file, c.want) })
	}
}

// A header value carrying CRLF would let a rule author — or anything feeding
// one — inject headers into every routed message.
func TestValidation_RejectsHeaderInjection(t *testing.T) {
	for _, a := range []Action{
		{Kind: ActAddHeader, Name: "X-Bad\r\nInjected", Value: "v"},
		{Kind: ActAddHeader, Name: "X-Bad", Value: "v\r\nInjected: yes"},
		{Kind: ActAddHeader, Name: "X-Bad: already-has-colon", Value: "v"},
		{Kind: ActReject, Code: 550, Message: "no\r\n250 OK"},
	} {
		f := &File{Routes: []Rule{{Name: "r", Match: Pred{All: []Pred{}}, Then: []Action{
			a, {Kind: ActRelay, Relay: "Exchange"},
		}}}}
		if _, err := Compile(f, relayTable(t, "Exchange"), DefaultSchema()); err == nil {
			t.Errorf("%+v must be rejected", a)
		}
	}
}

func TestValidation_UnknownFieldSuggestsAlternatives(t *testing.T) {
	f := &File{Routes: []Rule{relayRule("r", 0, Pred{Field: "rcpt.domainn", Op: OpEq, Value: "x"}, "Exchange")}}
	_, err := Compile(f, relayTable(t, "Exchange"), DefaultSchema())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "rcpt.domain") {
		t.Errorf("the error should suggest the real field name: %v", err)
	}
}

// --- explain ---------------------------------------------------------------

func TestExplain_ReportsTheWinningRuleAndWhatWasSkipped(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("ngm.dev", 10, Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}, "Exchange"),
		relayRule("catch all", 20, Pred{All: []Pred{}}, "Outbound"),
	}})

	ex := rs.Explain(EnvForPair("a@x.com", "b@ngm.dev"), StageData)
	if !ex.Decided || ex.Decision.Rule != "ngm.dev" {
		t.Fatalf("expected the ngm.dev rule to win, got %+v", ex.Decision)
	}

	byName := map[string]string{}
	for _, tr := range ex.Traces {
		byName[tr.Name] = tr.Status
	}
	if byName["ngm.dev"] != StatusMatched {
		t.Errorf("winning rule status: %q", byName["ngm.dev"])
	}
	if byName["catch all"] != StatusSkipped {
		t.Errorf("a rule after the decision must read as skipped, got %q", byName["catch all"])
	}

	var sb strings.Builder
	ex.Print(&sb)
	if !strings.Contains(sb.String(), "relay Exchange") {
		t.Errorf("printed explanation should name the destination:\n%s", sb.String())
	}
}

func TestExplain_ReportsDeferral(t *testing.T) {
	rs := compileFile(t, &File{Routes: []Rule{
		relayRule("needs the body", 10, Pred{Field: "msg.size", Op: OpGt, Value: 10}, "Outbound"),
		relayRule("by domain", 20, Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}, "Exchange"),
	}})

	ex := rs.Explain(EnvForPair("a@x.com", "b@ngm.dev"), StageRcpt)
	if ex.Decided {
		t.Error("must not decide while a higher-priority rule is unevaluable")
	}
	if ex.Traces[0].Status != StatusDeferred {
		t.Errorf("first trace status: got %q, want deferred", ex.Traces[0].Status)
	}
}

func TestFieldsUsed(t *testing.T) {
	rs := compileFile(t, &File{
		Policy: []Rule{{Name: "p", Match: Pred{Field: "msg.received_count", Op: OpGt, Value: 100},
			Then: []Action{{Kind: ActReject}}}},
		Routes: []Rule{relayRule("r", 0, Pred{All: []Pred{
			{Field: "attachment.filename", Op: OpGlob, Value: "*.exe"},
			{Not: &Pred{Field: "rcpt.domain", Op: OpEq, Value: "ngm.dev"}},
		}}, "Exchange")},
	})

	want := map[string]bool{"msg.received_count": true, "attachment.filename": true, "rcpt.domain": true}
	got := rs.FieldsUsed()
	if len(got) != len(want) {
		t.Fatalf("FieldsUsed: got %v", got)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("unexpected field %q", f)
		}
	}
}

// --- file loading ----------------------------------------------------------

func TestLoadFile_ParsesYAMLAndRejectsUnknownKeys(t *testing.T) {
	good := writeFile(t, "routing.yaml", `
version: 1
default_action: { action: tempfail, code: 451, message: "No route found" }
routes:
  - name: "partners"
    priority: 30
    match:
      any:
        - { field: rcpt.domain, op: in, values: ["partner-a.com"] }
        - { field: rcpt.domain, op: glob, value: "*.partner-a.com" }
    then:
      - { action: add_header, name: "X-NGM-Route", value: "partner" }
      - { action: relay, relay: Exchange }
`)
	f, err := LoadFile(good)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	rs, err := Compile(f, relayTable(t, "Exchange"), DefaultSchema())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	d, _ := rs.Route(EnvForPair("a@x.com", "b@mx.partner-a.com"), StageData)
	if g, _ := d.Relay(); g != "Exchange" {
		t.Errorf("subdomain glob should route to Exchange, got %q", g)
	}

	// A misspelled key silently becoming a zero value would reorder the table.
	bad := writeFile(t, "bad.yaml", "version: 1\nroutes:\n  - name: r\n    piority: 5\n    match: {always: true}\n    then: [{action: relay, relay: Exchange}]\n")
	if _, err := LoadFile(bad); err == nil {
		t.Error("an unknown key must be an error, not a silent default")
	}
}

func TestLoadFile_RejectsUnsupportedVersion(t *testing.T) {
	p := writeFile(t, "routing.yaml", "version: 2\nroutes: []\n")
	if _, err := LoadFile(p); err == nil {
		t.Error("an unsupported version must be refused")
	}
}
