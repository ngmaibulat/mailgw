package smtpsrv

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// This file ports every assertion in tests/smtp/tests/smtp.test.ts so the
// contract can be checked with `go test`, without Docker or a running stack.
// The Bun suite still runs against the real binary; this is the fast loop.

// uuidScraper mirrors tests/smtp/src/smtp.ts:143.
var uuidScraper = regexp.MustCompile(`(?i)\(([0-9A-F-]+)(?:\.\d+)?\)`)

// --- harness ---

type capturedEvents struct {
	mu   sync.Mutex
	sent []events.Envelope
}

func (c *capturedEvents) Send(e events.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, e)
}

func (c *capturedEvents) kinds() []events.Kind {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]events.Kind, 0, len(c.sent))
	for _, e := range c.sent {
		out = append(out, e.Kind)
	}
	return out
}

type harness struct {
	addr     string
	spool    *queue.Spool
	events   *capturedEvents
	queued   chan []*queue.Envelope
	shutdown func()
}

func startServer(t *testing.T, routes *ruleset.LegacyTable) *harness {
	t.Helper()
	return startServerWithRules(t, compileLegacy(t, routes))
}

func startServerWithRules(t *testing.T, rs *ruleset.Ruleset) *harness {
	t.Helper()

	cfg := &config.Config{
		Server: config.Server{
			Hostname:   "devbook.local",
			Max:        config.Limits{Bytes: 26214400, LineLength: 512, Recipients: 100},
			SMTPUTF8:   true,
			Inactivity: config.Duration(30 * time.Second),
		},
		Logging: config.Logging{
			URLConn:     "http://127.0.0.1:1/api/connection",
			URLQueue:    "http://127.0.0.1:1/api/queue",
			URLDelivery: "http://127.0.0.1:1/api/delivery",
		},
	}

	spool, err := queue.NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	h := &harness{
		spool:  spool,
		events: &capturedEvents{},
		queued: make(chan []*queue.Envelope, 16),
	}

	b := &Backend{
		Cfg:      cfg,
		Rules:    StaticRules(rs),
		Spool:    spool,
		Events:   h.events,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnQueued: func(es []*queue.Envelope) { h.queued <- es },
	}

	srv, err := NewServer(b, &cfg.Server, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h.addr = l.Addr().String()

	go func() { _ = srv.Serve(l) }()
	h.shutdown = func() { _ = srv.Close() }
	t.Cleanup(h.shutdown)

	return h
}

// defaultRoutes mirrors the shipped mailgw/config/routing.json.
func defaultRoutes() *ruleset.LegacyTable {
	return &ruleset.LegacyTable{Rules: []ruleset.LegacyRule{
		{RouteName: "To Exchange", RcptDomain: "ngm.dev", Relay: "Exchange"},
		{RouteName: "Default", Relay: "Outbound"},
	}}
}

// compileLegacy runs the Haraka routing table through the transpiler and the
// DSL compiler.
//
// The whole contract suite therefore exercises the compiled rule engine, not a
// legacy shortcut — which is the point: if the transpiler changed behaviour,
// these assertions would be the first thing to notice.
func compileLegacy(t *testing.T, routes *ruleset.LegacyTable) *ruleset.Ruleset {
	t.Helper()

	tbl, err := relays.NewTable(map[string][]relays.Relay{
		"Exchange": {{Name: "Exch-01", Exchange: "127.0.0.1", Port: 25}},
		"Outbound": {{Name: "Default", Exchange: "127.0.0.1", Port: 25}},
	})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}

	rs, err := ruleset.Compile(routes.Transpile(), tbl, ruleset.DefaultSchema())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return rs
}

// client is a minimal line-oriented SMTP client, deliberately independent of
// go-smtp so the test exercises the wire protocol rather than the library.
type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dialClient(t *testing.T, addr string) *client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	t.Cleanup(func() { _ = conn.Close() })
	return &client{t: t, conn: conn, r: bufio.NewReader(conn)}
}

// readResponse consumes a full (possibly multi-line) SMTP reply.
func (c *client) readResponse() (int, string) {
	c.t.Helper()
	var lines []string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read: %v (so far: %q)", err, strings.Join(lines, "\n"))
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if len(line) < 4 || line[3] != '-' {
			break
		}
	}
	raw := strings.Join(lines, "\n")
	code, err := strconv.Atoi(raw[:3])
	if err != nil {
		c.t.Fatalf("unparseable reply %q", raw)
	}
	return code, raw
}

func (c *client) cmd(format string, args ...any) (int, string) {
	c.t.Helper()
	line := fmt.Sprintf(format, args...)
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatalf("write %q: %v", line, err)
	}
	return c.readResponse()
}

func (c *client) greet() (int, string) { return c.readResponse() }

// sendBody writes a DATA payload and its terminator.
func (c *client) sendBody(body string) (int, string) {
	c.t.Helper()
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	if _, err := c.conn.Write([]byte(body + ".\r\n")); err != nil {
		c.t.Fatalf("write body: %v", err)
	}
	return c.readResponse()
}

// --- greeting and EHLO (smtp.test.ts:40-69) ---

func TestContract_GreetingIsESMTP(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)

	code, raw := c.greet()
	if code != 220 {
		t.Errorf("greeting code: got %d, want 220", code)
	}
	// The Bun suite asserts /ESMTP|Haraka/i.
	if !regexp.MustCompile(`(?i)ESMTP|Haraka`).MatchString(raw) {
		t.Errorf("greeting %q must match /ESMTP|Haraka/i", raw)
	}
}

func TestContract_EHLOAdvertisesRequiredExtensions(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()

	code, raw := c.cmd("EHLO test.example.com")
	if code != 250 {
		t.Fatalf("EHLO: got %d (%s)", code, raw)
	}
	for _, ext := range []string{"SIZE", "PIPELINING", "8BITMIME", "SMTPUTF8"} {
		if !strings.Contains(raw, ext) {
			t.Errorf("EHLO must advertise %s; got:\n%s", ext, raw)
		}
	}
}

func TestContract_HELOWorks(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()

	if code, raw := c.cmd("HELO test.example.com"); code != 250 {
		t.Errorf("HELO: got %d (%s)", code, raw)
	}
}

// --- simple verbs (smtp.test.ts:73-91) ---

func TestContract_NoopResetQuit(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")

	if code, raw := c.cmd("NOOP"); code != 250 {
		t.Errorf("NOOP: got %d (%s)", code, raw)
	}
	if code, raw := c.cmd("RSET"); code != 250 {
		t.Errorf("RSET: got %d (%s)", code, raw)
	}
	if code, raw := c.cmd("QUIT"); code != 221 {
		t.Errorf("QUIT: got %d (%s)", code, raw)
	}
}

// --- sequencing (smtp.test.ts:95-131), all asserted as >= 500 ---

func TestContract_OutOfSequenceCommandsAre5xx(t *testing.T) {
	t.Run("MAIL before EHLO", func(t *testing.T) {
		h := startServer(t, defaultRoutes())
		c := dialClient(t, h.addr)
		c.greet()
		if code, raw := c.cmd("MAIL FROM:<me@ngm.dev>"); code < 500 {
			t.Errorf("got %d (%s), want >= 500", code, raw)
		}
	})

	t.Run("RCPT before MAIL", func(t *testing.T) {
		h := startServer(t, defaultRoutes())
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO test.example.com")
		if code, raw := c.cmd("RCPT TO:<you@ngm.dev>"); code < 500 {
			t.Errorf("got %d (%s), want >= 500", code, raw)
		}
	})

	t.Run("DATA before RCPT", func(t *testing.T) {
		h := startServer(t, defaultRoutes())
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO test.example.com")
		c.cmd("MAIL FROM:<me@ngm.dev>")
		if code, raw := c.cmd("DATA"); code < 500 {
			t.Errorf("got %d (%s), want >= 500", code, raw)
		}
	})

	t.Run("unknown verb", func(t *testing.T) {
		h := startServer(t, defaultRoutes())
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO test.example.com")
		if code, raw := c.cmd("FROBNICATE please"); code < 500 {
			t.Errorf("got %d (%s), want >= 500", code, raw)
		}
	})

	// RSET clears the sender, so a following RCPT must fail.
	t.Run("RCPT after RSET", func(t *testing.T) {
		h := startServer(t, defaultRoutes())
		c := dialClient(t, h.addr)
		c.greet()
		c.cmd("EHLO test.example.com")
		c.cmd("MAIL FROM:<me@ngm.dev>")
		c.cmd("RSET")
		if code, raw := c.cmd("RCPT TO:<you@ngm.dev>"); code < 500 {
			t.Errorf("got %d (%s), want >= 500", code, raw)
		}
	})
}

// --- envelope (smtp.test.ts:135-150) ---

func TestContract_NullSenderIsAccepted(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")

	if code, raw := c.cmd("MAIL FROM:<>"); code != 250 {
		t.Fatalf("MAIL FROM:<>: got %d (%s), want 250", code, raw)
	}
	if code, raw := c.cmd("RCPT TO:<you@ngm.dev>"); code != 250 {
		t.Errorf("RCPT after null sender: got %d (%s), want 250", code, raw)
	}
}

func TestContract_MultipleRecipientsAccepted(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")

	for _, rcpt := range []string{"a@ngm.dev", "b@ngm.dev"} {
		if code, raw := c.cmd("RCPT TO:<%s>", rcpt); code != 250 {
			t.Errorf("RCPT %s: got %d (%s)", rcpt, code, raw)
		}
	}
}

// --- delivery (smtp.test.ts:154-172) ---

func TestContract_QueuedReplyCarriesScrapeableUUID(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<you@ngm.dev>")
	if code, raw := c.cmd("DATA"); code != 354 {
		t.Fatalf("DATA: got %d (%s), want 354", code, raw)
	}

	code, raw := c.sendBody("Subject: test\r\n\r\nhello")
	if code != 250 {
		t.Fatalf("end of DATA: got %d (%s), want 250", code, raw)
	}
	if !regexp.MustCompile(`(?i)queued`).MatchString(raw) {
		t.Errorf("reply %q must match /queued/i", raw)
	}

	m := uuidScraper.FindStringSubmatch(raw)
	if m == nil {
		t.Fatalf("reply %q does not contain a scrapeable uuid", raw)
	}
	if m[1] == "" {
		t.Error("scraped uuid must not be empty")
	}
}

func TestContract_TwoMessagesOnOneConnectionGetDistinctReplies(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")

	var replies []string
	for i := 0; i < 2; i++ {
		c.cmd("MAIL FROM:<me@ngm.dev>")
		c.cmd("RCPT TO:<you@ngm.dev>")
		c.cmd("DATA")
		code, raw := c.sendBody(fmt.Sprintf("Subject: msg %d\r\n\r\nhello", i))
		if code != 250 {
			t.Fatalf("message %d: got %d (%s)", i, code, raw)
		}
		replies = append(replies, raw)
	}

	if replies[0] == replies[1] {
		t.Errorf("both messages returned the same reply %q; they must be distinct", replies[0])
	}
}

// --- the uuid hierarchy asserted by smtp.e2e.test.ts:46-48 ---

func TestContract_UUIDHierarchy(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<you@ngm.dev>")
	c.cmd("DATA")
	_, raw := c.sendBody("Subject: test\r\n\r\nhello")

	connUUID := uuidScraper.FindStringSubmatch(raw)[1]

	var envelopes []*queue.Envelope
	select {
	case envelopes = <-h.queued:
	case <-time.After(5 * time.Second):
		t.Fatal("message was never queued")
	}
	if len(envelopes) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envelopes))
	}

	e := envelopes[0]
	if e.ConnUUID != connUUID {
		t.Errorf("Connection.uuid: got %q, want the scraped %q", e.ConnUUID, connUUID)
	}
	if want := connUUID + ".1"; e.TxnUUID != want {
		t.Errorf("Transaction.uuid: got %q, want %q", e.TxnUUID, want)
	}
	if want := connUUID + ".1.1"; e.UUID != want {
		t.Errorf("Delivery.uuid: got %q, want %q", e.UUID, want)
	}

	// The e2e test finds all three rows with WHERE uuid LIKE '<conn>%'.
	for _, id := range []string{e.ConnUUID, e.TxnUUID, e.UUID} {
		if !strings.HasPrefix(id, connUUID) {
			t.Errorf("%q is not prefixed by the connection uuid %q", id, connUUID)
		}
	}
}

// A second message on the same connection is X.2, not a new connection uuid.
func TestContract_SecondTransactionIncrementsWithinTheConnection(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")

	var got []*queue.Envelope
	for i := 0; i < 2; i++ {
		c.cmd("MAIL FROM:<me@ngm.dev>")
		c.cmd("RCPT TO:<you@ngm.dev>")
		c.cmd("DATA")
		c.sendBody("Subject: t\r\n\r\nhello")
		select {
		case es := <-h.queued:
			got = append(got, es...)
		case <-time.After(5 * time.Second):
			t.Fatalf("message %d was never queued", i)
		}
	}

	if got[0].ConnUUID != got[1].ConnUUID {
		t.Error("both transactions must share one connection uuid")
	}
	if want := got[0].ConnUUID + ".1"; got[0].TxnUUID != want {
		t.Errorf("first txn: got %q, want %q", got[0].TxnUUID, want)
	}
	if want := got[0].ConnUUID + ".2"; got[1].TxnUUID != want {
		t.Errorf("second txn: got %q, want %q", got[1].TxnUUID, want)
	}
}

// --- per-recipient routing: the fix for npRoute.js's rcpt_to[0] ---

func TestContract_RecipientsSplitAcrossRelayGroups(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	// ngm.dev routes to Exchange; elsewhere.com falls through to Outbound.
	c.cmd("RCPT TO:<inside@ngm.dev>")
	c.cmd("RCPT TO:<outside@elsewhere.com>")
	c.cmd("DATA")
	if code, raw := c.sendBody("Subject: split\r\n\r\nhello"); code != 250 {
		t.Fatalf("got %d (%s)", code, raw)
	}

	var envelopes []*queue.Envelope
	select {
	case envelopes = <-h.queued:
	case <-time.After(5 * time.Second):
		t.Fatal("message was never queued")
	}

	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want 2 (Haraka would have produced 1, routed by rcpt_to[0])", len(envelopes))
	}

	byGroup := map[string][]string{}
	for _, e := range envelopes {
		byGroup[e.RelayGrp] = e.RcptAddrs()
	}
	if got := byGroup["Exchange"]; len(got) != 1 || got[0] != "inside@ngm.dev" {
		t.Errorf("Exchange envelope: got %v, want [inside@ngm.dev]", got)
	}
	if got := byGroup["Outbound"]; len(got) != 1 || got[0] != "outside@elsewhere.com" {
		t.Errorf("Outbound envelope: got %v, want [outside@elsewhere.com]", got)
	}

	// Both envelopes share the transaction and body, and get sibling uuids.
	if envelopes[0].TxnUUID != envelopes[1].TxnUUID {
		t.Error("split envelopes must share the transaction uuid")
	}
	if envelopes[0].Body != envelopes[1].Body {
		t.Error("split envelopes must share one spooled body")
	}
	ids := []string{envelopes[0].UUID, envelopes[1].UUID}
	txn := envelopes[0].TxnUUID
	for _, want := range []string{txn + ".1", txn + ".2"} {
		if ids[0] != want && ids[1] != want {
			t.Errorf("missing envelope uuid %q (got %v)", want, ids)
		}
	}
}

// No matching route is a temporary failure, matching npRoute.js:65's DENYSOFT.
func TestContract_UnroutableMailIsTempfailed(t *testing.T) {
	// A table with no catch-all.
	routes := &ruleset.LegacyTable{Rules: []ruleset.LegacyRule{
		{RouteName: "only ngm.dev", RcptDomain: "ngm.dev", Relay: "Exchange"},
	}}
	h := startServer(t, routes)
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<nobody@elsewhere.com>")
	c.cmd("DATA")

	code, raw := c.sendBody("Subject: t\r\n\r\nhello")
	if code < 400 || code >= 500 {
		t.Errorf("got %d (%s), want a 4xx tempfail", code, raw)
	}
	if !strings.Contains(raw, "No route found") {
		t.Errorf("reply %q should say \"No route found\" as npRoute.js:65 does", raw)
	}
}

// --- spooling ---

func TestContract_BodyIsSpooledWithAReceivedHeader(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO sender.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<you@ngm.dev>")
	c.cmd("DATA")
	c.sendBody("Subject: spooled\r\n\r\nthe body")

	var envelopes []*queue.Envelope
	select {
	case envelopes = <-h.queued:
	case <-time.After(5 * time.Second):
		t.Fatal("message was never queued")
	}

	raw, err := os.ReadFile(filepath.Join(h.spool.Root(), "data", envelopes[0].Body))
	if err != nil {
		t.Fatalf("read spooled body: %v", err)
	}
	body := string(raw)

	if !strings.HasPrefix(body, "Received: from sender.example.com") {
		t.Errorf("spooled body must start with our Received header, got:\n%s", body)
	}
	if !strings.Contains(body, "X-NGM-Gateway: go") {
		t.Error("spooled body should identify the producing gateway")
	}
	if !strings.Contains(body, envelopes[0].TxnUUID) {
		t.Error("Received header should carry the transaction id")
	}
	if !strings.Contains(body, "Subject: spooled") || !strings.Contains(body, "the body") {
		t.Errorf("original message was not preserved:\n%s", body)
	}
}

// A hostile EHLO name must not be able to inject headers.
func TestContract_HeaderInjectionViaHELOIsNeutralised(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	// go-smtp rejects most malformed domains outright; if it accepts one, our
	// sanitiser must still strip CR/LF.
	c.cmd("EHLO evil.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<you@ngm.dev>")
	c.cmd("DATA")
	c.sendBody("Subject: t\r\n\r\nhello")

	envelopes := <-h.queued
	raw, err := os.ReadFile(filepath.Join(h.spool.Root(), "data", envelopes[0].Body))
	if err != nil {
		t.Fatal(err)
	}
	if got := sanitizeHeaderValue("evil\r\nX-Injected: yes"); strings.Contains(got, "\r") || strings.Contains(got, "\n") {
		t.Errorf("sanitizeHeaderValue left a line break in %q", got)
	}
	if strings.Contains(string(raw), "X-Injected") {
		t.Error("header injection succeeded")
	}
}

// --- audit events ---

func TestContract_PostsConnectionAndQueueEvents(t *testing.T) {
	h := startServer(t, defaultRoutes())
	c := dialClient(t, h.addr)
	c.greet()
	c.cmd("EHLO test.example.com")
	c.cmd("MAIL FROM:<me@ngm.dev>")
	c.cmd("RCPT TO:<you@ngm.dev>")
	c.cmd("DATA")
	c.sendBody("Subject: t\r\n\r\nhello")
	<-h.queued

	kinds := h.events.kinds()
	var conn, queued int
	for _, k := range kinds {
		switch k {
		case events.KindConnection:
			conn++
		case events.KindQueue:
			queued++
		}
	}
	if conn != 1 {
		t.Errorf("connection events: got %d, want 1", conn)
	}
	if queued != 1 {
		t.Errorf("queue events: got %d, want 1", queued)
	}
}
