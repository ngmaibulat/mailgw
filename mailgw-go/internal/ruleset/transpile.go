package ruleset

import "fmt"

// Transpile converts a Haraka routing.json into the DSL.
//
// The result is exactly equivalent, rule for rule: each non-empty field of a
// legacy rule becomes one `eq` predicate, the four are ANDed (Route.js:24-28),
// rules keep their file order via ascending priorities, and the first match
// wins. A legacy rule with every field empty is the catch-all.
//
// Equivalence is not asserted by inspection — legacy_test.go runs both matchers
// over the same envelope matrix and requires identical answers.
func (t *LegacyTable) Transpile() *File {
	f := &File{Version: 1}
	always := true

	for i, r := range t.Rules {
		preds := []Pred{}
		for _, m := range []struct{ field, want string }{
			{"mail.from", r.Sender},
			{"mail.from_domain", r.SenderDomain},
			{"rcpt.to", r.Rcpt},
			{"rcpt.domain", r.RcptDomain},
		} {
			// An empty expectation is a wildcard
			// (Route.js:getCheckerFunction), so it contributes no predicate.
			if m.want == "" {
				continue
			}
			preds = append(preds, Pred{Field: m.field, Op: OpEq, Value: m.want})
		}

		match := Pred{All: preds}
		if len(preds) == 0 {
			match = Pred{Always: &always}
		}

		name := r.RouteName
		if name == "" {
			name = fmt.Sprintf("route[%d]", i)
		}

		f.Routes = append(f.Routes, Rule{
			Name: name,
			// Spaced out so an operator can insert a rule between two
			// converted ones without renumbering the file.
			Priority: (i + 1) * 10,
			Match:    match,
			Then:     []Action{{Kind: ActRelay, Relay: r.Relay}},
		})
	}

	def := DefaultAction()
	f.DefaultAction = &def
	return f
}

// EnvForPair builds the minimal Env that the legacy four-field model needs: a
// sender and one recipient, evaluated at RCPT. Used by the equivalence tests
// and by `explain` when no message file is supplied.
func EnvForPair(sender, rcpt string) *Env {
	return &Env{
		Stage: StageRcpt,
		Conn:  &ConnEnv{},
		Helo:  &HeloEnv{},
		Mail:  &MailEnv{From: sender},
		Rcpt:  &RcptEnv{To: rcpt, Index: 1, CountSoFar: 1},
	}
}
