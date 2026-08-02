// Package attach walks a message's MIME structure and checks the attachments it
// finds against the logservice MD5 blocklist.
//
// Like internal/dsn, this package knows nothing else: no spool, no routing, no
// configuration, no rule engine. It takes bytes and returns facts, which is what
// makes it pinnable against golden fixtures and reusable by both callers — the
// SMTP session at end of DATA, and `explain -eml`.
//
// # It replaces mailgw/plugins/AttachChecker.js, whose two bypasses are the
// # reason the feature shipped disabled
//
//   - AttachChecker.js:51 skips any part whose Content-Disposition is not
//     literally "attachment", so `Content-Disposition: inline; filename="x.exe"`
//     was never hashed. Walk classifies on the filename as well as the
//     disposition, so that part is now an attachment whatever include_inline
//     says.
//   - AttachChecker.js:77 turns a MIME parse failure into "allow", so a
//     deliberately malformed multipart skipped the scanner entirely. Walk always
//     returns the parts it did find *and* the error, so the caller can apply
//     attach.fail rather than being told everything was fine.
package attach

import (
	"bufio"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
)

// Limits on what a single message may cost us. Deliberately constants rather
// than configuration: a MIME bomb is not a tuning knob, and an operator who
// raises the limit to make one message work has disabled the guard for every
// message. The message size itself is already bounded by max.bytes.
const (
	maxParts = 500
	maxDepth = 20
)

// ErrTruncated reports that the walk stopped at a limit rather than at the end
// of the message. The parts found before that point are still returned.
var ErrTruncated = errors.New("attach: MIME structure exceeds the walk limits")

// Options selects which parts count as attachments.
type Options struct {
	// IncludeInline widens the definition to every leaf part that is not one of
	// the message's own text bodies — an image referenced by a Content-ID, a
	// nameless application/octet-stream. Defaults on in config, because
	// AttachChecker.js:51 excluding these is a bypass.
	IncludeInline bool
}

// Part is one MIME part classified as an attachment.
type Part struct {
	Filename    string
	ContentType string
	// MD5 is over the DECODED content, hex-encoded.
	MD5 string
	// Disposition is "attachment", "inline", or "" when the part declared none.
	Disposition string
	// Size is the decoded byte count, so it agrees with MD5 about what was
	// hashed.
	Size int64
}

// Result is what a walk found.
type Result struct {
	// PartCount counts every MIME part, containers included. A message with no
	// MIME structure at all is one part.
	PartCount int
	Parts     []Part
}

// Walk reads a complete RFC 5322 message and returns its attachment facts.
//
// A non-nil error never means "no facts": whatever was parsed before the failure
// is in the Result. A caller that only needs the facts for rule evaluation can
// log and carry on; a caller running the blocklist scan must treat the error as
// a scan failure and apply attach.fail, or a malformed message becomes a bypass.
func Walk(r io.Reader, opts Options) (Result, error) {
	var res Result
	tp := textproto.NewReader(bufio.NewReader(r))
	hdr, err := tp.ReadMIMEHeader()
	if err != nil && len(hdr) == 0 {
		return res, fmt.Errorf("attach: read headers: %w", err)
	}
	// A header parse that returned something is worth walking: a receiving MTA
	// would have accepted it, and routing on a partial header set is what
	// smtpsrv already does (scan.go's headers()).
	err = walkPart(&res, hdr, tp.R, opts, 0)
	return res, err
}

// walkPart handles one part, whose headers are already read and whose body is
// the remainder of r.
func walkPart(res *Result, hdr textproto.MIMEHeader, r io.Reader, opts Options, depth int) error {
	if depth > maxDepth || res.PartCount >= maxParts {
		return ErrTruncated
	}
	res.PartCount++

	mediaType, params := parseMedia(hdr.Get("Content-Type"), "text/plain")

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			// A multipart with no boundary is unparseable, and treating it as a
			// leaf would hash the raw sub-parts as if they were one blob.
			return fmt.Errorf("attach: %s part has no boundary", mediaType)
		}
		mr := multipart.NewReader(r, boundary)
		for {
			// NextRawPart, not NextPart: NextPart silently decodes
			// quoted-printable AND deletes the Content-Transfer-Encoding header,
			// so the encoding we later read would lie about what we are holding.
			// Decoding explicitly is the only way to be sure MD5 covers the same
			// bytes mailparser hashed.
			p, err := mr.NextRawPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("attach: %s: %w", mediaType, err)
			}
			err = walkPart(res, p.Header, p, opts, depth+1)
			_ = p.Close()
			if err != nil {
				return err
			}
			if res.PartCount >= maxParts {
				return ErrTruncated
			}
		}
	}

	return leaf(res, hdr, r, mediaType, params, opts)
}

// leaf classifies a non-multipart part and, when it is an attachment, hashes it.
func leaf(res *Result, hdr textproto.MIMEHeader, r io.Reader,
	mediaType string, ctParams map[string]string, opts Options) error {

	disp, dispParams := parseMedia(hdr.Get("Content-Disposition"), "")

	// filename first, then the legacy Content-Type "name" parameter. Both are
	// RFC 2231-decoded by ParseMediaType; RFC 2047 encoded-words are not, and
	// still turn up in the wild.
	filename := decodeWord(firstNonEmpty(dispParams["filename"], ctParams["name"]))

	if !isAttachment(mediaType, disp, filename, opts) {
		// Still has to be drained: the multipart reader needs this part consumed
		// before it can find the next boundary.
		_, err := io.Copy(io.Discard, r)
		return err
	}

	sum, n, err := hashDecoded(r, hdr.Get("Content-Transfer-Encoding"))
	if err != nil {
		return err
	}

	res.Parts = append(res.Parts, Part{
		Filename:    filename,
		ContentType: mediaType,
		MD5:         sum,
		Disposition: disp,
		Size:        n,
	})
	return nil
}

// isAttachment is the whole classification rule, in one place because the legacy
// version being spread across a disposition comparison and mailparser's own
// heuristics is how the inline bypass survived.
//
// A leaf part is an attachment when:
//
//	(a) it says so — Content-Disposition: attachment; or
//	(b) it names a file, in either the disposition or the content type. This
//	    alone closes AttachChecker.js:51, whatever include_inline says: a part
//	    carrying a filename is a file however it is dressed; or
//	(c) include_inline is set and it is not one of the message's own text
//	    bodies.
func isAttachment(mediaType, disposition, filename string, opts Options) bool {
	if strings.EqualFold(disposition, "attachment") {
		return true
	}
	if filename != "" {
		return true
	}
	if !opts.IncludeInline {
		return false
	}
	return !isBodyPart(mediaType)
}

// isBodyPart reports whether a nameless, undisposed leaf is the message text
// rather than content attached to it.
func isBodyPart(mediaType string) bool {
	switch strings.ToLower(mediaType) {
	case "text/plain", "text/html":
		return true
	}
	return false
}

// hashDecoded MD5s a part's content after undoing its transfer encoding, and
// returns the decoded length.
//
// Hashing the decoded bytes is not a detail: mailparser's checksum
// (AttachChecker.js:59) is over decoded content, so every md5 already sitting in
// the BlockMD5s table is a decoded-content digest. Hashing the base64 text
// instead would silently stop matching all of them.
func hashDecoded(r io.Reader, encoding string) (string, int64, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		// Strict decoding would reject the line breaks every mailer inserts, so
		// strip whitespace on the way in.
		r = base64.NewDecoder(base64.StdEncoding, &stripSpace{r: r})
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	}

	h := md5.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, fmt.Errorf("attach: decode part: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// parseMedia is ParseMediaType with a default and without the error: a header we
// cannot parse means we know nothing about that part, not that the message
// should be refused. Classification then falls back on the other signals.
func parseMedia(v, fallback string) (string, map[string]string) {
	if strings.TrimSpace(v) == "" {
		return fallback, map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(v)
	if err != nil {
		// ParseMediaType fails on a bad parameter but the type itself is usually
		// intact and is what decides whether we recurse.
		if i := strings.IndexByte(v, ';'); i >= 0 {
			v = v[:i]
		}
		return strings.ToLower(strings.TrimSpace(v)), map[string]string{}
	}
	if params == nil {
		params = map[string]string{}
	}
	return mt, params
}

// decodeWord expands RFC 2047 encoded-words. A filename we cannot decode is
// returned as-is: the raw form is still a better rule subject than "".
func decodeWord(s string) string {
	if s == "" || !strings.Contains(s, "=?") {
		return s
	}
	dec := new(mime.WordDecoder)
	if out, err := dec.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// stripSpace drops ASCII whitespace, so a base64 body wrapped at 76 columns
// decodes without StdEncoding objecting to the newlines.
type stripSpace struct{ r io.Reader }

func (s *stripSpace) Read(p []byte) (int, error) {
	for {
		n, err := s.r.Read(p)
		w := 0
		for i := 0; i < n; i++ {
			switch p[i] {
			case ' ', '\t', '\r', '\n':
			default:
				p[w] = p[i]
				w++
			}
		}
		if w > 0 || err != nil {
			return w, err
		}
		// Everything read was whitespace: read again rather than return (0, nil),
		// which io.Copy is entitled to treat as a stalled reader.
	}
}
