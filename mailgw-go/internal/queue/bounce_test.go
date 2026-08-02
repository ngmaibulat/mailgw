package queue

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/dsn"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// bouncingRunner is a runner whose relay group permanently rejects every
// recipient, with notifications enabled and routed to a named group.
func bouncingRunner(t *testing.T) (*Runner, *obs.Metrics) {
	t.Helper()
	r, m := metricsRunner(t, 1)
	r.cfg.Bouncer = &Bouncer{
		Spool:      r.spool,
		Enabled:    true,
		Postmaster: "postmaster@gw.ngm.dev",
		Hostname:   "gw.ngm.dev",
		RelayGroup: "Outbound",
		Metrics:    m,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return r, m
}

// rejectingDeliver connects fine and then refuses every recipient 5xx — the
// shape that makes an envelope Done with nothing delivered.
func rejectingDeliver(_ context.Context, relay relays.Relay, msg deliver.Message, _ deliver.Options) *deliver.Result {
	res := &deliver.Result{Host: relay.Exchange, Port: relay.Port.String(), Response: "550 rejected"}
	for _, a := range msg.Rcpts {
		res.Rcpts = append(res.Rcpts, deliver.RcptResult{
			Addr:    a,
			Outcome: deliver.OutcomeRejected,
			Code:    550,
			Message: "5.1.1 no such user",
		})
	}
	return res
}

// queued reads every envelope currently in q/.
func queued(t *testing.T, s *Spool) []*Envelope {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.Root(), dirReady))
	if err != nil {
		t.Fatalf("ReadDir q/: %v", err)
	}
	out := make([]*Envelope, 0, len(ents))
	for _, e := range ents {
		env, err := readEnvelope(filepath.Join(s.Root(), dirReady, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out = append(out, env)
	}
	return out
}

func TestBounce_PermanentRejectionNotifiesTheSender(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	attemptOnce(t, r)

	envs := queued(t, r.spool)
	if len(envs) != 1 {
		t.Fatalf("q/ holds %d envelopes, want 1 (the notification)", len(envs))
	}
	b := envs[0]

	if !b.IsDSN {
		t.Error("the notification is not marked as a DSN; a failure to deliver it would bounce again")
	}
	// RFC 5321 §4.5.5 — a notification carries a null return path.
	if b.MailFrom != "" {
		t.Errorf("notification MailFrom = %q, want a null sender", b.MailFrom)
	}
	if len(b.Rcpts) != 1 || b.Rcpts[0].Addr != "me@ngm.dev" {
		t.Errorf("notification recipients = %v, want the original sender", b.RcptAddrs())
	}
	check(t, m, map[string]int64{"dsn_generated": 1, "deliver_bounced": 1})
}

// The identity is a literal prefix extension of the delivery that failed, which
// is what keeps `WHERE uuid LIKE 'X%'` finding the whole tree in the log tables.
func TestBounce_IdentityNestsUnderTheFailedDelivery(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	original := sample(r.spool, t, 1)
	if err := r.spool.Enqueue(original); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, _ := r.spool.ReadyAndNext(time.Now())
	env, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), env, inflight)

	b := queued(t, r.spool)[0]
	if want := original.UUID + ".1"; b.UUID != want {
		t.Errorf("notification uuid = %q, want %q", b.UUID, want)
	}
	if b.TxnUUID != original.UUID {
		t.Errorf("notification txn_uuid = %q, want the failed envelope %q", b.TxnUUID, original.UUID)
	}
	if b.ConnUUID != original.ConnUUID {
		t.Errorf("notification conn_uuid = %q, want %q", b.ConnUUID, original.ConnUUID)
	}
	if !strings.HasPrefix(b.UUID, original.ConnUUID) {
		t.Errorf("notification uuid %q does not extend the connection root %q",
			b.UUID, original.ConnUUID)
	}
}

// Never bounce a bounce. A failed notification is buried, not answered with
// another one — that is how two mail systems bounce at each other forever.
func TestBounce_NotGeneratedForAMessageWithNoReturnPath(t *testing.T) {
	for name, mangle := range map[string]func(*Envelope){
		"already a notification": func(e *Envelope) { e.IsDSN = true },
		"null sender":            func(e *Envelope) { e.MailFrom = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r, m := bouncingRunner(t)
			r.cfg.Deliver = rejectingDeliver

			env := sample(r.spool, t, 1)
			mangle(env)
			if err := r.spool.Enqueue(env); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			names, _, _, _ := r.spool.ReadyAndNext(time.Now())
			claimed, inflight, err := r.spool.Claim(names[0])
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			r.attempt(context.Background(), claimed, inflight)

			if envs := queued(t, r.spool); len(envs) != 0 {
				t.Errorf("q/ holds %d envelopes, want 0 — a bounce was bounced", len(envs))
			}
			check(t, m, map[string]int64{"dsn_generated": 0, "dsn_suppressed": 1})
		})
	}
}

// With no route rule and no dsn.relay_group there is nowhere to send a bounce.
// Guessing — the failed envelope's own group is the tempting guess — sends it to
// the recipient's smarthost, which answers "relay denied" and buries it.
func TestBounce_UnroutableIsCountedNotGuessed(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Bouncer.RelayGroup = ""
	r.cfg.Deliver = rejectingDeliver

	attemptOnce(t, r)

	if envs := queued(t, r.spool); len(envs) != 0 {
		t.Errorf("q/ holds %d envelopes, want 0", len(envs))
	}
	check(t, m, map[string]int64{"dsn_generated": 0, "dsn_unroutable": 1})
}

// A route rule wins over the configured fallback, so bounces obey the same
// declarative configuration as everything else.
func TestBounce_RouteRuleWinsOverTheConfiguredFallback(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	var askedFor string
	r.cfg.Bouncer.Route = func(to string) (string, string, bool) {
		askedFor = to
		return "Outbound", "bounces-go-here", true
	}

	attemptOnce(t, r)

	if askedFor != "me@ngm.dev" {
		t.Errorf("the router was asked about %q, want the original sender", askedFor)
	}
	if envs := queued(t, r.spool); len(envs) != 1 {
		t.Fatalf("q/ holds %d envelopes, want 1", len(envs))
	}
}

// A router that declines falls through to the configured group rather than
// dropping the notification.
func TestBounce_FallsBackWhenNoRuleClaimsIt(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver
	r.cfg.Bouncer.Route = func(string) (string, string, bool) { return "", "", false }

	attemptOnce(t, r)

	envs := queued(t, r.spool)
	if len(envs) != 1 {
		t.Fatalf("q/ holds %d envelopes, want 1", len(envs))
	}
	if envs[0].RelayGrp != "Outbound" {
		t.Errorf("notification relay group = %q, want the configured fallback", envs[0].RelayGrp)
	}
	check(t, m, map[string]int64{"dsn_unroutable": 0})
}

// One report naming every failed recipient, not one report each: the
// per-recipient section of RFC 3464 exists precisely so this is possible.
func TestBounce_OneReportCoversEveryRejectedRecipient(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	env := sample(r.spool, t, 1)
	env.Rcpts = []Recipient{
		{Addr: "one@partner.com", Status: StatusPending},
		{Addr: "two@partner.com", Status: StatusPending},
		{Addr: "three@partner.com", Status: StatusPending},
	}
	if err := r.spool.Enqueue(env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, _ := r.spool.ReadyAndNext(time.Now())
	claimed, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), claimed, inflight)

	envs := queued(t, r.spool)
	if len(envs) != 1 {
		t.Fatalf("q/ holds %d notifications, want exactly 1", len(envs))
	}
	body, err := os.ReadFile(r.spool.BodyPath(envs[0].Body))
	if err != nil {
		t.Fatalf("read notification body: %v", err)
	}
	if n := strings.Count(string(body), "Final-Recipient:"); n != 3 {
		t.Errorf("the report names %d recipients, want 3", n)
	}
	check(t, m, map[string]int64{"dsn_generated": 1})
}

func TestBounce_ExpiredEnvelopeNotifiesBeforeItIsBuried(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.MaxLifetime = time.Hour
	r.cfg.Deliver = failingDeliver

	attemptAged(t, r, 2*time.Hour)

	envs := queued(t, r.spool)
	if len(envs) != 1 {
		t.Fatalf("q/ holds %d envelopes, want 1 (the notification)", len(envs))
	}
	body, err := os.ReadFile(r.spool.BodyPath(envs[0].Body))
	if err != nil {
		t.Fatalf("read notification body: %v", err)
	}
	// 5.4.7 is "message expired in the queue" — the reason matters to a sender
	// deciding whether to resend. The 5 rather than 4 is the point: the report
	// says `failed` and will not be retried, so a 4.x would tell the sender's
	// software the condition is transient.
	if !strings.Contains(string(body), "Status: 5.4.7") {
		t.Errorf("expiry notification does not carry the expiry status:\n%s", body)
	}
	if !strings.Contains(string(body), "Action: failed") {
		t.Errorf("expiry notification is not marked as a failure:\n%s", body)
	}
	check(t, m, map[string]int64{"env_expired": 1, "dsn_generated": 1})
}

func TestBounce_DelayWarningIsSentOnceAndIsNotAFailure(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.MaxLifetime = 96 * time.Hour
	r.cfg.DelayWarnAfter = time.Hour
	r.cfg.Deliver = failingDeliver
	r.cfg.Backoff = func(int) time.Duration { return 0 }

	attemptAged(t, r, 2*time.Hour)

	// The original was requeued; the notification is the second envelope.
	envs := queued(t, r.spool)
	if len(envs) != 2 {
		t.Fatalf("q/ holds %d envelopes, want 2 (the message and one warning)", len(envs))
	}

	var warning, original *Envelope
	for _, e := range envs {
		if e.IsDSN {
			warning = e
		} else {
			original = e
		}
	}
	if warning == nil || original == nil {
		t.Fatalf("expected one notification and one requeued message, got %v", envs)
	}
	if !original.DelayWarned {
		t.Error("the requeued message is not marked as warned; it would warn again every retry")
	}

	body, err := os.ReadFile(r.spool.BodyPath(warning.Body))
	if err != nil {
		t.Fatalf("read warning body: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "Action: delayed") {
		t.Error("the delay warning is not marked as a delay")
	}
	if strings.Contains(s, "Action: failed") {
		t.Error("the delay warning claims the message failed; it is still being retried")
	}

	// A second attempt on the requeued envelope must not warn again.
	names, _, _, _ := r.spool.ReadyAndNext(time.Now())
	for _, n := range names {
		claimed, inflight, err := r.spool.Claim(n)
		if err != nil {
			continue
		}
		if claimed.IsDSN {
			_ = r.spool.Reschedule(inflight, claimed, time.Now().Add(time.Hour))
			continue
		}
		r.attempt(context.Background(), claimed, inflight)
	}
	check(t, m, map[string]int64{"dsn_generated": 1})
}

// The notification quotes the message, so it must be built while the body is
// still there — Complete and Bury both collect it.
func TestBounce_QuotesTheOriginalMessage(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	attemptOnce(t, r)

	envs := queued(t, r.spool)
	if len(envs) != 1 {
		t.Fatalf("q/ holds %d envelopes, want 1", len(envs))
	}
	body, err := os.ReadFile(r.spool.BodyPath(envs[0].Body))
	if err != nil {
		t.Fatalf("read notification body: %v", err)
	}
	if !strings.Contains(string(body), "Subject: hi") {
		t.Errorf("the notification does not quote the original headers:\n%s", body)
	}
}

func TestBounce_DisabledMeansNoNotification(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Bouncer.Enabled = false
	r.cfg.Deliver = rejectingDeliver

	attemptOnce(t, r)

	if envs := queued(t, r.spool); len(envs) != 0 {
		t.Errorf("q/ holds %d envelopes, want 0", len(envs))
	}
	check(t, m, map[string]int64{"dsn_generated": 0})
}

// A second notification from the same envelope must not collide with the first.
// Both would be numbered .1 without DSNSeq, giving them one queue filename and
// silently losing the earlier one.
func TestBounce_SecondNotificationGetsItsOwnIdentity(t *testing.T) {
	s := newSpool(t)
	b := &Bouncer{
		Spool: s, Enabled: true,
		Postmaster: "postmaster@gw.ngm.dev", Hostname: "gw.ngm.dev",
		RelayGroup: "Outbound",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env := sample(s, t, 1)

	for range 2 {
		sent, err := b.Bounce(Request{
			Env:   env,
			Rcpts: []dsn.Rcpt{{Addr: "nobody@partner.com", Status: "5.1.1"}},
		})
		if err != nil || !sent {
			t.Fatalf("Bounce: sent=%v err=%v", sent, err)
		}
	}

	envs := queued(t, s)
	if len(envs) != 2 {
		t.Fatalf("q/ holds %d notifications, want 2 — one overwrote the other", len(envs))
	}
	if envs[0].UUID == envs[1].UUID {
		t.Errorf("both notifications share the uuid %q", envs[0].UUID)
	}
}

func TestHeaderBlock_StopsAtTheBlankLine(t *testing.T) {
	got := HeaderBlock(strings.NewReader("Subject: hi\r\nFrom: a@b\r\n\r\nthe body\r\n"))
	if strings.Contains(got, "the body") {
		t.Errorf("HeaderBlock returned part of the body: %q", got)
	}
	if !strings.Contains(got, "Subject: hi") || !strings.Contains(got, "From: a@b") {
		t.Errorf("HeaderBlock dropped a header: %q", got)
	}
}

// A message with no blank line at all is malformed; reading all of it into
// memory to discover that is not the right response.
func TestHeaderBlock_TerminatesOnAMessageWithNoBody(t *testing.T) {
	got := HeaderBlock(strings.NewReader("Subject: no blank line follows"))
	if !strings.Contains(got, "Subject:") {
		t.Errorf("HeaderBlock = %q", got)
	}
}
