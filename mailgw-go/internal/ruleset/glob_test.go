package ruleset

import "testing"

func TestGlob_DotSeparatedNames(t *testing.T) {
	// sep=true is what domain-shaped fields use.
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		// The whole reason for a separator: `rcpt_domain: "ngm.dev"` in the
		// Haraka table did not match mail.ngm.dev, and an operator reaching for
		// a wildcard expects the subdomain form to be explicit about it.
		{"*.partner.com", "mx.partner.com", true},
		{"*.partner.com", "partner.com", false},
		{"*.partner.com", "a.b.partner.com", false},
		{"**.partner.com", "a.b.partner.com", false},
		{"*.*.partner.com", "a.b.partner.com", true},
		{"partner.com", "partner.com", true},
		{"partner.com", "PARTNER.COM", true}, // ci
		{"?x.partner.com", "mx.partner.com", true},
		{"?.partner.com", "mx.partner.com", false},
	}
	for _, c := range cases {
		g, err := compileGlob(c.pattern, true, true)
		if err != nil {
			t.Fatalf("compile %q: %v", c.pattern, err)
		}
		if got := g.match(c.subject); got != c.want {
			t.Errorf("glob %q vs %q: got %v, want %v", c.pattern, c.subject, got, c.want)
		}
	}
}

func TestGlob_FilenamesCrossDots(t *testing.T) {
	// sep=false, so `*.exe` still catches a filename with dots in it.
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"*.exe", "setup.exe", true},
		{"*.exe", "report.q3.exe", true},
		{"*.exe", "exe", false},
		{"*.{exe,scr,js,vbs}", "payload.scr", true},
		{"*.{exe,scr,js,vbs}", "notes.txt", false},
		{"invoice*.pdf", "invoice-2026.pdf", true},
	}
	for _, c := range cases {
		g, err := compileGlob(c.pattern, false, true)
		if err != nil {
			t.Fatalf("compile %q: %v", c.pattern, err)
		}
		if got := g.match(c.subject); got != c.want {
			t.Errorf("glob %q vs %q: got %v, want %v", c.pattern, c.subject, got, c.want)
		}
	}
}

func TestGlob_BraceExpansion(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"{a,b}c", "ac", true},
		{"{a,b}c", "bc", true},
		{"{a,b}c", "cc", false},
		{"x{1,2}y{3,4}z", "x2y4z", true},
		{"x{1,2}y{3,4}z", "x2y5z", false},
		{"a{b,{c,d}}e", "ade", true},
		{"a{b,{c,d}}e", "abe", true},
		{"a{b,{c,d}}e", "aee", false},
	}
	for _, c := range cases {
		g, err := compileGlob(c.pattern, false, false)
		if err != nil {
			t.Fatalf("compile %q: %v", c.pattern, err)
		}
		if got := g.match(c.subject); got != c.want {
			t.Errorf("glob %q vs %q: got %v, want %v", c.pattern, c.subject, got, c.want)
		}
	}
}

func TestGlob_Escaping(t *testing.T) {
	g, err := compileGlob(`a\*b`, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !g.match("a*b") {
		t.Error(`\* must match a literal asterisk`)
	}
	if g.match("axxb") {
		t.Error(`\* must not behave as a wildcard`)
	}
}

func TestGlob_CaseSensitivity(t *testing.T) {
	ci, _ := compileGlob("*.EXE", false, true)
	if !ci.match("setup.exe") {
		t.Error("case-insensitive glob must fold case")
	}
	cs, _ := compileGlob("*.EXE", false, false)
	if cs.match("setup.exe") {
		t.Error("case-sensitive glob must not fold case")
	}
}

func TestGlob_RejectsMalformedPatterns(t *testing.T) {
	for _, p := range []string{"", "a{b", "a}b", "{a,{b}"} {
		if _, err := compileGlob(p, false, false); err == nil {
			t.Errorf("pattern %q should not compile", p)
		}
	}
}

// A pattern must not be able to allocate unboundedly at load time.
func TestGlob_BoundsBraceExpansion(t *testing.T) {
	pattern := ""
	for range 12 {
		pattern += "{a,b}"
	}
	if _, err := compileGlob(pattern, false, false); err == nil {
		t.Error("4096 alternatives should exceed the expansion limit")
	}
}
