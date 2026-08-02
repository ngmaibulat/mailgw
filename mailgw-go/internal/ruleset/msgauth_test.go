package ruleset

import "testing"

// TestMsgAuthFields_AreRegisteredAtTheRightStage pins where the message
// authentication facts become available.
//
// SPF is a DNS walk over the sender's domain and the peer's address, so it is
// answerable on the MAIL line — which is what lets a rule refuse a failing
// sender before a megabyte of DATA arrives. DKIM needs the body and DMARC needs
// the From header, so both are DATA facts however early SPF was answered.
func TestMsgAuthFields_AreRegisteredAtTheRightStage(t *testing.T) {
	sc := DefaultSchema()
	want := map[string]Stage{
		"spf.result":   StageMail,
		"spf.domain":   StageMail,
		"dkim.result":  StageData,
		"dkim.domains": StageData,
		"dmarc.result": StageData,
		"dmarc.policy": StageData,
	}
	for name, stage := range want {
		d, ok := sc.Lookup(name)
		if !ok {
			t.Errorf("%s is not in the registry, so a rule using it would be a load-time error", name)
			continue
		}
		if d.Stage != stage {
			t.Errorf("%s is at stage %s, want %s", name, d.Stage, stage)
		}
		if d.Kind != KindString {
			t.Errorf("%s has kind %s, want string", name, d.Kind)
		}
	}
	if d, _ := sc.Lookup("dkim.domains"); !d.List {
		t.Error("dkim.domains must be a list: a message can carry several passing signatures")
	}
}

// TestMsgAuthFields_StageInference is the reason the stages above matter: a rule
// reading only spf.* must be inferred to MAIL, so its rejection reaches the
// client on its own MAIL line rather than at the end of DATA.
func TestMsgAuthFields_StageInference(t *testing.T) {
	cases := map[string]struct {
		pred Pred
		want Stage
	}{
		"spf alone is a mail rule": {
			pred: Pred{Field: "spf.result", Op: "eq", Value: "fail"},
			want: StageMail,
		},
		"dkim alone is a data rule": {
			pred: Pred{Field: "dkim.result", Op: "eq", Value: "pass"},
			want: StageData,
		},
		"dmarc alone is a data rule": {
			pred: Pred{Field: "dmarc.result", Op: "eq", Value: "fail"},
			want: StageData,
		},
		"spf with dmarc defers to data": {
			pred: Pred{All: []Pred{
				{Field: "spf.result", Op: "eq", Value: "fail"},
				{Field: "dmarc.policy", Op: "eq", Value: "reject"},
			}},
			want: StageData,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rs := compileFile(t, &File{Policy: []Rule{{
				Name: "r", Match: c.pred,
				Then: []Action{{Kind: ActReject, Code: 550, Enhanced: "5.7.1", Message: "no"}},
			}}})
			if got := rs.Policy[0].Stage; got != c.want {
				t.Errorf("inferred stage %s, want %s", got, c.want)
			}
		})
	}
}

// TestNeedsChecks is the gating that keeps the shipped default free.
//
// A configuration whose rules never ask must not pay for an SPF walk, a DKIM
// verification or a DMARC lookup — the same bargain NeedsMIME strikes for the
// MIME walk, and for the same reason: each of these is DNS and a body re-read
// per message.
func TestNeedsChecks(t *testing.T) {
	none := compileFile(t, &File{Routes: []Rule{
		relayRule("r", 0, Pred{Field: "rcpt.domain", Op: "eq", Value: "ngm.dev"}, "Outbound"),
	}})
	if none.NeedsSPF() || none.NeedsDKIM() || none.NeedsDMARC() || none.NeedsMIME() {
		t.Error("a configuration with no msgauth rules should need no msgauth check")
	}

	spfOnly := compileFile(t, &File{Routes: []Rule{
		relayRule("r", 0, Pred{Field: "spf.result", Op: "eq", Value: "pass"}, "Outbound"),
	}})
	if !spfOnly.NeedsSPF() {
		t.Error("a rule reading spf.* must turn the SPF check on")
	}
	if spfOnly.NeedsDKIM() || spfOnly.NeedsDMARC() {
		t.Error("a rule reading spf.* must NOT turn on the checks it did not ask for")
	}

	dkimOnly := compileFile(t, &File{Routes: []Rule{
		relayRule("r", 0, Pred{Field: "dkim.domains", Op: "contains", Value: "ngm.dev"}, "Outbound"),
	}})
	if !dkimOnly.NeedsDKIM() || dkimOnly.NeedsSPF() || dkimOnly.NeedsDMARC() {
		t.Errorf("dkim rule: spf=%v dkim=%v dmarc=%v",
			dkimOnly.NeedsSPF(), dkimOnly.NeedsDKIM(), dkimOnly.NeedsDMARC())
	}

	// DMARC is alignment over an SPF result and a DKIM result, so asking for it
	// without them would answer "fail" for every message.
	dmarc := compileFile(t, &File{Routes: []Rule{
		relayRule("r", 0, Pred{Field: "dmarc.result", Op: "eq", Value: "pass"}, "Outbound"),
	}})
	if !dmarc.NeedsDMARC() || !dmarc.NeedsSPF() || !dmarc.NeedsDKIM() {
		t.Errorf("a dmarc rule must imply spf and dkim: spf=%v dkim=%v dmarc=%v",
			dmarc.NeedsSPF(), dmarc.NeedsDKIM(), dmarc.NeedsDMARC())
	}

	// The nil receiver answers false rather than panicking, matching NeedsMIME:
	// a managed gateway has no compiled ruleset before its first apply.
	var nilRS *Ruleset
	if nilRS.NeedsSPF() || nilRS.NeedsDKIM() || nilRS.NeedsDMARC() {
		t.Error("a nil ruleset must need nothing")
	}
}

// TestMsgAuthFields_UncheckedReadsAsMissing is the distinction the whole design
// rests on: a check that did not run is ABSENT, not "none". Reading it as "none"
// would make `spf.result eq "none"` match on a gateway that never looked, which
// is a rule firing on a fact nobody established.
func TestMsgAuthFields_UncheckedReadsAsMissing(t *testing.T) {
	env := &Env{
		Stage: StageData,
		Mail:  &MailEnv{From: "a@ngm.dev"},
		Msg:   &MsgEnv{Size: 100},
	}

	for _, f := range []string{"spf.result", "spf.domain", "dkim.result", "dkim.domains", "dmarc.result", "dmarc.policy"} {
		if v := env.Lookup(f); v.OK {
			t.Errorf("%s is present (%+v) when no check ran", f, v)
		}
		if matchOne(t, Pred{Field: f, Op: "eq", Value: "none"}, env) {
			t.Errorf(`%s eq "none" matched when no check ran`, f)
		}
		if matchOne(t, Pred{Field: f, Op: "exists"}, env) {
			t.Errorf("%s exists when no check ran", f)
		}
	}

	// Once a check has run, its result is readable — including "none", which is
	// a real answer meaning "the domain published no policy".
	env.Mail.SPFResult, env.Mail.SPFDomain = "none", "ngm.dev"
	env.Msg.DKIMResult = "pass"
	env.Msg.DKIMDomains = []string{"ngm.dev"}
	env.Msg.DMARCResult, env.Msg.DMARCPolicy = "fail", "reject"

	if !matchOne(t, Pred{Field: "spf.result", Op: "eq", Value: "none"}, env) {
		t.Error(`spf.result eq "none" did not match a check that ran and found no policy`)
	}
	if !matchOne(t, Pred{Field: "spf.domain", Op: "eq", Value: "NGM.dev"}, env) {
		t.Error("spf.domain should compare case-insensitively")
	}
	if !matchOne(t, Pred{Field: "dkim.domains", Op: "eq", Value: "ngm.dev"}, env) {
		t.Error("dkim.domains is a list, so a leaf predicate over it is existential")
	}
	if !matchOne(t, Pred{Field: "dmarc.policy", Op: "eq", Value: "reject"}, env) {
		t.Error("dmarc.policy did not match")
	}
}

// TestMsgAuthFields_DomainGlobStopsAtADot: spf.domain and dkim.domains are
// domain-shaped, so `*.partner.com` must not match `partner.com` — the same
// dialect mail.from_domain and rcpt.domain use.
func TestMsgAuthFields_DomainGlobStopsAtADot(t *testing.T) {
	env := &Env{
		Stage: StageData,
		Mail:  &MailEnv{From: "a@ngm.dev", SPFResult: "pass", SPFDomain: "partner.com"},
		Msg:   &MsgEnv{DKIMResult: "pass", DKIMDomains: []string{"mx.partner.com"}},
	}
	if matchOne(t, Pred{Field: "spf.domain", Op: "glob", Value: "*.partner.com"}, env) {
		t.Error("*.partner.com should not match partner.com")
	}
	if !matchOne(t, Pred{Field: "dkim.domains", Op: "glob", Value: "*.partner.com"}, env) {
		t.Error("*.partner.com should match mx.partner.com")
	}
}
