package msgauth

import (
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleResults is the fixture every rendering test mutates a copy of. Its
// values are chosen so the golden pins one of each: a pass with parameters, two
// DKIM domains, and a DMARC failure carrying a reason.
func sampleResults() (SPFResult, DKIMResult, DMARCResult) {
	spf := SPFResult{
		Value: ResultPass, Domain: "ngm.dev",
		MailFrom: "alice@ngm.dev", Helo: "mx1.ngm.dev",
	}
	dk := DKIMResult{Value: ResultPass, Domains: []string{"ngm.dev", "partner.example"}}
	dm := DMARCResult{
		Value: ResultFail, Policy: "quarantine", FromDomain: "ngm.dev",
		Reason: "SPF passed but is not aligned with the From domain",
	}
	return spf, dk, dm
}

// TestFormatAuthResults_Golden pins the rendered headers.
//
// It is the point of this file: these two strings are a contract with every
// system downstream, and a change to them should be a deliberate edit to a file
// rather than a surprise. Regenerate with UPDATE_GOLDEN=1.
func TestFormatAuthResults_Golden(t *testing.T) {
	spf, dk, dm := sampleResults()
	ip := netip.MustParseAddr("203.0.113.7")

	got := "Authentication-Results: " + FormatAuthResults("gw.ngm.dev", spf, dk, dm) + "\n" +
		"Received-SPF: " + FormatReceivedSPF(spf, ip, "gw.ngm.dev") + "\n"

	path := filepath.Join("testdata", "headers.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if got != string(want) {
		t.Errorf("rendered headers differ from testdata/headers.golden\n--- got ---\n%s", got)
	}
}

// TestFormatAuthResults_OmitsChecksThatDidNotRun is the distinction the whole
// header depends on. "none" asserts that a check RAN and found no policy;
// claiming it for a check that never happened would put a false statement in the
// one header whose purpose is to be believed downstream.
func TestFormatAuthResults_OmitsChecksThatDidNotRun(t *testing.T) {
	spf, dk, _ := sampleResults()

	if got := FormatAuthResults("gw.ngm.dev", SPFResult{}, DKIMResult{}, DMARCResult{}); got != "" {
		t.Errorf("with nothing checked, want an empty value so the caller omits the header; got %q", got)
	}

	got := FormatAuthResults("gw.ngm.dev", spf, DKIMResult{}, DMARCResult{})
	if strings.Contains(got, "dkim=") || strings.Contains(got, "dmarc=") {
		t.Errorf("reported a check that did not run: %q", got)
	}
	if !strings.Contains(got, "spf=pass") {
		t.Errorf("dropped the check that did run: %q", got)
	}

	// A check that ran and found nothing IS reported, as "none".
	got = FormatAuthResults("gw.ngm.dev", spf, dk, DMARCResult{Value: ResultNone, FromDomain: "ngm.dev"})
	if !strings.Contains(got, "dmarc=none") {
		t.Errorf("dropped a none result, which is a real answer: %q", got)
	}
}

// TestFormatAuthResults_NoStrayWhitespace: authres.Format writes the parameters
// after a space whether there are any or not, so a parameterless result leaves
// " ;" behind. Legal, but it makes the header look mangled to the operator
// reading it and would put the library's whitespace habits in our golden file.
func TestFormatAuthResults_NoStrayWhitespace(t *testing.T) {
	got := FormatAuthResults("gw.ngm.dev",
		SPFResult{Value: ResultPass, Domain: "ngm.dev", MailFrom: "a@ngm.dev"},
		DKIMResult{Value: ResultNone},
		DMARCResult{Value: ResultNone, FromDomain: "ngm.dev"})

	if strings.Contains(got, " ;") {
		t.Errorf("a space precedes a semicolon: %q", got)
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("leading or trailing whitespace: %q", got)
	}
	if !strings.Contains(got, "dkim=none;") {
		t.Errorf("a parameterless result did not survive the cleanup: %q", got)
	}
}

// TestFormatAuthResults_OneLinePerPassingSignature: RFC 7601 has no way to say
// "pass, with these three domains" on one method result, so each passing
// signature gets its own — otherwise header.d would be ambiguous.
func TestFormatAuthResults_OneLinePerPassingSignature(t *testing.T) {
	_, dk, _ := sampleResults()
	got := FormatAuthResults("gw.ngm.dev", SPFResult{}, dk, DMARCResult{})
	if n := strings.Count(got, "dkim=pass"); n != 2 {
		t.Errorf("want one dkim=pass per passing domain (2), got %d: %q", n, got)
	}
	for _, d := range dk.Domains {
		if !strings.Contains(got, "header.d="+d) {
			t.Errorf("missing header.d=%s in %q", d, got)
		}
	}
}

// TestFormatReceivedSPF_CommentCannotBeClosedEarly: a Received-SPF comment ends
// at the first unbalanced ")", so a domain or an SPF exp= explanation containing
// one would let a remote party write structured content into a header this
// gateway signs its name to.
func TestFormatReceivedSPF_CommentCannotBeClosedEarly(t *testing.T) {
	res := SPFResult{
		Value:    ResultFail,
		Domain:   "evil.example) client-ip=127.0.0.1; (",
		MailFrom: "a@evil.example",
	}
	got := FormatReceivedSPF(res, netip.MustParseAddr("203.0.113.7"), "gw.ngm.dev")

	open := strings.Count(got, "(")
	closed := strings.Count(got, ")")
	if open != 1 || closed != 1 {
		t.Fatalf("comment parentheses are not balanced exactly once: %q", got)
	}
	if strings.Index(got, "(") > strings.Index(got, ")") {
		t.Fatalf("comment closes before it opens: %q", got)
	}
}

func TestFormatReceivedSPF_NullSender(t *testing.T) {
	res := SPFResult{Value: ResultNeutral, Domain: "mx.partner.example", Helo: "mx.partner.example"}
	got := FormatReceivedSPF(res, netip.MustParseAddr("203.0.113.7"), "gw.ngm.dev")
	if !strings.Contains(got, "envelope-from=<>;") {
		t.Errorf("a null sender must render as <>: %q", got)
	}
}

// TestFormatReceivedSPF_UnmapsIPv4 keeps the header readable: an IPv4 client on
// a dual-stack listener would otherwise be recorded as ::ffff:10.20.0.5, which
// no operator greps for.
func TestFormatReceivedSPF_UnmapsIPv4(t *testing.T) {
	res := SPFResult{Value: ResultPass, Domain: "ngm.dev", MailFrom: "a@ngm.dev"}
	got := FormatReceivedSPF(res, netip.MustParseAddr("::ffff:10.20.0.5"), "gw.ngm.dev")
	if strings.Contains(got, "::ffff:") {
		t.Errorf("IPv4-mapped address not unmapped: %q", got)
	}
	if !strings.Contains(got, "client-ip=10.20.0.5;") {
		t.Errorf("missing client-ip: %q", got)
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("x", maxReason+50)
	if got := clip(long); len([]rune(got)) != maxReason+1 {
		t.Errorf("clip produced %d runes, want %d plus an ellipsis", len([]rune(got)), maxReason)
	}
	if got := clip("  short  "); got != "short" {
		t.Errorf("clip(%q) = %q", "  short  ", got)
	}
}

// ── StripAuthResults ─────────────────────────────────────────────────────────

func strip(t *testing.T, msg, id string) string {
	t.Helper()
	out, err := io.ReadAll(StripAuthResults(strings.NewReader(msg), id))
	if err != nil {
		t.Fatalf("StripAuthResults: %v", err)
	}
	return string(out)
}

// TestStripAuthResults is RFC 7601 §5: a field claiming OUR authserv-id is
// removed, because without that a sender can simply assert "dkim=pass" under our
// name and nothing downstream can tell the forgery from the real thing.
//
// It is deliberately narrow. A third party's field survives, so a gateway behind
// an upstream that legitimately verifies mail does not destroy its results.
func TestStripAuthResults(t *testing.T) {
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			msg := load(t, "forged_ar.eml")
			if ending == "CRLF" {
				msg = crlf(msg)
			}
			got := strip(t, msg, "gw.ngm.dev")

			if strings.Contains(got, "spf=pass smtp.mailfrom=evil.example") {
				t.Error("a forged field bearing our authserv-id survived")
			}
			// Its folded continuation must go with it, or the remains parse as
			// a header field of their own.
			if strings.Contains(got, "dmarc=pass header.from=ngm.dev") {
				t.Error("the continuation of a stripped field survived")
			}
			// Case-insensitive: an authserv-id is a domain.
			if strings.Contains(got, "header.d=bank.example") {
				t.Error("GW.NGM.DEV was not matched against gw.ngm.dev")
			}
			// A third party's results are somebody else's to make.
			if !strings.Contains(got, "upstream.partner.com") {
				t.Error("a third party's Authentication-Results was removed")
			}
			// Everything else is untouched, including a line in the BODY that
			// happens to look like the header.
			for _, want := range []string{
				"X-Keep-Me: yes",
				"From: Mallory <mallory@evil.example>",
				"Authentication-Results: gw.ngm.dev; this line is in the body and stays",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q from the filtered message", want)
				}
			}
		})
	}
}

// TestStripAuthResults_IsIdentityWhenNothingMatches is what makes the filter
// safe to install unconditionally on the DATA path: a message that carries
// nothing of ours comes out byte-identical, so installing it cannot change what
// gets spooled.
func TestStripAuthResults_IsIdentityWhenNothingMatches(t *testing.T) {
	cases := map[string]string{
		"ordinary message":     crlf(load(t, "plain.eml")),
		"third-party AR only":  "Authentication-Results: other.example; spf=pass\r\nFrom: a@b.example\r\n\r\nbody\r\n",
		"headers with no body": "From: a@b.example\r\nSubject: x\r\n",
		"no header/body break": "From: a@b.example\r\nSubject: x\r\nBody-ish line\r\n",
		"empty message":        "",
		"body only":            "\r\njust a body\r\n",
		"lone LF endings":      load(t, "plain.eml"),
		"binary-ish body":      "From: a@b.example\r\n\r\n\x00\x01\x02\r\n.\r\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := strip(t, in, "gw.ngm.dev"); got != in {
				t.Errorf("filter altered a message it should have passed through\n got: %q\nwant: %q", got, in)
			}
		})
	}
}

// TestStripAuthResults_EmptyIDIsAPassThrough: with no authserv-id there is
// nothing to forge, and the caller gets its own reader back rather than a
// wrapper that copies every byte for nothing.
func TestStripAuthResults_EmptyIDIsAPassThrough(t *testing.T) {
	r := strings.NewReader("From: a@b.example\r\n\r\nbody\r\n")
	if StripAuthResults(r, "  ") != io.Reader(r) {
		t.Error("an empty authserv-id should return the original reader unwrapped")
	}
}

func TestOurs(t *testing.T) {
	cases := []struct {
		field string
		want  bool
	}{
		{"Authentication-Results: gw.ngm.dev; spf=pass\r\n", true},
		{"authentication-results: gw.ngm.dev; spf=pass\r\n", true},
		{"Authentication-Results: GW.NGM.DEV; spf=pass\r\n", true},
		{"Authentication-Results: gw.ngm.dev.; spf=pass\r\n", true},
		{"Authentication-Results:  gw.ngm.dev 1; spf=pass\r\n", true}, // RFC 7601 version token
		{"Authentication-Results: gw.ngm.dev; none\r\n", true},
		{"Authentication-Results: gw.ngm.dev\r\n", true}, // no semicolon at all
		{"Authentication-Results:\r\n\tgw.ngm.dev; spf=pass\r\n", true},
		{"Authentication-Results: other.example; spf=pass\r\n", false},
		// A prefix is not a match: notgw.ngm.dev is a different party.
		{"Authentication-Results: notgw.ngm.dev; spf=pass\r\n", false},
		{"X-Authentication-Results: gw.ngm.dev; spf=pass\r\n", false},
		{"Received: from gw.ngm.dev\r\n", false},
		{"no colon at all\r\n", false},
	}
	for _, c := range cases {
		if got := ours([]byte(c.field), "gw.ngm.dev"); got != c.want {
			t.Errorf("ours(%q) = %v, want %v", c.field, got, c.want)
		}
	}
}
