package dsn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedTime keeps Build deterministic so the output can be pinned.
var (
	arrival = time.Date(2026, 7, 30, 9, 15, 0, 0, time.UTC)
	sent    = time.Date(2026, 7, 30, 9, 20, 30, 0, time.UTC)
)

func sampleFailure() Report {
	return Report{
		ReportingMTA: "gw1.ngm.dev",
		From:         "postmaster@gw1.ngm.dev",
		To:           "sender@ngm.dev",
		Kind:         KindFailed,
		OriginalID:   "ABCDEF01-2345-6789-ABCD-EF0123456789.1.1",
		// The sender's own identifier, which is the only thing
		// Original-Envelope-Id may carry. Chosen to need xtext escaping.
		EnvelopeID:  "batch=2026Q3+run1",
		MessageID:   "dsn-0001@gw1.ngm.dev",
		ArrivalDate: arrival,
		Now:         sent,
		Rcpts: []Rcpt{
			{
				Addr: "nobody@partner.com",
				// A plus-addressed original recipient, so the golden pins the
				// xtext encoding of the one character that shows up in ordinary
				// mail rather than only in a contrived test.
				OrigAddr:    "sales+q3@partner.com",
				OrigType:    "rfc822",
				Status:      "5.1.1",
				Diagnostic:  "550 5.1.1 <nobody@partner.com>: Recipient address rejected",
				RemoteMTA:   "mx1.partner.com",
				LastAttempt: sent,
			},
		},
		Original: strings.NewReader("Subject: quarterly report\r\nFrom: sender@ngm.dev\r\n"),
	}
}

// golden pins the whole rendered message. It is the point of this package: the
// format is a contract with every receiving mail system, and a change to it
// should be a deliberate edit to a file, not a surprise.
func TestBuild_Golden(t *testing.T) {
	got, err := Build(sampleFailure())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	path := filepath.Join("testdata", "failure.eml")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Build output differs from testdata/failure.eml\n--- got ---\n%s", got)
	}
}

// Every line must be CRLF-terminated: a bare LF in a message handed to a relay
// is a protocol violation and breaks the dot-stuffing downstream.
func TestBuild_UsesCRLFThroughout(t *testing.T) {
	got, err := Build(sampleFailure())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for i, c := range got {
		if c == '\n' && (i == 0 || got[i-1] != '\r') {
			t.Fatalf("bare LF at byte %d", i)
		}
	}
}

// A diagnostic comes from a remote server and a recipient address from the
// envelope. Neither may end a header line early: this gateway signs the
// resulting message with its own name.
func TestBuild_StripsHeaderInjection(t *testing.T) {
	r := sampleFailure()
	r.Rcpts = []Rcpt{{
		Addr:       "victim@ngm.dev\r\nBcc: attacker@evil.example",
		Status:     "5.1.1\r\nX-Injected: yes",
		Diagnostic: "550 no\r\nX-Also-Injected: yes",
	}}

	got, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The property that matters is that no injected text became a header of its
	// own. Appearing inline within a legitimate header's value is harmless — it
	// is a value, not a field — so this checks line starts, not substrings.
	for _, line := range strings.Split(string(got), "\r\n") {
		for _, bad := range []string{"Bcc:", "X-Injected:", "X-Also-Injected:"} {
			if strings.HasPrefix(line, bad) {
				t.Errorf("injected header %q became a header line:\n%s", bad, got)
			}
		}
	}
}

func TestBuild_DelayReportIsNotAFailure(t *testing.T) {
	r := sampleFailure()
	r.Kind = KindDelayed
	r.RetryUntil = arrival.Add(96 * time.Hour)
	r.Rcpts = []Rcpt{{Addr: "slow@partner.com", Diagnostic: "451 4.4.1 connection timed out"}}

	got, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := string(got)

	if !strings.Contains(s, "Action: delayed") {
		t.Error("delay report does not carry Action: delayed")
	}
	if strings.Contains(s, "Action: failed") {
		t.Error("delay report carries Action: failed")
	}
	// A missing status must default to a 4.x on a delay, or a sender's software
	// reads a still-queued message as permanently dead.
	if !strings.Contains(s, "Status: 4.0.0") {
		t.Errorf("delay report did not default to a temporary status:\n%s", s)
	}
	if !strings.Contains(s, "keep\r\ntrying") {
		t.Error("delay report does not say it will keep trying")
	}
}

func TestBuild_DefaultStatusIsPermanentOnFailure(t *testing.T) {
	r := sampleFailure()
	r.Rcpts = []Rcpt{{Addr: "nobody@partner.com"}}

	got, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(string(got), "Status: 5.0.0") {
		t.Errorf("failure report did not default to a permanent status:\n%s", got)
	}
}

// A relay's reply is an SMTP diagnostic; this gateway's own words are not, and
// mislabelling them makes a parser read "unknown relay group" as a reply code.
func TestBuild_DiagnosticTypeMatchesItsSource(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"550 5.1.1 no such user", "Diagnostic-Code: smtp; 550 5.1.1 no such user"},
		{"unknown relay group", "Diagnostic-Code: X-Local; unknown relay group"},
	} {
		r := sampleFailure()
		r.Rcpts = []Rcpt{{Addr: "a@b.com", Status: "5.1.1", Diagnostic: tc.in}}
		got, err := Build(r)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if !strings.Contains(string(got), tc.want) {
			t.Errorf("diagnostic %q did not render as %q", tc.in, tc.want)
		}
	}
}

func TestBuild_MultipleRecipientsGetOneBlockEach(t *testing.T) {
	r := sampleFailure()
	r.Rcpts = []Rcpt{
		{Addr: "one@partner.com", Status: "5.1.1", Diagnostic: "550 no"},
		{Addr: "two@partner.com", Status: "5.2.2", Diagnostic: "552 full"},
	}

	got, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := string(got)
	if n := strings.Count(s, "Final-Recipient:"); n != 2 {
		t.Errorf("got %d Final-Recipient blocks, want 2", n)
	}
	// One report, not two — the format exists to carry several recipients.
	if n := strings.Count(s, "Reporting-MTA:"); n != 1 {
		t.Errorf("got %d Reporting-MTA lines, want 1", n)
	}
}

func TestBuild_FullReturnQuotesTheWholeMessage(t *testing.T) {
	r := sampleFailure()
	r.Full = true
	r.Original = strings.NewReader("Subject: hi\r\n\r\nthe body\r\n")

	got, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "Content-Type: message/rfc822") {
		t.Error("full return did not use message/rfc822")
	}
	if !strings.Contains(s, "the body") {
		t.Error("full return did not include the body")
	}
}

func TestBuild_HeadersOnlyIsTheDefault(t *testing.T) {
	got, err := Build(sampleFailure())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(string(got), "Content-Type: text/rfc822-headers") {
		t.Error("default return did not use text/rfc822-headers")
	}
}

// Build must refuse rather than emit an unsendable or unparseable message.
func TestBuild_RejectsIncompleteReports(t *testing.T) {
	for name, mangle := range map[string]func(*Report){
		"no reporting MTA": func(r *Report) { r.ReportingMTA = "" },
		"no postmaster":    func(r *Report) { r.From = "" },
		// A null sender has nowhere to send a bounce. Reaching here means a
		// caller skipped the never-bounce-a-bounce check.
		"null sender":   func(r *Report) { r.To = "" },
		"no recipients": func(r *Report) { r.Rcpts = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := sampleFailure()
			mangle(&r)
			if _, err := Build(r); err == nil {
				t.Errorf("Build accepted a report with %s", name)
			}
		})
	}
}

// The closing boundary has to begin a line, whatever the quoted message ended
// with — otherwise the last part never closes and the report is unparseable.
func TestBuild_ClosesTheBoundaryAfterAnUnterminatedOriginal(t *testing.T) {
	r := sampleFailure()
	r.Original = strings.NewReader("Subject: no trailing newline")

	got, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.HasSuffix(string(got), "\r\n--"+boundary+"--\r\n") {
		t.Errorf("report does not end with a closing boundary on its own line:\n%q",
			string(got)[max(0, len(got)-120):])
	}
}

// The status class and the Action must agree. RFC 3463 defines most conditions
// in both classes, so it is easy to hand a 4.x to a failure report — which tells
// the sender's software the condition is transient one line before telling it no
// further attempt will be made. Receiving systems act on the status.
func TestBuild_StatusClassAgreesWithTheAction(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind Kind
		in   string
		want string
	}{
		{"temporary status on a failure is corrected", KindFailed, "4.4.7", "Status: 5.4.7"},
		{"permanent status on a delay is corrected", KindDelayed, "5.4.7", "Status: 4.4.7"},
		{"a matching class is left alone", KindFailed, "5.1.1", "Status: 5.1.1"},
		{"a delay's own class is left alone", KindDelayed, "4.4.1", "Status: 4.4.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleFailure()
			r.Kind = tc.kind
			r.Rcpts = []Rcpt{{Addr: "a@b.com", Status: tc.in}}

			got, err := Build(r)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !strings.Contains(string(got), tc.want) {
				t.Errorf("status %q on a %s report did not render as %q",
					tc.in, tc.kind.Action(), tc.want)
			}
		})
	}
}

// --- RFC 3461 fields (M13) ---

func TestXtext_EncodesSpecialsAndNonPrintables(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain@example.com", "plain@example.com"},
		// The two xtext specials, and the reason this exists: a plus-addressed
		// recipient is an ordinary address, not an edge case.
		{"user+tag@example.com", "user+2Btag@example.com"},
		{"a=b", "a+3Db"},
		{"a b", "a+20b"},
		// Below 0x10, where a formatter without zero-padding emits "+9" and
		// produces something no receiving system can decode.
		{"a\tb", "a+09b"},
		{"\x01", "+01"},
		{"\r\n", "+0D+0A"},
		{"é", "+C3+A9"},
		{"", ""},
	} {
		if got := xtext(tc.in); got != tc.want {
			t.Errorf("xtext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Whatever an attacker puts in an address, the encoded form cannot carry a line
// break — which is what lets writeStatus emit it without clean().
func TestXtext_CannotBreakAHeaderLine(t *testing.T) {
	got := xtext("victim@example.com\r\nBcc: attacker@evil.test")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("xtext(%q) still contains a line break", got)
	}
}

func TestBuild_OmitsOriginalRecipientWhenAbsent(t *testing.T) {
	r := sampleFailure()
	r.Rcpts[0].OrigAddr = ""
	r.Rcpts[0].OrigType = ""

	out := build(t, r)
	if strings.Contains(out, "Original-Recipient") {
		t.Errorf("Original-Recipient emitted with no ORCPT:\n%s", out)
	}
	if !strings.Contains(out, "Final-Recipient: rfc822; nobody@partner.com") {
		t.Error("Final-Recipient is missing")
	}
}

func TestBuild_DefaultsAndLowerCasesTheOriginalRecipientType(t *testing.T) {
	for _, tc := range []struct{ name, typ, want string }{
		// go-smtp upper-cases the address type it parsed, and every MTA in the
		// world writes it lower-case.
		{"upper-cased by go-smtp", "RFC822", "Original-Recipient: rfc822; orig@partner.com"},
		{"absent", "", "Original-Recipient: rfc822; orig@partner.com"},
		{"something else", "x-unknown", "Original-Recipient: x-unknown; orig@partner.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleFailure()
			r.Rcpts[0].OrigAddr = "orig@partner.com"
			r.Rcpts[0].OrigType = tc.typ

			if out := build(t, r); !strings.Contains(out, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, out)
			}
		})
	}
}

// Original-Envelope-Id is the sender's ENVID and nothing else. Answering it with
// an identifier this gateway invented gives a sender that does use ENVID a
// confident non-match against something that looks like one.
func TestBuild_OriginalEnvelopeIDIsTheSendersENVID(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		r := sampleFailure()
		r.EnvelopeID = "sender-envid-1"

		out := build(t, r)
		if !strings.Contains(out, "Original-Envelope-Id: sender-envid-1") {
			t.Errorf("the sender's ENVID is missing:\n%s", out)
		}
	})

	t.Run("absent", func(t *testing.T) {
		r := sampleFailure()
		r.EnvelopeID = ""

		out := build(t, r)
		if strings.Contains(out, "Original-Envelope-Id") {
			t.Errorf("Original-Envelope-Id emitted with no ENVID:\n%s", out)
		}
		// The gateway's own identifier is still reachable, just not under a
		// field RFC 3464 gives a different meaning.
		if !strings.Contains(out, "X-NGM-Original-Envelope: "+r.OriginalID) {
			t.Errorf("the envelope uuid is not in the machine-readable part:\n%s", out)
		}
	})
}

// A relayed report is a success, so its Action, Subject and status class must
// all say so — a 5.x status under "Action: relayed" would tell a sender's
// software two contradictory things in consecutive lines.
func TestBuild_RelayedReportIsASuccess(t *testing.T) {
	r := sampleFailure()
	r.Kind = KindRelayed
	r.Rcpts[0].Diagnostic = ""
	r.Rcpts[0].RemoteMTA = "mx1.partner.com"

	t.Run("default status", func(t *testing.T) {
		r := r
		r.Rcpts = []Rcpt{{Addr: "someone@partner.com"}}

		out := build(t, r)
		for _, want := range []string{"Action: relayed", "Status: 2.0.0", "(Relayed)"} {
			if !strings.Contains(out, want) {
				t.Errorf("want %q in:\n%s", want, out)
			}
		}
	})

	t.Run("a contradictory class is corrected", func(t *testing.T) {
		r := r
		r.Rcpts = []Rcpt{{Addr: "someone@partner.com", Status: "5.1.1"}}

		if out := build(t, r); !strings.Contains(out, "Status: 2.1.1") {
			t.Errorf("a 5.x status survived a relayed report:\n%s", out)
		}
	})

	// It must not claim delivery: this gateway knows a relay accepted the
	// recipient and nothing about what happened next.
	t.Run("says relayed, not delivered", func(t *testing.T) {
		r := r
		r.Rcpts = []Rcpt{{Addr: "someone@partner.com"}}

		if out := build(t, r); strings.Contains(out, "Action: delivered") {
			t.Error("a relayed report claims final delivery")
		}
	})
}

func build(t *testing.T, r Report) string {
	t.Helper()
	out, err := Build(r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return string(out)
}
