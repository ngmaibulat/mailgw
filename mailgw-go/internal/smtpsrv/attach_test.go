package smtpsrv

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/attach"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// fakeScanner stands in for logservice. It records what it was asked about, so a
// test can assert the gateway sent the digests it claims to have computed.
type fakeScanner struct {
	verdict attach.Verdict
	err     error

	mu   sync.Mutex
	seen []attach.Part
	txn  string
	call int
}

func (f *fakeScanner) Check(_ context.Context, txnUUID string, parts []attach.Part) (attach.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call++
	f.txn = txnUUID
	f.seen = append(f.seen, parts...)
	if f.err != nil {
		return "", f.err
	}
	return f.verdict, nil
}

func (f *fakeScanner) parts() []attach.Part {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]attach.Part(nil), f.seen...)
}

func (f *fakeScanner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.call
}

// A minimal multipart message with one base64 attachment whose decoded content
// is "hello attachment\n".
const bodyWithAttachment = "Subject: with an attachment\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
	"\r\n" +
	"--B\r\n" +
	"Content-Type: text/plain\r\n" +
	"\r\n" +
	"the body\r\n" +
	"--B\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"payload.bin\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"aGVsbG8gYXR0YWNobWVudAo=\r\n" +
	"--B--\r\n"

const relayEverything = `
version: 1
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`

// scanning starts a harness with attachment scanning on.
func scanning(t *testing.T, sc *fakeScanner, tweak func(*config.AttachConfig)) *harness {
	t.Helper()
	return startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, b *Backend) {
		cfg.Server.Attach = config.AttachConfig{Enabled: true, URL: "http://scanner.invalid", Fail: "closed"}
		if tweak != nil {
			tweak(&cfg.Server.Attach)
		}
		b.Attach = sc
	})
}

// blockingScanner blocks until its context is cancelled, and reports which it
// was given.
type blockingScanner struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingScanner) Check(ctx context.Context, _ string, _ []attach.Part) (attach.Verdict, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return "", ctx.Err()
}

// The scan is an HTTP call made inside the DATA reply, so it must run on the
// process's serve context: on context.Background() a scanner that never answers
// pins the session and its goroutine straight through the shutdown budget. This
// is the same defect M8 fixed in events.Client.
func TestAttach_ScanRunsOnTheServeContextNotBackground(t *testing.T) {
	sc := &blockingScanner{entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := startServerTuned(t, compileRulesYAML(t, relayEverything), func(cfg *config.Config, b *Backend) {
		cfg.Server.Attach = config.AttachConfig{
			Enabled: true, URL: "http://scanner.invalid", Fail: "closed",
			Timeout: config.Duration(time.Minute),
		}
		b.Attach = sc
		b.Ctx = ctx
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// fail: closed, so an aborted scan answers 451 rather than relaying.
		if code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment); code != 451 {
			t.Errorf("end of DATA: got %d %q, want 451", code, raw)
		}
	}()

	select {
	case <-sc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the scanner was never called")
	}

	// Nothing but the serve context can end this scan: its own timeout is a
	// minute away and the session has no deadline of its own.
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the serve context did not release the DATA reply — " +
			"the scan is not running on it")
	}
}

func TestAttach_BlockedMessageIsRejectedAndLeavesNothingBehind(t *testing.T) {
	sc := &fakeScanner{verdict: attach.Block}
	h := scanning(t, sc, nil)

	code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment)
	if code != 550 {
		t.Fatalf("end of DATA: got %d %q, want 550", code, raw)
	}
	if !strings.Contains(strings.ToLower(raw), "attachment scan") {
		t.Errorf("reply %q does not say why", raw)
	}

	entries, err := h.spool.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a blocked message left %d envelope(s) in the spool", len(entries))
	}
	if h.metrics.MsgRejected.Load() != 1 {
		t.Errorf("msg_rejected = %d, want 1", h.metrics.MsgRejected.Load())
	}
}

// The digest the gateway sends is what makes an existing BlockMD5s row match. It
// is over the decoded content, not the base64 the message carried.
func TestAttach_SendsDecodedDigestsAndTheTransactionUUID(t *testing.T) {
	sc := &fakeScanner{verdict: attach.Allow}
	h := scanning(t, sc, nil)

	if code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment); code != 250 {
		t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
	}

	parts := sc.parts()
	if len(parts) != 1 {
		t.Fatalf("scanner saw %d parts, want 1: %+v", len(parts), parts)
	}
	// md5("hello attachment\n")
	const want = "17b8f931068345055c3e719aab14f158"
	if parts[0].MD5 != want {
		t.Errorf("MD5 = %s, want %s (the DECODED content)", parts[0].MD5, want)
	}
	if parts[0].Filename != "payload.bin" {
		t.Errorf("Filename = %q", parts[0].Filename)
	}
	if !strings.Contains(sc.txn, ".") {
		t.Errorf("txn_uuid = %q, want a transaction id like X.1 so HashLookups can join", sc.txn)
	}
}

func TestAttach_OnBlockActions(t *testing.T) {
	cases := []struct {
		onBlock  string
		wantCode int
		wantQ    string // spool queue the envelope should land in, "" for none
	}{
		{config.AttachBlockReject, 550, ""},
		{config.AttachBlockTempfail, 451, ""},
		{config.AttachBlockQuarantine, 250, queue.QueueQuarantine},
		{config.AttachBlockDiscard, 250, ""},
	}

	for _, c := range cases {
		t.Run(c.onBlock, func(t *testing.T) {
			sc := &fakeScanner{verdict: attach.Block}
			h := scanning(t, sc, func(a *config.AttachConfig) { a.OnBlock = c.onBlock })

			code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment)
			if code != c.wantCode {
				t.Fatalf("end of DATA: got %d %q, want %d", code, raw, c.wantCode)
			}

			entries, err := h.spool.ListAll()
			if err != nil {
				t.Fatalf("ListAll: %v", err)
			}
			if c.wantQ == "" {
				if len(entries) != 0 {
					t.Fatalf("left %d envelope(s) in the spool: %+v", len(entries), entries)
				}
				return
			}
			if len(entries) != 1 || entries[0].Queue != c.wantQ {
				t.Fatalf("envelopes = %+v, want exactly one in %s", entries, c.wantQ)
			}
		})
	}
}

// AttachChecker.js:36 and :77 turned every failure into "allow". attach.fail is
// what makes that a choice rather than the only behaviour.
func TestAttach_ScannerFailureHonoursAttachFail(t *testing.T) {
	t.Run("closed refuses with a 4xx", func(t *testing.T) {
		sc := &fakeScanner{err: context.DeadlineExceeded}
		h := scanning(t, sc, nil)

		code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment)
		// 4xx, not 5xx: the scanner being down is not the sender's fault, and a
		// 550 would turn an outage into permanently lost mail.
		if code != 451 {
			t.Fatalf("end of DATA: got %d %q, want 451", code, raw)
		}
		if h.metrics.MsgTempfailed.Load() != 1 {
			t.Errorf("msg_tempfailed = %d, want 1", h.metrics.MsgTempfailed.Load())
		}
	})

	t.Run("open relays unscanned", func(t *testing.T) {
		sc := &fakeScanner{err: context.DeadlineExceeded}
		h := scanning(t, sc, func(a *config.AttachConfig) { a.Fail = "open" })

		if code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment); code != 250 {
			t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
		}
	})
}

// A malformed multipart was the more serious of the two legacy bypasses: it
// skipped the scanner entirely and the message was relayed.
func TestAttach_MalformedMIMEIsAScanFailureNotAnAllow(t *testing.T) {
	const malformed = "Subject: broken\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed\r\n" + // no boundary
		"\r\n" +
		"--B\r\n" +
		"Content-Disposition: attachment; filename=\"hidden.exe\"\r\n" +
		"\r\n" +
		"payload\r\n" +
		"--B--\r\n"

	sc := &fakeScanner{verdict: attach.Allow}
	h := scanning(t, sc, nil)

	code, raw := send(t, h, "rcpt@example.com", malformed)
	if code != 451 {
		t.Fatalf("end of DATA: got %d %q, want 451 — a message we cannot parse was not scanned", code, raw)
	}
	if sc.calls() != 0 {
		t.Errorf("the blocklist was consulted about an unparseable message")
	}
}

// An `accept` rule is the whitelist. Applying the verdict before the rules would
// leave an operator no way to override the scanner.
func TestAttach_AnAcceptRuleOverridesTheBlocklist(t *testing.T) {
	const rules = `
version: 1
policy:
  - name: trust-this-sender
    match: {field: mail.from, op: eq, value: "sender@example.com"}
    then:
      - {action: accept}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	sc := &fakeScanner{verdict: attach.Block}
	h := startServerTuned(t, compileRulesYAML(t, rules), func(cfg *config.Config, b *Backend) {
		cfg.Server.Attach = config.AttachConfig{Enabled: true, URL: "http://scanner.invalid", Fail: "closed"}
		b.Attach = sc
	})

	if code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment); code != 250 {
		t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
	}
}

// The rule engine must see the same facts the scanner did, which is the whole
// reason the walk runs before the data-stage policy pass.
func TestAttach_RulesMatchOnAttachmentFacts(t *testing.T) {
	const rules = `
version: 1
policy:
  - name: no-executables
    match: {field: attachment.filename, op: glob, value: "*.bin"}
    then:
      - {action: reject, code: 550, message: "executables are not accepted"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	rs := compileRulesYAML(t, rules)
	if !rs.NeedsMIME() {
		t.Fatal("a rule reading attachment.filename did not set NeedsMIME")
	}

	// No scanner at all: the facts must be populated for the rules alone.
	h := startServerTuned(t, rs, nil)
	code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment)
	if code != 550 || !strings.Contains(raw, "executables") {
		t.Fatalf("end of DATA: got %d %q, want 550 from the attachment rule", code, raw)
	}
}

func TestAttach_TagIsReadableByRules(t *testing.T) {
	const rules = `
version: 1
policy:
  - name: quarantine-blocked
    # tag.* is stage-agnostic, so the stage has to be stated: the verdict does
    # not exist before the message does.
    stage: data
    match: {field: tag.attach_scan, op: eq, value: block}
    then:
      - {action: quarantine}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	sc := &fakeScanner{verdict: attach.Block}
	h := startServerTuned(t, compileRulesYAML(t, rules), func(cfg *config.Config, b *Backend) {
		cfg.Server.Attach = config.AttachConfig{Enabled: true, URL: "http://scanner.invalid", Fail: "closed"}
		b.Attach = sc
	})

	if code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment); code != 250 {
		t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
	}
	entries, err := h.spool.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(entries) != 1 || entries[0].Queue != queue.QueueQuarantine {
		t.Fatalf("envelopes = %+v, want one quarantined by the tag rule", entries)
	}
}

// The walk is a re-read of the spooled body, so a configuration with no interest
// in MIME must not pay for it.
func TestAttach_NoWalkWhenNothingWouldReadIt(t *testing.T) {
	rs := compileRulesYAML(t, relayEverything)
	if rs.NeedsMIME() {
		t.Fatal("a rule set with no attachment fields reported NeedsMIME")
	}

	h := startServerTuned(t, rs, nil)
	if code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment); code != 250 {
		t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
	}

	for _, e := range h.events.sent {
		if e.Kind != events.KindQueue {
			continue
		}
		if q, ok := e.Body.(events.Queue); ok && q.MimePartCount != 0 {
			t.Errorf("mime_part_count = %d with no walk; it should be 0, not a guess", q.MimePartCount)
		}
	}
}

// mime_part_count has been on the queue payload since the Haraka days and this
// gateway has always sent 0. The walk is what finally fills it.
func TestAttach_QueueEventCarriesTheMimePartCount(t *testing.T) {
	const rules = `
version: 1
policy:
  - name: touch-the-part-count
    match: {field: msg.mime_part_count, op: ge, value: 1}
    then:
      - {action: tag, key: counted, value: "yes"}
routes:
  - name: catch-all
    match: {always: true}
    then:
      - {action: relay, relay: Outbound}
`
	h := startServerTuned(t, compileRulesYAML(t, rules), nil)
	if code, raw := send(t, h, "rcpt@example.com", bodyWithAttachment); code != 250 {
		t.Fatalf("end of DATA: got %d %q, want 250", code, raw)
	}

	var got int
	for _, e := range h.events.sent {
		if q, ok := e.Body.(events.Queue); ok && e.Kind == events.KindQueue {
			got = q.MimePartCount
		}
	}
	// root + text/plain + the attachment.
	if got != 3 {
		t.Errorf("mime_part_count = %d, want 3", got)
	}
}

func TestNeedsMIME(t *testing.T) {
	cases := map[string]bool{
		"attachment.filename": true,
		"attachment.md5":      true,
		"msg.has_attachment":  true,
		"msg.mime_part_count": true,
		"msg.size":            false,
		"header.subject":      false,
		"rcpt.domain":         false,
	}
	for field, want := range cases {
		if got := ruleset.NeedsMIMEField(field); got != want {
			t.Errorf("NeedsMIMEField(%q) = %v, want %v", field, got, want)
		}
	}
}
