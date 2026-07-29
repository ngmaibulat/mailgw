package ruleset

import (
	"fmt"
	"sort"
	"strings"
)

// Stage is the point in the SMTP conversation at which a field becomes known.
//
// Stages are ordered, and a rule can only be evaluated once every field it
// mentions is available. That ordering is what lets a per-recipient rejection
// fire at RCPT rather than being deferred to the end of DATA.
type Stage uint8

const (
	StageConnect Stage = iota
	StageHelo
	StageMail
	StageRcpt
	StageData
)

func (s Stage) String() string {
	switch s {
	case StageConnect:
		return "connect"
	case StageHelo:
		return "helo"
	case StageMail:
		return "mail"
	case StageRcpt:
		return "rcpt"
	case StageData:
		return "data"
	}
	return fmt.Sprintf("stage(%d)", uint8(s))
}

// ParseStage resolves a stage name from configuration.
func ParseStage(s string) (Stage, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "connect":
		return StageConnect, nil
	case "helo", "ehlo":
		return StageHelo, nil
	case "mail", "mail_from":
		return StageMail, nil
	case "rcpt", "rcpt_to":
		return StageRcpt, nil
	case "data", "data_post", "eod":
		return StageData, nil
	}
	return 0, fmt.Errorf("unknown stage %q (want connect|helo|mail|rcpt|data)", s)
}

// Kind is the type of a field's value. Operators are checked against it at
// compile time, so `msg.size gt "big"` is a config error rather than a
// surprise at runtime.
type Kind uint8

const (
	KindString Kind = iota
	KindInt
	KindBool
	KindIP
)

func (k Kind) String() string {
	switch k {
	case KindString:
		return "string"
	case KindInt:
		return "int"
	case KindBool:
		return "bool"
	case KindIP:
		return "ip"
	}
	return "unknown"
}

// FieldDef describes one matchable field.
type FieldDef struct {
	Name  string
	Stage Stage
	Kind  Kind

	// List marks a multi-valued field. A leaf predicate over a list is
	// existential ("some element matches"); `every` gives the universal form.
	List bool

	// CI makes string comparison case-insensitive by default. Set for
	// addresses, domains and header names, where case is not significant; a
	// rule can override it per node with `ci`.
	CI bool

	// DotGlob makes `*` in a glob pattern stop at a dot, so `*.partner.com`
	// matches `mx.partner.com` but not `partner.com` — the behaviour an
	// operator expects from a hierarchical name. Filenames get the opposite,
	// so `*.exe` matches `report.q3.exe`.
	DotGlob bool

	Desc string
}

// prefixDef is a family of fields with a dynamic suffix, such as
// `header.subject`. The suffix is arbitrary, so these cannot be enumerated.
type prefixDef struct {
	Prefix string
	Def    FieldDef
}

// Schema is the field registry. Every field named by a rule must resolve here,
// which turns a typo into a load-time error instead of a rule that silently
// never matches.
type Schema struct {
	byName   map[string]FieldDef
	prefixes []prefixDef
}

// DefaultSchema returns the built-in field registry.
//
// Compare with what Haraka's matcher could see: mailgw/plugins/npRoute.js:54-55
// passes exactly two values, `sender` and `rcpt_to[0]`, from which Route.js
// derives four fields. Everything below was simply unavailable.
func DefaultSchema() *Schema {
	defs := []FieldDef{
		// ── connect ──────────────────────────────────────────────────────
		{Name: "conn.remote_ip", Stage: StageConnect, Kind: KindIP, Desc: "peer IP address"},
		{Name: "conn.remote_port", Stage: StageConnect, Kind: KindInt, Desc: "peer TCP port"},
		{Name: "conn.local_ip", Stage: StageConnect, Kind: KindIP, Desc: "local IP the peer connected to"},
		{Name: "conn.local_port", Stage: StageConnect, Kind: KindInt, Desc: "local TCP port"},
		{Name: "conn.is_private", Stage: StageConnect, Kind: KindBool, Desc: "peer is in RFC1918/ULA space"},
		{Name: "conn.is_loopback", Stage: StageConnect, Kind: KindBool, Desc: "peer is on the loopback interface"},

		// ── helo ─────────────────────────────────────────────────────────
		{Name: "helo.name", Stage: StageHelo, Kind: KindString, CI: true, DotGlob: true, Desc: "name given in EHLO/HELO"},
		{Name: "conn.tls", Stage: StageHelo, Kind: KindBool, Desc: "session is encrypted"},
		{Name: "conn.tls_version", Stage: StageHelo, Kind: KindString, CI: true, Desc: "negotiated TLS version, e.g. TLS1.3"},
		{Name: "conn.tls_cipher", Stage: StageHelo, Kind: KindString, CI: true, Desc: "negotiated cipher suite"},
		{Name: "auth.user", Stage: StageHelo, Kind: KindString, CI: true, Desc: "authenticated user, empty when unauthenticated"},
		{Name: "auth.mechanism", Stage: StageHelo, Kind: KindString, CI: true, Desc: "SASL mechanism used"},

		// ── mail ─────────────────────────────────────────────────────────
		{Name: "mail.from", Stage: StageMail, Kind: KindString, CI: true, Desc: "envelope sender, empty for a null sender"},
		{Name: "mail.from_local", Stage: StageMail, Kind: KindString, CI: true, Desc: "local part of the envelope sender"},
		{Name: "mail.from_domain", Stage: StageMail, Kind: KindString, CI: true, DotGlob: true, Desc: "domain of the envelope sender"},
		{Name: "mail.is_null_sender", Stage: StageMail, Kind: KindBool, Desc: "MAIL FROM:<>"},
		{Name: "mail.is_dsn", Stage: StageMail, Kind: KindBool, Desc: "message is a bounce (null sender)"},
		{Name: "mail.size_declared", Stage: StageMail, Kind: KindInt, Desc: "SIZE= parameter, 0 when absent"},
		{Name: "mail.body", Stage: StageMail, Kind: KindString, CI: true, Desc: "BODY= parameter: 7BIT, 8BITMIME or BINARYMIME"},
		{Name: "mail.smtputf8", Stage: StageMail, Kind: KindBool, Desc: "SMTPUTF8 was requested"},
		{Name: "mail.requiretls", Stage: StageMail, Kind: KindBool, Desc: "REQUIRETLS was requested"},

		// ── rcpt ─────────────────────────────────────────────────────────
		{Name: "rcpt.to", Stage: StageRcpt, Kind: KindString, CI: true, Desc: "the recipient being evaluated"},
		{Name: "rcpt.local", Stage: StageRcpt, Kind: KindString, CI: true, Desc: "local part of the recipient"},
		{Name: "rcpt.domain", Stage: StageRcpt, Kind: KindString, CI: true, DotGlob: true, Desc: "domain of the recipient"},
		{Name: "rcpt.index", Stage: StageRcpt, Kind: KindInt, Desc: "1-based position of this recipient"},
		{Name: "txn.rcpt_count_so_far", Stage: StageRcpt, Kind: KindInt, Desc: "recipients accepted so far"},

		// ── data ─────────────────────────────────────────────────────────
		{Name: "msg.size", Stage: StageData, Kind: KindInt, Desc: "message size in bytes"},
		{Name: "msg.line_count", Stage: StageData, Kind: KindInt, Desc: "lines in the message"},
		{Name: "msg.received_count", Stage: StageData, Kind: KindInt, Desc: "Received: headers — the hop count"},
		{Name: "msg.mime_part_count", Stage: StageData, Kind: KindInt, Desc: "MIME parts"},
		{Name: "msg.has_attachment", Stage: StageData, Kind: KindBool, Desc: "at least one attachment part"},
		{Name: "txn.rcpt_count", Stage: StageData, Kind: KindInt, Desc: "total accepted recipients"},
		{Name: "txn.rcpt_domains", Stage: StageData, Kind: KindString, List: true, CI: true, DotGlob: true, Desc: "distinct recipient domains"},
		{Name: "attachment.filename", Stage: StageData, Kind: KindString, List: true, CI: true, Desc: "attachment filenames"},
		{Name: "attachment.content_type", Stage: StageData, Kind: KindString, List: true, CI: true, Desc: "attachment MIME types"},
		{Name: "attachment.md5", Stage: StageData, Kind: KindString, List: true, CI: true, Desc: "attachment MD5 digests"},
		{Name: "attachment.size", Stage: StageData, Kind: KindInt, List: true, Desc: "attachment sizes in bytes"},
		{Name: "attachment.disposition", Stage: StageData, Kind: KindString, List: true, CI: true, Desc: "attachment dispositions"},
	}

	s := &Schema{byName: make(map[string]FieldDef, len(defs))}
	for _, d := range defs {
		s.byName[d.Name] = d
	}

	s.prefixes = []prefixDef{
		{"header.", FieldDef{Stage: StageData, Kind: KindString, List: true, CI: true,
			Desc: "values of the named header; a repeated header yields several"}},
		{"header_count.", FieldDef{Stage: StageData, Kind: KindInt,
			Desc: "how many times the named header appears"}},
		// Tags are set by earlier rules, so they are readable from the stage the
		// rule that sets them ran at. Connect is the safe floor: it never forces
		// a rule to defer.
		{"tag.", FieldDef{Stage: StageConnect, Kind: KindString, CI: true,
			Desc: "a tag set by an earlier rule"}},
	}
	return s
}

// Lookup resolves a field name, expanding dynamic prefixes.
func (s *Schema) Lookup(name string) (FieldDef, bool) {
	if d, ok := s.byName[name]; ok {
		return d, true
	}
	for _, p := range s.prefixes {
		if suffix, ok := strings.CutPrefix(name, p.Prefix); ok && suffix != "" {
			d := p.Def
			d.Name = name
			return d, true
		}
	}
	return FieldDef{}, false
}

// Names lists every static field, sorted — used by `check` and by error
// messages that suggest what the author might have meant.
func (s *Schema) Names() []string {
	out := make([]string, 0, len(s.byName))
	for n := range s.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Prefixes lists the dynamic field families.
func (s *Schema) Prefixes() []string {
	out := make([]string, 0, len(s.prefixes))
	for _, p := range s.prefixes {
		out = append(out, p.Prefix+"*")
	}
	return out
}

// Describe returns every field with its stage, kind and description, for
// `mailgw-go fields`.
func (s *Schema) Describe() []FieldDef {
	out := make([]FieldDef, 0, len(s.byName)+len(s.prefixes))
	for _, d := range s.byName {
		out = append(out, d)
	}
	for _, p := range s.prefixes {
		d := p.Def
		d.Name = p.Prefix + "<name>"
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stage != out[j].Stage {
			return out[i].Stage < out[j].Stage
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// suggest finds registry entries close to an unknown name, so a typo reports
// something more useful than "unknown field".
func (s *Schema) suggest(name string) []string {
	var out []string
	lower := strings.ToLower(name)
	base := lower
	if i := strings.LastIndexByte(lower, '.'); i >= 0 {
		base = lower[i+1:]
	}
	for n := range s.byName {
		ln := strings.ToLower(n)
		if strings.Contains(ln, base) || strings.Contains(lower, ln) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}
