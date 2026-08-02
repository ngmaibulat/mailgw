package attach

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// md5of is what an operator's `md5sum` would print for the DECODED content, and
// therefore what a row in logservice's BlockMD5s table holds. Every expectation
// below is written as the literal bytes rather than a copied digest, so a change
// to the decoding is a visible diff instead of a hex string that stops matching.
func md5of(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// crlf renders a fixture the way it arrives over SMTP. The fixtures are stored
// LF-only so they stay diffable; every case runs both ways, because the line
// ending is exactly the kind of difference that makes a hash stop matching in
// production but not in tests.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

func walkBoth(t *testing.T, raw string, opts Options) (Result, error) {
	t.Helper()
	lf, lfErr := Walk(strings.NewReader(raw), opts)
	cr, crErr := Walk(strings.NewReader(crlf(raw)), opts)

	if (lfErr == nil) != (crErr == nil) {
		t.Fatalf("LF and CRLF disagree on error: %v vs %v", lfErr, crErr)
	}
	if lf.PartCount != cr.PartCount || len(lf.Parts) != len(cr.Parts) {
		t.Fatalf("LF and CRLF disagree on structure: %+v vs %+v", lf, cr)
	}
	for i := range lf.Parts {
		if lf.Parts[i] != cr.Parts[i] {
			t.Fatalf("LF and CRLF disagree on part %d:\n LF: %+v\nCRLF: %+v", i, lf.Parts[i], cr.Parts[i])
		}
	}
	return cr, crErr
}

func TestWalk_NestedMultipartFindsTheBase64Attachment(t *testing.T) {
	res, err := walkBoth(t, load(t, "nested.eml"), Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// root + alternative + text/plain + text/html + the attachment.
	if res.PartCount != 5 {
		t.Errorf("PartCount = %d, want 5", res.PartCount)
	}
	if len(res.Parts) != 1 {
		t.Fatalf("found %d attachments, want 1: %+v", len(res.Parts), res.Parts)
	}

	got := res.Parts[0]
	want := Part{
		Filename:    "report.q3.exe",
		ContentType: "application/octet-stream",
		MD5:         md5of("hello attachment\n"),
		Disposition: "attachment",
		Size:        int64(len("hello attachment\n")),
	}
	if got != want {
		t.Errorf("part =\n %+v\nwant\n %+v", got, want)
	}
}

// The MD5 must be over the DECODED bytes. mailparser hashes decoded content
// (AttachChecker.js:59), so every digest already in BlockMD5s is a decoded-content
// digest — hashing the base64 text would silently stop matching all of them.
func TestWalk_HashesDecodedBytesNotTheEncoding(t *testing.T) {
	res, err := walkBoth(t, load(t, "nested.eml"), Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if enc := md5of("aGVsbG8gYXR0YWNobWVudAo=\n"); res.Parts[0].MD5 == enc {
		t.Fatal("hashed the base64 text rather than the decoded content")
	}
}

// AttachChecker.js:51 returned early for any part whose disposition was not
// literally "attachment", so this part was never hashed at all.
func TestWalk_InlineWithAFilenameIsAnAttachment(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(fmt.Sprintf("include_inline=%v", inline), func(t *testing.T) {
			res, err := walkBoth(t, load(t, "inline_filename.eml"), Options{IncludeInline: inline})
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(res.Parts) != 1 {
				t.Fatalf("found %d attachments, want 1: %+v", len(res.Parts), res.Parts)
			}
			got := res.Parts[0]
			if got.Filename != "invoice.exe" || got.Disposition != "inline" {
				t.Errorf("part = %+v", got)
			}
			if got.MD5 != md5of("inline payload\n") {
				t.Errorf("MD5 = %s, want %s", got.MD5, md5of("inline payload\n"))
			}
		})
	}
}

func TestWalk_DecodesQuotedPrintable(t *testing.T) {
	res, err := walkBoth(t, load(t, "quoted_printable.eml"), Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Parts) != 1 {
		t.Fatalf("found %d attachments, want 1", len(res.Parts))
	}
	// The newline before the closing boundary belongs to the boundary, not the
	// part, so the decoded content has no trailing newline.
	const want = "quoted printable = fun"
	if res.Parts[0].MD5 != md5of(want) {
		t.Errorf("MD5 = %s, want md5(%q) = %s", res.Parts[0].MD5, want, md5of(want))
	}
	if res.Parts[0].Size != int64(len(want)) {
		t.Errorf("Size = %d, want %d", res.Parts[0].Size, len(want))
	}
}

// A nameless Content-ID image is what include_inline is for: it is not one of
// the message's own text bodies, and it is not covered by the filename rule.
func TestWalk_IncludeInlineCoversNamelessNonTextParts(t *testing.T) {
	raw := load(t, "nameless_inline.eml")

	off, err := walkBoth(t, raw, Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(off.Parts) != 0 {
		t.Errorf("include_inline off: found %d attachments, want 0: %+v", len(off.Parts), off.Parts)
	}

	on, err := walkBoth(t, raw, Options{IncludeInline: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(on.Parts) != 1 {
		t.Fatalf("include_inline on: found %d attachments, want 1", len(on.Parts))
	}
	if on.Parts[0].ContentType != "image/png" || on.Parts[0].Disposition != "" {
		t.Errorf("part = %+v", on.Parts[0])
	}
	if on.Parts[0].MD5 != md5of("PNG-ish bytes\n") {
		t.Errorf("MD5 = %s, want %s", on.Parts[0].MD5, md5of("PNG-ish bytes\n"))
	}
}

func TestWalk_PlainMessageIsOnePartAndNoAttachments(t *testing.T) {
	res, err := walkBoth(t, load(t, "plain.eml"), Options{IncludeInline: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.PartCount != 1 {
		t.Errorf("PartCount = %d, want 1", res.PartCount)
	}
	if len(res.Parts) != 0 {
		t.Errorf("found %d attachments in a plain-text message: %+v", len(res.Parts), res.Parts)
	}
}

// AttachChecker.js:77 turned this into "allow", so a deliberately malformed
// multipart skipped the scanner. It must be an error the caller can act on.
func TestWalk_MalformedMultipartIsAnError(t *testing.T) {
	res, err := Walk(strings.NewReader(crlf(load(t, "no_boundary.eml"))), Options{})
	if err == nil {
		t.Fatalf("a multipart with no boundary parsed cleanly: %+v", res)
	}
	if len(res.Parts) != 0 {
		t.Errorf("reported attachments from an unparseable message: %+v", res.Parts)
	}
}

func TestWalk_DecodesRFC2047Filenames(t *testing.T) {
	res, err := walkBoth(t, load(t, "encoded_word.eml"), Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Parts) != 1 {
		t.Fatalf("found %d attachments, want 1", len(res.Parts))
	}
	if got := res.Parts[0].Filename; got != "résumé.pdf" {
		t.Errorf("Filename = %q, want %q", got, "résumé.pdf")
	}
}

// A small message can declare an unbounded structure. The walk must stop and say
// so rather than recurse until the stack gives out.
func TestWalk_StopsAtTheDepthLimit(t *testing.T) {
	// Each level is one multipart wrapping the next, with a text/plain at the
	// bottom — a few hundred bytes that declare a structure deeper than the walk
	// is willing to follow.
	var nest func(level int) string
	nest = func(level int) string {
		if level == maxDepth+5 {
			return "Content-Type: text/plain\r\n\r\nbottom\r\n"
		}
		return fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"B%d\"\r\n\r\n--B%d\r\n%s--B%d--\r\n",
			level, level, nest(level+1), level)
	}

	res, err := Walk(strings.NewReader("MIME-Version: 1.0\r\n"+nest(0)), Options{IncludeInline: true})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated (parts=%d)", err, res.PartCount)
	}
	if res.PartCount > maxParts {
		t.Errorf("walked %d parts past the limit of %d", res.PartCount, maxParts)
	}
}

func TestWalk_StopsAtThePartLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"B\"\r\n\r\n")
	for i := 0; i < maxParts+50; i++ {
		fmt.Fprintf(&b, "--B\r\nContent-Type: text/plain\r\n\r\npart %d\r\n", i)
	}
	b.WriteString("--B--\r\n")

	res, err := Walk(strings.NewReader(b.String()), Options{})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if res.PartCount > maxParts+1 {
		t.Errorf("walked %d parts, limit is %d", res.PartCount, maxParts)
	}
}

// The classification rule, exercised directly: it is the thing the legacy
// implementation got wrong, and it should be readable as a table.
func TestIsAttachment(t *testing.T) {
	cases := []struct {
		name      string
		mediaType string
		disp      string
		filename  string
		inline    bool
		want      bool
	}{
		{"explicit attachment", "application/pdf", "attachment", "a.pdf", false, true},
		{"attachment without a name", "application/pdf", "attachment", "", false, true},
		{"inline with a filename", "application/octet-stream", "inline", "x.exe", false, true},
		{"content-type name only", "application/octet-stream", "", "x.exe", false, true},
		{"nameless octet-stream, inline off", "application/octet-stream", "", "", false, false},
		{"nameless octet-stream, inline on", "application/octet-stream", "", "", true, true},
		{"text body, inline on", "text/plain", "", "", true, false},
		{"html body, inline on", "text/html", "", "", true, false},
		{"html body sent as an attachment", "text/html", "attachment", "", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isAttachment(c.mediaType, c.disp, c.filename, Options{IncludeInline: c.inline})
			if got != c.want {
				t.Errorf("isAttachment(%q, %q, %q, inline=%v) = %v, want %v",
					c.mediaType, c.disp, c.filename, c.inline, got, c.want)
			}
		})
	}
}
