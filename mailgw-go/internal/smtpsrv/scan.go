package smtpsrv

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/textproto"
	"strings"
)

// maxHeaderCapture bounds how much of a message is buffered while looking for
// the end of the header block. A message that never ends its headers must not
// be able to make the gateway allocate without limit.
const maxHeaderCapture = 1 << 20 // 1 MiB

// errTooManyHeaderLines ends the read once the header block has grown past
// max.header_lines.
//
// It surfaces as an error from Read rather than as a check in Data because this
// is the only thing that sees the header block stream past, and captureHeader
// has nowhere to return one. Spool.WriteBody wraps it with %w, so it reaches
// spoolRefusal by the same route smtp.ErrDataTooLarge does and earns the same
// 552 — one mechanism for both limits rather than two.
var errTooManyHeaderLines = errors.New("smtpsrv: message header exceeds max.header_lines")

// bodyScan wraps the DATA reader and collects the facts the data-stage rules
// need, in the single pass that already streams the message to disk.
//
// Reading the spooled file back would be simpler, but it doubles the I/O for
// every message and gives the page cache a 25 MiB working set per concurrent
// session for no benefit.
type bodyScan struct {
	r io.Reader

	// maxHeaderLines is max.header_lines; 0 disables the limit.
	maxHeaderLines int

	lines    int64
	total    int64
	hdr      bytes.Buffer
	hdrDone  bool
	hdrLines int64
	scanFrom int
	// err is sticky: once a limit has been exceeded the message is refused, and
	// no later read may un-refuse it.
	err error
}

func newBodyScan(r io.Reader, maxHeaderLines int) *bodyScan {
	return &bodyScan{r: r, maxHeaderLines: maxHeaderLines}
}

func (s *bodyScan) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	if n > 0 {
		chunk := p[:n]
		s.total += int64(n)
		s.lines += int64(bytes.Count(chunk, []byte{'\n'}))
		if !s.hdrDone {
			s.captureHeader(chunk)
		}
	}
	// Reported the moment the limit trips, so the rest of an abusive message is
	// never streamed to disk. io.Copy writes the n bytes it was handed and then
	// stops on the error, and WriteBody's defer removes the temp file.
	if s.err != nil {
		return n, s.err
	}
	return n, err
}

// captureHeader accumulates bytes until the blank line that ends the header
// block. The search resumes three bytes back from the previous end so a
// "\r\n\r\n" split across two reads is still found.
func (s *bodyScan) captureHeader(chunk []byte) {
	room := maxHeaderCapture - s.hdr.Len()
	if room <= 0 {
		s.hdrDone = true
		return
	}
	if len(chunk) > room {
		chunk = chunk[:room]
	}
	s.hdr.Write(chunk)

	buf := s.hdr.Bytes()
	from := s.scanFrom - 3
	if from < 0 {
		from = 0
	}
	if i := bytes.Index(buf[from:], []byte("\r\n\r\n")); i >= 0 {
		s.hdr.Truncate(from + i + 4)
		s.hdrDone = true
		s.countHeaderLines()
		return
	}
	if i := bytes.Index(buf[from:], []byte("\n\n")); i >= 0 {
		s.hdr.Truncate(from + i + 2)
		s.hdrDone = true
		s.countHeaderLines()
		return
	}
	s.scanFrom = len(buf)
	if s.hdr.Len() >= maxHeaderCapture {
		s.hdrDone = true
	}
	// Counted only after the terminator search, and always against the buffer
	// rather than the chunk just written. A whole message usually arrives in one
	// Read, so counting the chunk would charge every body line to the header and
	// refuse an ordinary message with a long body.
	s.countHeaderLines()
}

// countHeaderLines re-counts the captured block and trips the limit if it is
// over. Until the terminator is found every buffered byte is header, so this is
// exact both before and after truncation — and an abusive header that never ends
// still trips on the read that crosses the limit rather than at 1 MiB.
func (s *bodyScan) countHeaderLines() {
	s.hdrLines = int64(bytes.Count(s.hdr.Bytes(), []byte{'\n'}))
	if s.err == nil && s.maxHeaderLines > 0 && s.hdrLines > int64(s.maxHeaderLines) {
		s.err = errTooManyHeaderLines
	}
}

// rawHeaders returns the captured header block verbatim.
//
// A delivery status notification quotes the original message's headers, and this
// is already in hand — reading the spooled file back to recover text that was
// buffered on the way past would double the I/O for a message that has, by
// definition, just gone wrong.
func (s *bodyScan) rawHeaders() string { return s.hdr.String() }

// headers parses the captured block into lowercase-keyed values.
//
// A malformed header line is not fatal: textproto returns what it managed to
// read, and routing on a partial header set is better than failing a message
// that a receiving MTA would have accepted.
func (s *bodyScan) headers() map[string][]string {
	raw := s.hdr.Bytes()
	if !s.hdrDone {
		// No blank line was seen; ReadMIMEHeader needs a terminator.
		raw = append(append([]byte(nil), raw...), '\r', '\n', '\r', '\n')
	}

	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	mh, _ := tp.ReadMIMEHeader()

	out := make(map[string][]string, len(mh))
	for k, v := range mh {
		out[strings.ToLower(k)] = v
	}
	return out
}
