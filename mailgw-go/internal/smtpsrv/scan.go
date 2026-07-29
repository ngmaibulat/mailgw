package smtpsrv

import (
	"bufio"
	"bytes"
	"io"
	"net/textproto"
	"strings"
)

// maxHeaderCapture bounds how much of a message is buffered while looking for
// the end of the header block. A message that never ends its headers must not
// be able to make the gateway allocate without limit.
const maxHeaderCapture = 1 << 20 // 1 MiB

// bodyScan wraps the DATA reader and collects the facts the data-stage rules
// need, in the single pass that already streams the message to disk.
//
// Reading the spooled file back would be simpler, but it doubles the I/O for
// every message and gives the page cache a 25 MiB working set per concurrent
// session for no benefit.
type bodyScan struct {
	r io.Reader

	lines    int64
	total    int64
	hdr      bytes.Buffer
	hdrDone  bool
	scanFrom int
}

func newBodyScan(r io.Reader) *bodyScan { return &bodyScan{r: r} }

func (s *bodyScan) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		chunk := p[:n]
		s.total += int64(n)
		s.lines += int64(bytes.Count(chunk, []byte{'\n'}))
		if !s.hdrDone {
			s.captureHeader(chunk)
		}
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
		return
	}
	if i := bytes.Index(buf[from:], []byte("\n\n")); i >= 0 {
		s.hdr.Truncate(from + i + 2)
		s.hdrDone = true
		return
	}
	s.scanFrom = len(buf)
	if s.hdr.Len() >= maxHeaderCapture {
		s.hdrDone = true
	}
}

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
