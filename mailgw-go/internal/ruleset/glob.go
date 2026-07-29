package ruleset

import (
	"fmt"
	"strings"
)

// maxGlobExpansion caps brace expansion. `{a,b}{c,d}{e,f}…` multiplies, and a
// config file should not be able to allocate unboundedly at load time.
const maxGlobExpansion = 1024

// globMatcher tests a string against one or more alternative patterns.
type globMatcher struct {
	pats [][]rune
	sep  bool // '*' and '?' do not cross '.'
	ci   bool
}

// compileGlob builds a matcher for a glob pattern.
//
// Supported syntax is deliberately small: `*`, `?`, `{a,b,c}` alternation, and
// `\` to escape any of them. Character classes are not supported — `regex`
// covers that, and two overlapping mini-languages is one too many.
//
// When sep is set, `*` and `?` will not cross a dot. That is what makes
// `*.partner.com` match `mx.partner.com` but not `partner.com`, which is the
// behaviour an operator expects from a domain and the single most common
// mistake in hand-written mail routing. Filenames use sep=false, so `*.exe`
// still matches `report.q3.exe`.
func compileGlob(pattern string, sep, ci bool) (*globMatcher, error) {
	if pattern == "" {
		return nil, fmt.Errorf("empty glob pattern")
	}
	alts, err := expandBraces(pattern)
	if err != nil {
		return nil, err
	}

	m := &globMatcher{sep: sep, ci: ci, pats: make([][]rune, 0, len(alts))}
	for _, a := range alts {
		if ci {
			a = strings.ToLower(a)
		}
		m.pats = append(m.pats, []rune(a))
	}
	return m, nil
}

func (m *globMatcher) match(s string) bool {
	if m.ci {
		s = strings.ToLower(s)
	}
	rs := []rune(s)
	for _, p := range m.pats {
		if globMatch(p, rs, m.sep) {
			return true
		}
	}
	return false
}

// globMatch is the standard linear wildcard matcher with a single backtrack
// point, extended with an optional separator that `*` may not cross.
func globMatch(pat, s []rune, sep bool) bool {
	star, starS := -1, 0
	p, i := 0, 0

	for i < len(s) {
		matched := false
		if p < len(pat) {
			switch c := pat[p]; {
			case c == '\\' && p+1 < len(pat):
				if pat[p+1] == s[i] {
					p, i = p+2, i+1
					matched = true
				}
			case c == '?':
				if !sep || s[i] != '.' {
					p, i = p+1, i+1
					matched = true
				}
			case c == '*':
				star, starS = p, i
				p++
				matched = true
			case c == s[i]:
				p, i = p+1, i+1
				matched = true
			}
		}
		if matched {
			continue
		}
		if star < 0 {
			return false
		}
		// Let the last '*' swallow one more character, unless that character is
		// a separator it is not permitted to cross.
		if sep && s[starS] == '.' {
			return false
		}
		starS++
		i, p = starS, star+1
	}

	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

// expandBraces turns "a{b,c}d" into ["abd", "acd"], recursively.
func expandBraces(pattern string) ([]string, error) {
	out, err := expand(pattern)
	if err != nil {
		return nil, err
	}
	if len(out) > maxGlobExpansion {
		return nil, fmt.Errorf("glob %q expands to %d alternatives (limit %d)", pattern, len(out), maxGlobExpansion)
	}
	return out, nil
}

func expand(s string) ([]string, error) {
	open := -1
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped byte
		case '{':
			if depth == 0 {
				open = i
			}
			depth++
		case '}':
			if depth == 0 {
				return nil, fmt.Errorf("glob %q: unmatched '}' at %d", s, i)
			}
			depth--
			if depth > 0 {
				continue
			}

			prefix, body, suffix := s[:open], s[open+1:i], s[i+1:]
			tails, err := expand(suffix)
			if err != nil {
				return nil, err
			}

			var out []string
			for _, alt := range splitTopLevel(body) {
				heads, err := expand(prefix + alt)
				if err != nil {
					return nil, err
				}
				for _, h := range heads {
					for _, t := range tails {
						out = append(out, h+t)
						if len(out) > maxGlobExpansion {
							return nil, fmt.Errorf("glob %q expands past the limit of %d alternatives", s, maxGlobExpansion)
						}
					}
				}
			}
			return out, nil
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("glob %q: unmatched '{'", s)
	}
	return []string{s}, nil
}

// splitTopLevel splits on commas that are not inside a nested brace group.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
