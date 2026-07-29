package deliver

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// A real go-smtp server stands in for the relay, so these tests exercise the
// wire protocol rather than a hand-written mock of it.

type fakeBackend struct {
	mu       sync.Mutex
	received []fakeMessage
	// rcptErr, if set, is returned for recipients it matches.
	rcptErr func(addr string) error
	dataErr error
}

type fakeMessage struct {
	From  string
	Rcpts []string
	Body  string
}

func (b *fakeBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &fakeSession{b: b}, nil
}

func (b *fakeBackend) messages() []fakeMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]fakeMessage(nil), b.received...)
}

type fakeSession struct {
	b     *fakeBackend
	from  string
	rcpts []string
}

func (s *fakeSession) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *fakeSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.b.rcptErr != nil {
		if err := s.b.rcptErr(to); err != nil {
			return err
		}
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *fakeSession) Data(r io.Reader) error {
	if s.b.dataErr != nil {
		return s.b.dataErr
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.b.mu.Lock()
	s.b.received = append(s.b.received, fakeMessage{From: s.from, Rcpts: s.rcpts, Body: string(body)})
	s.b.mu.Unlock()
	return nil
}

func (s *fakeSession) Reset()        { s.from = ""; s.rcpts = nil }
func (s *fakeSession) Logout() error { return nil }

// startRelay runs a fake relay and returns its Relay config.
func startRelay(t *testing.T, b *fakeBackend, name string) relays.Relay {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := smtp.NewServer(b)
	srv.Domain = "relay.test"
	srv.MaxMessageBytes = 1 << 20
	srv.AllowInsecureAuth = true

	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	host, port, _ := net.SplitHostPort(l.Addr().String())
	p, _ := net.LookupPort("tcp", port)
	return relays.Relay{
		Name:     name,
		Exchange: host,
		Port:     relays.Port(p),
		// The fake relay speaks plaintext; opportunistic TLS would just add a
		// wasted connection to every test.
		TLS: relays.TLSNone,
	}
}

func msg(from string, rcpts ...string) Message {
	return Message{
		From:  from,
		Rcpts: rcpts,
		Body:  strings.NewReader("Subject: test\r\n\r\nhello\r\n"),
	}
}

func TestDeliver_HappyPath(t *testing.T) {
	b := &fakeBackend{}
	relay := startRelay(t, b, "r1")

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{LocalName: "devbook.local"})

	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := res.Accepted(); len(got) != 1 || got[0] != "you@ngm.dev" {
		t.Errorf("accepted: got %v", got)
	}
	if res.Host != relay.Exchange {
		t.Errorf("host: got %q, want %q", res.Host, relay.Exchange)
	}
	if res.IP == "" {
		t.Error("the peer IP must be recorded for the audit event")
	}
	if res.Port != relay.Port.String() {
		t.Errorf("port: got %q, want %q", res.Port, relay.Port.String())
	}
	if res.Response == "" {
		t.Error("the relay's end-of-DATA reply must be captured")
	}
	if res.Delay <= 0 {
		t.Error("delay should be measured")
	}

	got := b.messages()
	if len(got) != 1 {
		t.Fatalf("relay received %d messages, want 1", len(got))
	}
	if got[0].From != "me@ngm.dev" {
		t.Errorf("from: got %q", got[0].From)
	}
	if !strings.Contains(got[0].Body, "Subject: test") {
		t.Errorf("body not delivered intact: %q", got[0].Body)
	}
}

func TestDeliver_NullSender(t *testing.T) {
	b := &fakeBackend{}
	relay := startRelay(t, b, "r1")

	res := Deliver(context.Background(), relay, msg("", "you@ngm.dev"), Options{})
	if res.Err != nil {
		t.Fatalf("a bounce must be deliverable: %v", res.Err)
	}
	if got := b.messages(); len(got) != 1 || got[0].From != "" {
		t.Errorf("relay should have seen an empty sender, got %+v", got)
	}
}

// A 5xx on one recipient must not affect the others, and must be classified as
// permanent so the message bounces rather than looping.
func TestDeliver_PerRecipientClassification(t *testing.T) {
	b := &fakeBackend{
		rcptErr: func(addr string) error {
			switch addr {
			case "gone@ngm.dev":
				return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user"}
			case "busy@ngm.dev":
				return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Try later"}
			}
			return nil
		},
	}
	relay := startRelay(t, b, "r1")

	res := Deliver(context.Background(), relay,
		msg("me@ngm.dev", "ok@ngm.dev", "gone@ngm.dev", "busy@ngm.dev"), Options{})
	if res.Err != nil {
		t.Fatalf("unexpected connection-level error: %v", res.Err)
	}

	byAddr := map[string]RcptResult{}
	for _, rr := range res.Rcpts {
		byAddr[rr.Addr] = rr
	}

	if got := byAddr["ok@ngm.dev"]; got.Outcome != OutcomeDelivered {
		t.Errorf("ok@: got outcome %v, want delivered", got.Outcome)
	}
	if got := byAddr["gone@ngm.dev"]; got.Outcome != OutcomeRejected || got.Code != 550 {
		t.Errorf("gone@: got %+v, want rejected/550", got)
	}
	if got := byAddr["busy@ngm.dev"]; got.Outcome != OutcomeDeferred || got.Code != 451 {
		t.Errorf("busy@: got %+v, want deferred/451", got)
	}

	// Only the accepted recipient should have reached the relay.
	got := b.messages()
	if len(got) != 1 || len(got[0].Rcpts) != 1 || got[0].Rcpts[0] != "ok@ngm.dev" {
		t.Errorf("relay recipients: got %+v", got)
	}
}

// Every recipient rejected means no DATA is sent at all.
func TestDeliver_AllRecipientsRejected(t *testing.T) {
	b := &fakeBackend{
		rcptErr: func(string) error {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No"}
		},
	}
	relay := startRelay(t, b, "r1")

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "a@ngm.dev", "b@ngm.dev"), Options{})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := res.Accepted(); len(got) != 0 {
		t.Errorf("accepted: got %v, want none", got)
	}
	if len(res.Rcpts) != 2 {
		t.Errorf("every recipient needs an outcome, got %d", len(res.Rcpts))
	}
	if got := b.messages(); len(got) != 0 {
		t.Errorf("no message should have been transmitted, got %d", len(got))
	}
}

// A DATA-stage failure applies to every recipient the relay had accepted, so
// none of them are silently lost.
func TestDeliver_DataFailureAppliesToAllAcceptedRecipients(t *testing.T) {
	b := &fakeBackend{
		dataErr: &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 3, 1}, Message: "Out of space"},
	}
	relay := startRelay(t, b, "r1")

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "a@ngm.dev", "b@ngm.dev"), Options{})
	if len(res.Rcpts) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(res.Rcpts))
	}
	for _, rr := range res.Rcpts {
		if rr.Outcome != OutcomeDeferred {
			t.Errorf("%s: got %v, want deferred", rr.Addr, rr.Outcome)
		}
		if rr.Code != 452 {
			t.Errorf("%s: got code %d, want 452", rr.Addr, rr.Code)
		}
	}
}

// An unreachable relay is a connection-level failure, which tells the caller to
// try the next relay in the group rather than to bounce.
func TestDeliver_UnreachableRelayIsConnectionLevel(t *testing.T) {
	relay := relays.Relay{Name: "dead", Exchange: "127.0.0.1", Port: 1, TLS: relays.TLSNone}

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{ConnectTimeout: 2 * time.Second})
	if res.Err == nil {
		t.Fatal("expected a connection-level error")
	}
	if len(res.Rcpts) != 0 {
		t.Errorf("no per-recipient outcome should be recorded, got %+v", res.Rcpts)
	}
	// The audit event still needs to name the relay that was tried.
	if res.Host != "127.0.0.1" || res.Port != "1" {
		t.Errorf("relay identity not recorded: host=%q port=%q", res.Host, res.Port)
	}
}

// tls=required must refuse a relay that cannot encrypt, rather than silently
// sending in the clear.
func TestDeliver_TLSRequiredRefusesPlaintextRelay(t *testing.T) {
	b := &fakeBackend{}
	relay := startRelay(t, b, "r1")
	relay.TLS = relays.TLSRequired

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), Options{})
	if res.Err == nil {
		t.Fatal("expected an error when TLS is required but unavailable")
	}
	if !strings.Contains(res.Err.Error(), "TLS") {
		t.Errorf("error should mention TLS, got %v", res.Err)
	}
	if got := b.messages(); len(got) != 0 {
		t.Error("nothing may be transmitted when the TLS policy cannot be met")
	}
	if !res.TLSForced {
		t.Error("TLSForced should be recorded for the audit event")
	}
}

// Credentials must not be sent over an unencrypted link unless explicitly
// permitted.
func TestDeliver_RefusesAuthOverPlaintext(t *testing.T) {
	b := &fakeBackend{}
	relay := startRelay(t, b, "r1")
	relay.AuthUser = "user"
	relay.AuthPass = "hunter2"

	res := Deliver(context.Background(), relay, msg("me@ngm.dev", "you@ngm.dev"), Options{})
	if res.Err == nil {
		t.Fatal("expected a refusal to AUTH in the clear")
	}
	if strings.Contains(res.Err.Error(), "hunter2") {
		t.Errorf("the password leaked into the error: %v", res.Err)
	}
	if res.Auth {
		t.Error("Auth must not be reported when authentication did not happen")
	}
}

func TestDeliver_ContextCancellationStopsDial(t *testing.T) {
	relay := relays.Relay{Name: "slow", Exchange: "203.0.113.1", Port: 25, TLS: relays.TLSNone}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := Deliver(ctx, relay, msg("me@ngm.dev", "you@ngm.dev"),
		Options{ConnectTimeout: 10 * time.Second})
	if res.Err == nil {
		t.Fatal("a cancelled context must abort the attempt")
	}
}
