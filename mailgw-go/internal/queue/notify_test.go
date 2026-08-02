package queue

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/deliver"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// runWith enqueues env and runs one delivery attempt over it.
func runWith(t *testing.T, r *Runner, env *Envelope) {
	t.Helper()
	if err := r.spool.Enqueue(env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	names, _, _, err := r.spool.ReadyAndNext(time.Now())
	if err != nil {
		t.Fatalf("ReadyAndNext: %v", err)
	}
	claimed, inflight, err := r.spool.Claim(names[0])
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	r.attempt(context.Background(), claimed, inflight)
}

// bodyOf reads the message a queued envelope points at.
func bodyOf(t *testing.T, s *Spool, env *Envelope) string {
	t.Helper()
	raw, err := os.ReadFile(s.BodyPath(env.Body))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

// --- the NOTIFY defaults ---

// RFC 3461 §4.1 defines the defaults, and the DELAY one is a judgement call
// rather than a transcription — see WantsDelayDSN. Pinned here so the next
// reader does not "fix" it toward a strict reading that silences
// delay_warning_after for essentially all mail.
func TestNotify_Defaults(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		notify                  []string
		failure, delay, success bool
	}{
		{"absent", nil, true, true, false},
		{"NEVER", []string{NotifyNever}, false, false, false},
		{"FAILURE alone suppresses the delay warning", []string{NotifyFailure}, true, false, false},
		{"DELAY alone suppresses the failure report", []string{NotifyDelay}, false, true, false},
		{"FAILURE,DELAY", []string{NotifyFailure, NotifyDelay}, true, true, false},
		{"SUCCESS is never implied", []string{NotifyFailure}, true, false, false},
		{"SUCCESS asked for", []string{NotifySuccess, NotifyFailure}, true, false, true},
		// go-smtp upper-cases these, but the envelope is a persisted document
		// that an operator can edit.
		{"lower case", []string{"failure", "delay"}, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := Recipient{Addr: "a@b.com", Notify: tc.notify}
			if got := rc.WantsFailureDSN(); got != tc.failure {
				t.Errorf("WantsFailureDSN = %v, want %v", got, tc.failure)
			}
			if got := rc.WantsDelayDSN(); got != tc.delay {
				t.Errorf("WantsDelayDSN = %v, want %v", got, tc.delay)
			}
			if got := rc.WantsSuccessDSN(); got != tc.success {
				t.Errorf("WantsSuccessDSN = %v, want %v", got, tc.success)
			}
		})
	}
}

// --- suppression ---

func TestNotify_NeverSuppressesTheFailureReport(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	env := sample(r.spool, t, 1)
	env.Rcpts = []Recipient{{Addr: "one@partner.com", Status: StatusPending, Notify: []string{NotifyNever}}}
	runWith(t, r, env)

	if envs := queued(t, r.spool); len(envs) != 0 {
		t.Errorf("q/ holds %d notifications, want 0 — the sender asked never to be told",
			len(envs))
	}
	// Both counters, because "suppressed" and "broken" look identical from
	// outside and only one of them is fine.
	check(t, m, map[string]int64{"dsn_notify_suppressed": 1, "dsn_suppressed": 1, "dsn_generated": 0})
}

// This is what distinguishes NOTIFY from Suppress(): NOTIFY is per recipient,
// so one abstainer must not silence the report for everybody else.
func TestNotify_NeverForOneRecipientStillReportsTheOthers(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	env := sample(r.spool, t, 1)
	env.Rcpts = []Recipient{
		{Addr: "quiet@partner.com", Status: StatusPending, Notify: []string{NotifyNever}},
		{Addr: "one@partner.com", Status: StatusPending},
		{Addr: "two@partner.com", Status: StatusPending},
	}
	runWith(t, r, env)

	envs := queued(t, r.spool)
	if len(envs) != 1 {
		t.Fatalf("q/ holds %d notifications, want 1", len(envs))
	}
	body := bodyOf(t, r.spool, envs[0])
	if n := strings.Count(body, "Final-Recipient:"); n != 2 {
		t.Errorf("the report names %d recipients, want 2", n)
	}
	if strings.Contains(body, "quiet@partner.com") {
		t.Errorf("the report names a recipient that asked never to be told:\n%s", body)
	}
	check(t, m, map[string]int64{"dsn_notify_suppressed": 1, "dsn_generated": 1})
}

// dead/ is metadata-only and permanent, so a buried envelope is the only
// surviving record of what happened to a message. "The sender asked not to be
// told" and "we told them" must not read the same there.
func TestNotify_ExpiredRecipientIsStillExpiredButNotMarkedNotified(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.MaxLifetime = time.Hour
	// A connection-level failure, so the envelope defers rather than completing
	// — expiry is only reached from deferEnvelope, which is the one path every
	// non-completing attempt goes through.
	r.cfg.Deliver = failingDeliver

	env := sample(r.spool, t, 1)
	env.QueuedAt = time.Now().Add(-2 * time.Hour).UnixMilli()
	env.Rcpts = []Recipient{{Addr: "quiet@partner.com", Status: StatusPending, Notify: []string{NotifyNever}}}
	runWith(t, r, env)

	buried := dead(t, r.spool)
	if len(buried) != 1 {
		t.Fatalf("dead/ holds %d envelopes, want 1", len(buried))
	}
	rc := buried[0].Rcpts[0]
	if rc.Status != StatusExpired {
		t.Errorf("recipient status = %q, want %q — it expired either way", rc.Status, StatusExpired)
	}
	if rc.DSNSent {
		t.Error("dsn_sent is true for a recipient nobody was told about")
	}
	if envs := queued(t, r.spool); len(envs) != 0 {
		t.Errorf("q/ holds %d notifications, want 0", len(envs))
	}
	check(t, m, map[string]int64{"dsn_notify_suppressed": 1})
}

// --- ORCPT and ENVID ---

func TestNotify_ORCPTAndENVIDReachTheReport(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	env := sample(r.spool, t, 1)
	env.DSNEnvID = "sender-batch-42"
	env.Rcpts = []Recipient{{
		Addr:         "nobody@partner.com",
		Status:       StatusPending,
		OrigRcpt:     "sales+q3@partner.com",
		OrigRcptType: "RFC822",
	}}
	runWith(t, r, env)

	body := bodyOf(t, r.spool, queued(t, r.spool)[0])

	// xtext-encoded and lower-cased on the way out, which is the round trip
	// go-smtp's decode-on-the-way-in leaves half finished.
	if !strings.Contains(body, "Original-Recipient: rfc822; sales+2Bq3@partner.com") {
		t.Errorf("Original-Recipient missing or unencoded:\n%s", body)
	}
	if !strings.Contains(body, "Original-Envelope-Id: sender-batch-42") {
		t.Errorf("the sender's ENVID is not in the report:\n%s", body)
	}
}

// --- RET ---

func TestNotify_RETOverridesTheConfiguredReturn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configFull bool
		ret        string
		wantFull   bool
	}{
		{"FULL against a headers-only default", false, "FULL", true},
		{"HDRS against a full default", true, "HDRS", false},
		{"silence keeps the configured default", true, "", true},
		{"silence keeps the configured default (headers)", false, "", false},
		{"case is not significant", false, "full", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := bouncingRunner(t)
			r.cfg.Deliver = rejectingDeliver
			r.cfg.Bouncer.Full = tc.configFull

			env := sample(r.spool, t, 1)
			env.DSNRet = tc.ret
			runWith(t, r, env)

			body := bodyOf(t, r.spool, queued(t, r.spool)[0])
			gotFull := strings.Contains(body, "Content-Type: message/rfc822")
			if gotFull != tc.wantFull {
				t.Errorf("quoted the full message = %v, want %v:\n%s", gotFull, tc.wantFull, body)
			}
		})
	}
}

// A per-message parameter must not be a way to make one large message spool
// several copies of itself back at its sender. RFC 3461 §6.2 always permits
// headers only.
func TestNotify_MaxReturnBytesStillCapsRETFull(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver
	r.cfg.Bouncer.Full = false
	r.cfg.Bouncer.MaxReturnBytes = 1

	env := sample(r.spool, t, 1)
	env.DSNRet = "FULL"
	runWith(t, r, env)

	body := bodyOf(t, r.spool, queued(t, r.spool)[0])
	if strings.Contains(body, "Content-Type: message/rfc822") {
		t.Errorf("RET=FULL escaped dsn.max_return_bytes:\n%s", body)
	}
}

// The content type and the content have to agree. They were chosen by two
// different rules before RET existed — the caller asked QuotesFullMessage and
// Bounce labelled from dsn.return alone — so an over-cap message was returned
// as headers under a message/rfc822 part.
func TestNotify_QuotedPartIsLabelledAsWhatItActuallyContains(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver
	r.cfg.Bouncer.Full = true
	r.cfg.Bouncer.MaxReturnBytes = 1

	env := sample(r.spool, t, 1)
	runWith(t, r, env)

	body := bodyOf(t, r.spool, queued(t, r.spool)[0])
	if !strings.Contains(body, "Content-Type: text/rfc822-headers") {
		t.Errorf("an over-cap message was not labelled as headers-only:\n%s", body)
	}
	if strings.Contains(body, "Content-Type: message/rfc822") {
		t.Errorf("headers were labelled as a whole message:\n%s", body)
	}
}

// --- NOTIFY=SUCCESS ---

// deliveringDeliver accepts every recipient, which is what makes an envelope
// Done with nothing failed.
func deliveringDeliver(_ context.Context, relay relays.Relay, msg deliver.Message, _ deliver.Options) *deliver.Result {
	res := &deliver.Result{Host: relay.Exchange, Port: relay.Port.String(), Response: "250 ok"}
	for _, a := range msg.Rcpts {
		res.Rcpts = append(res.Rcpts, deliver.RcptResult{
			Addr: a, Outcome: deliver.OutcomeDelivered, Code: 250, Message: "2.0.0 accepted",
		})
	}
	return res
}

func TestNotify_SuccessProducesARelayedReport(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Deliver = deliveringDeliver

	env := sample(r.spool, t, 1)
	env.Rcpts = []Recipient{{
		Addr: "you@partner.com", Status: StatusPending,
		Notify: []string{NotifySuccess},
	}}
	runWith(t, r, env)

	envs := queued(t, r.spool)
	if len(envs) != 1 {
		t.Fatalf("q/ holds %d envelopes, want 1 (the relayed report)", len(envs))
	}
	body := bodyOf(t, r.spool, envs[0])

	if !strings.Contains(body, "Action: relayed") {
		t.Errorf("the report does not carry Action: relayed:\n%s", body)
	}
	// Not "delivered": a relay accepted the recipient and this gateway knows
	// nothing about what happened afterwards.
	if strings.Contains(body, "Action: delivered") {
		t.Error("the report claims final delivery")
	}
	if !strings.Contains(body, "Status: 2.0.0") {
		t.Errorf("the report does not carry a success status:\n%s", body)
	}
	// RFC 3461 §5.2.1: a success notification returns headers only, whatever
	// RET said.
	if strings.Contains(body, "Content-Type: message/rfc822") {
		t.Errorf("a success report quoted the whole message:\n%s", body)
	}
	check(t, m, map[string]int64{"dsn_generated": 1, "deliver_ok": 1})
}

func TestNotify_NoSuccessReportWithoutTheKeyword(t *testing.T) {
	r, m := bouncingRunner(t)
	r.cfg.Deliver = deliveringDeliver

	// The default is FAILURE only, so an ordinary delivery is silent. A success
	// notification nobody asked for is unsolicited mail.
	runWith(t, r, sample(r.spool, t, 1))

	if envs := queued(t, r.spool); len(envs) != 0 {
		t.Errorf("q/ holds %d envelopes, want 0", len(envs))
	}
	check(t, m, map[string]int64{"dsn_generated": 0, "deliver_ok": 1})
}

// Never answer a bounce, on the success path as much as the failure one.
func TestNotify_NoSuccessReportForANullSender(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = deliveringDeliver

	env := sample(r.spool, t, 1)
	env.MailFrom = ""
	env.IsDSN = true
	env.Rcpts = []Recipient{{
		Addr: "you@partner.com", Status: StatusPending, Notify: []string{NotifySuccess},
	}}
	runWith(t, r, env)

	if envs := queued(t, r.spool); len(envs) != 0 {
		t.Errorf("q/ holds %d envelopes, want 0 — a bounce is never answered", len(envs))
	}
}

// A message with one delivered and one rejected recipient produces two reports,
// each naming only the recipients that asked for it.
func TestNotify_MixedOutcomesReportSeparately(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = func(ctx context.Context, relay relays.Relay, msg deliver.Message, o deliver.Options) *deliver.Result {
		res := &deliver.Result{Host: relay.Exchange, Port: relay.Port.String()}
		for _, a := range msg.Rcpts {
			rr := deliver.RcptResult{Addr: a, Outcome: deliver.OutcomeDelivered, Code: 250}
			if strings.HasPrefix(a, "bad@") {
				rr = deliver.RcptResult{Addr: a, Outcome: deliver.OutcomeRejected, Code: 550, Message: "5.1.1 no"}
			}
			res.Rcpts = append(res.Rcpts, rr)
		}
		return res
	}

	env := sample(r.spool, t, 1)
	env.Rcpts = []Recipient{
		{Addr: "good@partner.com", Status: StatusPending, Notify: []string{NotifySuccess, NotifyFailure}},
		{Addr: "bad@partner.com", Status: StatusPending, Notify: []string{NotifySuccess, NotifyFailure}},
	}
	runWith(t, r, env)

	envs := queued(t, r.spool)
	if len(envs) != 2 {
		t.Fatalf("q/ holds %d reports, want 2 (one failure, one relayed)", len(envs))
	}

	var sawFailed, sawRelayed bool
	for _, e := range envs {
		body := bodyOf(t, r.spool, e)
		switch {
		case strings.Contains(body, "Action: failed"):
			sawFailed = true
			if !strings.Contains(body, "bad@partner.com") || strings.Contains(body, "good@partner.com") {
				t.Errorf("the failure report names the wrong recipients:\n%s", body)
			}
		case strings.Contains(body, "Action: relayed"):
			sawRelayed = true
			if !strings.Contains(body, "good@partner.com") || strings.Contains(body, "bad@partner.com") {
				t.Errorf("the relayed report names the wrong recipients:\n%s", body)
			}
		}
	}
	if !sawFailed || !sawRelayed {
		t.Errorf("saw failure=%v relayed=%v, want both", sawFailed, sawRelayed)
	}
}

// --- on-disk compatibility ---

// An envelope written before any of these fields existed must load and behave
// exactly as it did — the spool's standing rule, stated on Envelope.DSNSeq:
// bumping EnvelopeVersion makes validate reject every envelope already on disk,
// and Claim answers that by moving them to dead/.
func TestNotify_EnvelopeFromAnOlderBuildStillLoads(t *testing.T) {
	r, _ := bouncingRunner(t)
	r.cfg.Deliver = rejectingDeliver

	env := sample(r.spool, t, 1)
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"notify", "orig_rcpt", "orig_rcpt_type", "dsn_ret", "dsn_envid"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Errorf("an envelope with no DSN parameters still serialises %q; "+
				"an unchanged spool must not change shape", field)
		}
	}

	var back Envelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := back.validate(); err != nil {
		t.Fatalf("validate rejected an envelope from an older build: %v", err)
	}
	// The zero value is the RFC 3461 default, which is what makes the upgrade
	// free: an old envelope keeps bouncing exactly as it always did.
	if !back.Rcpts[0].WantsFailureDSN() || !back.Rcpts[0].WantsDelayDSN() {
		t.Error("an envelope with no NOTIFY does not get the RFC 3461 defaults")
	}

	runWith(t, r, &back)
	if envs := queued(t, r.spool); len(envs) != 1 {
		t.Errorf("q/ holds %d notifications, want 1", len(envs))
	}
}

// dead reads every envelope buried in dead/.
func dead(t *testing.T, s *Spool) []*Envelope {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.Root(), dirDead))
	if err != nil {
		t.Fatalf("ReadDir dead/: %v", err)
	}
	out := make([]*Envelope, 0, len(ents))
	for _, e := range ents {
		env, err := readEnvelope(filepath.Join(s.Root(), dirDead, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out = append(out, env)
	}
	return out
}

var _ = obs.Discard
