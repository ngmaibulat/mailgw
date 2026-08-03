// Package obs is the gateway's counter registry.
//
// Deliberately not a metrics library. The Prometheus text format for a counter
// is three lines; pulling in prometheus/client_golang for that would more than
// double this module's dependency graph, which has four direct entries today.
// Revisit only if histograms are wanted.
//
// # Three units, and why they are not interchangeable
//
// Mixing them is how a dashboard silently goes wrong, so every counter name
// below says which one it is and every HELP string repeats it:
//
//   - message   — one SMTP transaction (MAIL through end of DATA)
//   - recipient — one RCPT
//   - envelope  — one file the spool holds
//
// One message can hold several recipients and become several envelopes, and a
// single envelope is retried many times. Adding a per-message counter to a
// per-recipient one produces a number that means nothing.
package obs

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"
)

// Metrics is the process-wide counter set. Every field is atomic and safe to
// increment from any goroutine on the SMTP or delivery path.
//
// The fields are grouped by unit. A new field must also be added to the
// counters table below — TestCounters_TableCoversEveryField enforces it.
type Metrics struct {
	// Connections, counted in the allowlist listener — the only place that
	// sees a peer before the banner.
	ConnAccepted atomic.Int64
	ConnDenied   atomic.Int64
	// ProxyDropped counts connections closed by the PROXY-protocol wrapper,
	// before the allowlist ever saw them. Always 0 unless a listener sets
	// proxy_protocol.
	ProxyDropped atomic.Int64
	// ConnThrottled counts connections refused 421 for exceeding
	// max.connections. A SUBSET of ConnAccepted, not a sibling: the cap sits
	// outside the allowlist, so the peer had already been allowed.
	ConnThrottled atomic.Int64

	// Rate-limit refusals, one counter per dimension — a limiter nobody can see
	// is a limiter nobody can tune, and one counter for all five would not say
	// which limit to raise.
	//
	// THREE units, and they are not interchangeable. RateConn is per CONNECTION
	// (and, like ConnThrottled, a subset of ConnAccepted — this limiter sits
	// inside the allowlist). RateSender and RateUser are per MESSAGE, refused at
	// MAIL FROM. RateRcptDomain is per RECIPIENT, so one message to fifty
	// blocked addresses contributes fifty. RateAuth is per AUTH COMMAND, like
	// AuthFailed beside it.
	//
	// Every one of these is a 4xx. Nothing in the rate limiter answers 5xx.
	RateConn       atomic.Int64
	RateSender     atomic.Int64
	RateUser       atomic.Int64
	RateRcptDomain atomic.Int64
	RateAuth       atomic.Int64

	// Inbound authentication, per AUTH COMMAND — not per connection and not per
	// message. A client that fails twice and then succeeds contributes two and
	// one; a client that authenticates once and sends fifty messages
	// contributes one.
	AuthOK     atomic.Int64
	AuthFailed atomic.Int64

	// Message authentication. Two units here, and mixing them would mislead:
	// SPFChecked and DKIMVerified are per INBOUND MESSAGE, while DKIMSigned and
	// DKIMSignFailed are per OUTBOUND ENVELOPE-ATTEMPT — one inbound message
	// split across three relay groups and retried twice contributes six.
	//
	// SPFFailed is a SUBSET of SPFChecked, not a sibling.
	//
	// There is deliberately no DMARC counter: a DMARC result is a function of
	// the SPF and DKIM results already here, and a counter should name something
	// that moves rather than something derived from two things that do.
	SPFChecked     atomic.Int64
	SPFFailed      atomic.Int64
	DKIMVerified   atomic.Int64
	DKIMSigned     atomic.Int64
	DKIMSignFailed atomic.Int64

	// Messages. Note MsgAccepted is a SUPERSET of MsgDiscarded and
	// MsgQuarantined, not a disjoint bucket: a message every rule dropped is
	// still answered 250.
	MsgAccepted    atomic.Int64
	MsgRejected    atomic.Int64
	MsgTempfailed  atomic.Int64
	MsgDiscarded   atomic.Int64
	MsgQuarantined atomic.Int64
	// MsgLoopRejected is a SUBSET of MsgRejected: the hop limit refuses through
	// the same path every other message-scoped refusal takes, and this names why.
	MsgLoopRejected atomic.Int64

	// Recipients. A discarded or quarantined recipient is also counted as
	// accepted, because that is what the client was told at RCPT TO.
	RcptAccepted   atomic.Int64
	RcptRejected   atomic.Int64
	RcptTempfailed atomic.Int64
	RcptDiscarded  atomic.Int64

	BytesIn atomic.Int64

	// Envelopes, counted where the spool file is actually created — or, for the
	// last two, where it changes directory.
	EnvelopesQueued      atomic.Int64
	EnvelopesQuarantined atomic.Int64
	EnvelopesExpired     atomic.Int64
	EnvelopesReleased    atomic.Int64

	// Delivery. The units differ within this group and the difference is real
	// — see each HELP string.
	DeliverAttempts   atomic.Int64
	DeliverOK         atomic.Int64
	DeliverDeferred   atomic.Int64
	DeliverBounced    atomic.Int64
	DeliverConnFail   atomic.Int64
	DeliverConnReused atomic.Int64
	// DeliverPoolFull counts finished connections closed rather than kept,
	// because outbound.max_pooled_connections was already reached.
	DeliverPoolFull atomic.Int64
	// DeliverTLSDowngraded counts opportunistic relay attempts that failed to
	// upgrade and were carried in the clear.
	DeliverTLSDowngraded atomic.Int64

	// Bounces this gateway generated. Per DSN MESSAGE, not per recipient: one
	// report can name several, which is the whole reason RFC 3464 has a
	// per-recipient section.
	DSNGenerated  atomic.Int64
	DSNSuppressed atomic.Int64
	DSNUnroutable atomic.Int64
	// DSNNotifySuppressed breaks the unit of this group: it is per RECIPIENT,
	// because RFC 3461 NOTIFY is a per-recipient parameter and one report can
	// name several recipients of which only some declined.
	DSNNotifySuppressed atomic.Int64

	// Audit events, per EVENT — one row bound for logservice, so a single
	// message contributes a connection, a queue and one delivery per recipient.
	EventsSpilled      atomic.Int64
	EventsReplayed     atomic.Int64
	EventsReplayFailed atomic.Int64
	// EventsDropped is the one audit-trail loss that is not recoverable: the
	// in-memory buffer was full, so the event was never written anywhere.
	EventsDropped atomic.Int64
}

// New returns an empty registry.
func New() *Metrics { return &Metrics{} }

// Discard absorbs counts from a component built without a registry — a test
// that constructs a Backend or a RunnerConfig by literal, most often.
//
// It exists so the mail path never has to nil-check and a forgotten field in a
// future test cannot panic inside a live SMTP session. Shared and never read.
var Discard = New()

// counter binds a field to its two stable names.
//
// The Snapshot key is a CONSOLE contract — it travels in the heartbeat and
// webui-fastify stores it. The Prometheus name is a DASHBOARD contract.
// Renaming either is a breaking change downstream; add a new counter instead.
type counter struct {
	// key is the Snapshot key. It must stay within 64 characters: the
	// console's AgentMetrics schema (webui-fastify/src/validation/agent.ts)
	// rejects a longer one, and a rejected report loses the heartbeat too.
	key  string
	name string
	help string
	get  func(*Metrics) *atomic.Int64
}

var counters = []counter{
	{"conn_accepted", "mailgw_connections_accepted_total",
		"Connections accepted past the IP allowlist. Per connection.",
		func(m *Metrics) *atomic.Int64 { return &m.ConnAccepted }},
	{"conn_denied", "mailgw_connections_denied_total",
		"Connections refused by the IP allowlist, before the banner. Per connection.",
		func(m *Metrics) *atomic.Int64 { return &m.ConnDenied }},
	{"proxy_dropped", "mailgw_proxy_dropped_total",
		"Connections closed because their PROXY header was missing, malformed, " +
			"or came from a peer outside proxy_trusted. Per connection, and " +
			"counted BEFORE the allowlist. Non-zero on a working deployment means " +
			"the load balancer stopped sending it.",
		func(m *Metrics) *atomic.Int64 { return &m.ProxyDropped }},
	{"conn_throttled", "mailgw_connections_throttled_total",
		"Connections answered 421 and closed because max.connections was already " +
			"reached. Per connection, and counted AFTER the allowlist — so every " +
			"one of these is also inside mailgw_connections_accepted_total. " +
			"Sustained non-zero means the cap is too low or sessions are not " +
			"ending; check inactivity_timeout before raising it.",
		func(m *Metrics) *atomic.Int64 { return &m.ConnThrottled }},

	{"rate_conn", "mailgw_ratelimited_connections_total",
		"Connections refused 421 for exceeding ratelimit.connect_per_ip. Per " +
			"CONNECTION, and — like mailgw_connections_throttled_total — a SUBSET " +
			"of mailgw_connections_accepted_total, because this limiter sits " +
			"inside the allowlist. Always 0 unless connect_per_ip is set.",
		func(m *Metrics) *atomic.Int64 { return &m.RateConn }},
	{"rate_sender", "mailgw_ratelimited_senders_total",
		"Messages refused 450 at MAIL FROM for exceeding " +
			"ratelimit.messages_per_sender. Per MESSAGE. A sustained non-zero " +
			"rate is usually one runaway application, not an attack — check " +
			"which sender before raising the limit.",
		func(m *Metrics) *atomic.Int64 { return &m.RateSender }},
	{"rate_user", "mailgw_ratelimited_users_total",
		"Messages refused 450 at MAIL FROM for exceeding " +
			"ratelimit.messages_per_user. Per MESSAGE. Only authenticated " +
			"sessions can reach this, so it is always 0 without auth.json.",
		func(m *Metrics) *atomic.Int64 { return &m.RateUser }},
	{"rate_rcpt_domain", "mailgw_ratelimited_recipients_total",
		"Recipients refused 450 at RCPT TO for exceeding " +
			"ratelimit.rcpts_per_domain. Per RECIPIENT, not per message: one " +
			"message to fifty addresses at a limited domain contributes fifty, " +
			"and the other recipients of that message are unaffected.",
		func(m *Metrics) *atomic.Int64 { return &m.RateRcptDomain }},
	{"rate_auth", "mailgw_ratelimited_auth_total",
		"AUTH commands refused 454 for exceeding ratelimit.auth_failures_per_ip. " +
			"Per AUTH COMMAND, the same unit as mailgw_auth_failed_total — but " +
			"NOT a subset of it: a refusal here happens before the password is " +
			"compared, so it is not counted as a failed authentication. Together " +
			"they are what a credential-stuffing run looks like.",
		func(m *Metrics) *atomic.Int64 { return &m.RateAuth }},

	{"auth_ok", "mailgw_auth_succeeded_total",
		"AUTH commands that succeeded. Per AUTH COMMAND, not per connection or " +
			"message: one authenticated session sends as many messages as it likes " +
			"and is counted here once.",
		func(m *Metrics) *atomic.Int64 { return &m.AuthOK }},
	{"auth_failed", "mailgw_auth_failed_total",
		"AUTH commands refused 535 for a bad username or password. Per AUTH " +
			"COMMAND. This is where a credential-stuffing run against the " +
			"submission port shows up, and until the rate limiter lands it is the " +
			"only place it shows up at all — alert on the rate, not the total.",
		func(m *Metrics) *atomic.Int64 { return &m.AuthFailed }},

	{"spf_checked", "mailgw_spf_checked_total",
		"Messages an SPF evaluation ran for. Per INBOUND MESSAGE, counted at " +
			"MAIL FROM. Zero unless msgauth.spf.enabled or a rule reads spf.*.",
		func(m *Metrics) *atomic.Int64 { return &m.SPFChecked }},
	{"spf_failed", "mailgw_spf_failed_total",
		"SPF evaluations that returned \"fail\" — the sending domain says this IP " +
			"may not use it. Per INBOUND MESSAGE, and a SUBSET of " +
			"mailgw_spf_checked_total, not a sibling. Nothing is refused on this " +
			"by default; a rule reading spf.result is what decides.",
		func(m *Metrics) *atomic.Int64 { return &m.SPFFailed }},
	{"dkim_verified", "mailgw_dkim_verified_total",
		"Messages a DKIM verification pass ran over. Per INBOUND MESSAGE, not per " +
			"signature: one message carrying three signatures counts once. Each " +
			"pass re-reads the spooled body, so this also measures what that " +
			"re-read is costing.",
		func(m *Metrics) *atomic.Int64 { return &m.DKIMVerified }},
	{"dkim_signed", "mailgw_dkim_signed_total",
		"Outbound messages a DKIM-Signature was added to. Per ENVELOPE-ATTEMPT, " +
			"NOT per message: one message split across three relay groups and " +
			"retried twice signs six times, because the signature is computed at " +
			"delivery over the headers this gateway prepends.",
		func(m *Metrics) *atomic.Int64 { return &m.DKIMSigned }},
	{"dkim_sign_failed", "mailgw_dkim_sign_failed_total",
		"Envelope-attempts where signing was configured for the message's From " +
			"domain but failed, so the message went out UNSIGNED. Per " +
			"ENVELOPE-ATTEMPT. Any non-zero value is a configuration problem: mail " +
			"from a domain publishing DMARC is being delivered here and refused at " +
			"the far end, while every log line on this side says delivered. A " +
			"message with no key for its From domain is NOT counted — that is not " +
			"signing, rather than failing to sign.",
		func(m *Metrics) *atomic.Int64 { return &m.DKIMSignFailed }},

	{"msg_accepted", "mailgw_messages_accepted_total",
		"Messages answered 250 at end of DATA. Per message. " +
			"Discarded and quarantined messages are a SUBSET of this, not a separate bucket.",
		func(m *Metrics) *atomic.Int64 { return &m.MsgAccepted }},
	{"msg_rejected", "mailgw_messages_rejected_total",
		"Messages refused 5xx by a message-scoped policy rule. Per message. " +
			"Includes connect- and helo-stage refusals, where no message was offered yet.",
		func(m *Metrics) *atomic.Int64 { return &m.MsgRejected }},
	{"msg_tempfailed", "mailgw_messages_tempfailed_total",
		"Messages refused 4xx by a message-scoped policy rule. Per message.",
		func(m *Metrics) *atomic.Int64 { return &m.MsgTempfailed }},
	{"msg_discarded", "mailgw_messages_discarded_total",
		"Messages a message-scoped discard dropped: accepted, then sent nowhere. Per message.",
		func(m *Metrics) *atomic.Int64 { return &m.MsgDiscarded }},
	{"msg_quarantined", "mailgw_messages_quarantined_total",
		"Messages a message-scoped quarantine held back. Per message.",
		func(m *Metrics) *atomic.Int64 { return &m.MsgQuarantined }},
	{"msg_loop_rejected", "mailgw_messages_loop_rejected_total",
		"Messages refused 554 for carrying more Received headers than " +
			"max.received_headers. Per message, and a SUBSET of " +
			"mailgw_messages_rejected_total. Sustained non-zero means mail is " +
			"looping between this gateway and something else.",
		func(m *Metrics) *atomic.Int64 { return &m.MsgLoopRejected }},

	{"rcpt_accepted", "mailgw_recipients_accepted_total",
		"Recipients answered 250 at RCPT TO. Per recipient. " +
			"A recipient later discarded or quarantined is still counted here.",
		func(m *Metrics) *atomic.Int64 { return &m.RcptAccepted }},
	{"rcpt_rejected", "mailgw_recipients_rejected_total",
		"Recipients refused 5xx. Per recipient.",
		func(m *Metrics) *atomic.Int64 { return &m.RcptRejected }},
	{"rcpt_tempfailed", "mailgw_recipients_tempfailed_total",
		"Recipients refused 4xx. Per recipient.",
		func(m *Metrics) *atomic.Int64 { return &m.RcptTempfailed }},
	{"rcpt_discarded", "mailgw_recipients_discarded_total",
		"Recipients accepted and then dropped without being relayed. Per recipient.",
		func(m *Metrics) *atomic.Int64 { return &m.RcptDiscarded }},

	{"bytes_in", "mailgw_bytes_received_total",
		"Message bytes spooled, including the Received header this gateway prepends.",
		func(m *Metrics) *atomic.Int64 { return &m.BytesIn }},

	{"env_queued", "mailgw_envelopes_queued_total",
		"Envelopes written to the outbound queue. Per envelope.",
		func(m *Metrics) *atomic.Int64 { return &m.EnvelopesQueued }},
	{"env_quarantined", "mailgw_envelopes_quarantined_total",
		"Envelopes filed in quarantine rather than queued. Per envelope.",
		func(m *Metrics) *atomic.Int64 { return &m.EnvelopesQuarantined }},
	{"env_expired", "mailgw_envelopes_expired_total",
		"Envelopes given up on for outliving max_lifetime and buried in dead/. Per envelope.",
		func(m *Metrics) *atomic.Int64 { return &m.EnvelopesExpired }},
	{"env_released", "mailgw_envelopes_released_total",
		"Envelopes an operator released from quarantine back into the queue. Per envelope.",
		func(m *Metrics) *atomic.Int64 { return &m.EnvelopesReleased }},

	{"deliver_attempts", "mailgw_delivery_attempts_total",
		"Delivery attempts. Per ENVELOPE, once per pass over its relay group.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverAttempts }},
	{"deliver_ok", "mailgw_delivery_delivered_total",
		"Recipients a relay accepted. Per RECIPIENT.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverOK }},
	{"deliver_deferred", "mailgw_delivery_deferred_total",
		"Attempts that ended with the envelope requeued. Per ENVELOPE-ATTEMPT — " +
			"one, however many relays in the group were tried.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverDeferred }},
	{"deliver_bounced", "mailgw_delivery_bounced_total",
		"Recipients a relay rejected 5xx. Per RECIPIENT.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverBounced }},
	{"deliver_connfail", "mailgw_delivery_connect_failed_total",
		"Connection-level failures: dial, TLS, AUTH or MAIL FROM. Per RELAY, so " +
			"one attempt over a three-relay group can add three.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverConnFail }},
	{"deliver_connreused", "mailgw_delivery_connections_reused_total",
		"Attempts served by an already-open relay connection instead of a new dial. " +
			"Per RELAY-attempt. Always 0 unless outbound.reuse_connections is on.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverConnReused }},
	{"deliver_pool_full", "mailgw_delivery_pool_full_total",
		"Finished relay connections closed instead of kept, because " +
			"outbound.max_pooled_connections was already reached. Per RELAY-attempt. " +
			"Always 0 unless outbound.reuse_connections is on. Nothing fails when " +
			"this moves — the next envelope to that relay pays a fresh dial — but a " +
			"steady rate means the cap, not the workload, is deciding what gets " +
			"pooled. Being over per_group_connections is NOT counted here: that is " +
			"one relay behaving as configured, where this is a global ceiling.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverPoolFull }},
	{"deliver_tls_downgraded", "mailgw_delivery_tls_downgraded_total",
		"Attempts against an opportunistic relay where STARTTLS failed and the " +
			"message was sent in the clear. Per RELAY-attempt. A relay that used " +
			"to encrypt and now does not shows up here and nowhere else.",
		func(m *Metrics) *atomic.Int64 { return &m.DeliverTLSDowngraded }},

	{"dsn_generated", "mailgw_dsn_generated_total",
		"Delivery status notifications this gateway created and queued. Per DSN " +
			"MESSAGE — one report can name several recipients.",
		func(m *Metrics) *atomic.Int64 { return &m.DSNGenerated }},
	{"dsn_suppressed", "mailgw_dsn_suppressed_total",
		"Bounces not generated because the failed message was itself a bounce or " +
			"had a null sender. Per DSN message that was NOT sent.",
		func(m *Metrics) *atomic.Int64 { return &m.DSNSuppressed }},
	{"dsn_unroutable", "mailgw_dsn_unroutable_total",
		"Bounces dropped because no rule and no dsn.relay_group gave them a relay " +
			"group. Per DSN message that was NOT sent. Any non-zero value is a " +
			"configuration problem: senders are not learning their mail failed.",
		func(m *Metrics) *atomic.Int64 { return &m.DSNUnroutable }},
	{"dsn_notify_suppressed", "mailgw_dsn_notify_suppressed_total",
		"Recipients left out of a notification because the sender's RFC 3461 " +
			"NOTIFY parameter did not ask for one. Per RECIPIENT — the odd unit in " +
			"this group, because NOTIFY is per recipient and a report naming three " +
			"can omit one. A report emptied entirely by this is also counted in " +
			"mailgw_dsn_suppressed_total. Like that counter, it can repeat across " +
			"attempts for a partially-terminal envelope.",
		func(m *Metrics) *atomic.Int64 { return &m.DSNNotifySuppressed }},

	{"events_spilled", "mailgw_events_spilled_total",
		"Audit events parked in failed-events/ because logservice would not take " +
			"them. Per EVENT. Non-zero means the log tables are missing rows until " +
			"a replay succeeds.",
		func(m *Metrics) *atomic.Int64 { return &m.EventsSpilled }},
	{"events_replayed", "mailgw_events_replayed_total",
		"Spilled audit events a replay pass delivered. Per EVENT.",
		func(m *Metrics) *atomic.Int64 { return &m.EventsReplayed }},
	{"events_replay_failed", "mailgw_events_replay_failed_total",
		"Spilled audit events a replay gave up on permanently and moved to " +
			"failed-events/rejected/. Per EVENT. Usually a 4xx — the payload does " +
			"not match the server's schema, so resending it cannot help — but also " +
			"a record that will not parse, or one naming a kind with no configured " +
			"endpoint. The file says which.",
		func(m *Metrics) *atomic.Int64 { return &m.EventsReplayFailed }},
	{"events_dropped", "mailgw_events_dropped_total",
		"Audit events discarded because the in-memory buffer was full, or because " +
			"they arrived after the pipeline closed at shutdown. Per EVENT, " +
			"and UNLIKE mailgw_events_spilled_total these are gone: a spilled event " +
			"is on disk and a replay can still deliver it, a dropped one was never " +
			"written anywhere. Non-zero means events.buffer_size or events.senders " +
			"cannot keep up with the mail rate.",
		func(m *Metrics) *atomic.Int64 { return &m.EventsDropped }},
}

// Gauges are read at scrape time rather than counted.
type Gauges struct {
	// QueueOK reports whether the four depths below were actually measured.
	// False means the spool does not exist yet — a managed gateway before its
	// first configuration applies — or that reading it failed.
	//
	// When it is false the queue series are OMITTED rather than reported as
	// zero. A fabricated 0 is the lie that makes a dashboard read "drained"
	// when what it means is "unreadable".
	QueueOK         bool
	QueueReady      int
	QueueInflight   int
	QueueQuarantine int
	QueueDead       int
	// FailedEvents is audit events waiting to be replayed, not envelopes. It
	// rides the same QueueOK flag because it comes from the same spool read.
	FailedEvents int
	// RejectedEvents is audit events a replay gave up on permanently. Nothing
	// retries them; only events.rejected_retention drains them, and only if the
	// background replay pass is running at all.
	RejectedEvents int

	// ConfigVersion is the applied configuration version, 0 in file mode.
	ConfigVersion int
	Approved      bool
	Serving       bool
	// Managed distinguishes "not approved" from "approval is meaningless here",
	// which is the difference between a file-mode gateway and a broken one.
	Managed bool

	Version string
	Commit  string
}

// Snapshot renders the counters as the heartbeat sends them.
//
// A map rather than the struct itself: the atomic fields make Metrics
// non-copyable, and the keys are a contract the console depends on.
func (m *Metrics) Snapshot() map[string]int64 {
	out := make(map[string]int64, len(counters))
	if m == nil {
		return out
	}
	for _, c := range counters {
		out[c.key] = c.get(m).Load()
	}
	return out
}

// WritePrometheus renders the exposition format, counters then gauges.
//
// The caller is expected to buffer: a mid-write failure must not leave half an
// exposition under a 200.
func (m *Metrics) WritePrometheus(w io.Writer, g Gauges) error {
	if m == nil {
		m = Discard
	}

	for _, c := range counters {
		if err := writeSeries(w, c.name, "counter", c.help, "",
			fmt.Sprintf("%d", c.get(m).Load())); err != nil {
			return err
		}
	}

	// version and commit come from -ldflags, so their contents are whatever a
	// build pipeline put there. Escaping them is the one thing a hand-rolled
	// exposition writer reliably gets wrong.
	// Quoted by hand rather than with %q: Go's quoting is not the exposition
	// format's, and applying both would escape everything twice.
	labels := fmt.Sprintf(`{version="%s",commit="%s"}`,
		escapeLabel(g.Version), escapeLabel(g.Commit))
	if err := writeSeries(w, "mailgw_build_info", "gauge",
		"Build information for the running binary; the value is always 1.",
		labels, "1"); err != nil {
		return err
	}

	gauges := []struct {
		name, help string
		value      int
	}{
		{"mailgw_config_version",
			"The configuration version currently applied; 0 before the first deploy.", g.ConfigVersion},
		{"mailgw_managed",
			"Always 1: Central Management is a gateway's only configuration source. Kept because the name is a dashboard contract.", boolGauge(g.Managed)},
		{"mailgw_approved",
			"1 when Central Management has approved this gateway.", boolGauge(g.Approved)},
		{"mailgw_serving",
			"1 when the SMTP listeners are up.", boolGauge(g.Serving)},
	}
	for _, s := range gauges {
		if err := writeSeries(w, s.name, "gauge", s.help, "",
			fmt.Sprintf("%d", s.value)); err != nil {
			return err
		}
	}

	// Omitted entirely when the spool could not be read — see Gauges.QueueOK.
	if !g.QueueOK {
		return nil
	}
	queue := []struct {
		name, help string
		value      int
	}{
		{"mailgw_queue_ready", "Envelopes waiting in the outbound queue.", g.QueueReady},
		{"mailgw_queue_inflight", "Envelopes currently being delivered.", g.QueueInflight},
		{"mailgw_queue_quarantine", "Envelopes held in quarantine. Nothing drains this on its own.", g.QueueQuarantine},
		{"mailgw_queue_dead", "Envelopes that expired or were buried.", g.QueueDead},
		{"mailgw_failed_events", "Audit events waiting in failed-events/ for a replay. " +
			"Non-zero means the log tables are missing rows.", g.FailedEvents},
		{"mailgw_failed_events_rejected", "Audit events a replay gave up on permanently, " +
			"held in failed-events/rejected/ as the evidence of what was refused — a 4xx, " +
			"an unparseable record, or a kind with no configured endpoint. " +
			"Nothing retries these; only events.rejected_retention removes them, " +
			"counting from when the event was filed here.",
			g.RejectedEvents},
	}
	for _, s := range queue {
		if err := writeSeries(w, s.name, "gauge", s.help, "",
			fmt.Sprintf("%d", s.value)); err != nil {
			return err
		}
	}
	return nil
}

func writeSeries(w io.Writer, name, typ, help, labels, value string) error {
	_, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s%s %s\n",
		name, help, name, typ, name, labels, value)
	return err
}

func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}

// escapeLabel escapes a label value per the exposition format: backslash,
// double quote and newline.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}
