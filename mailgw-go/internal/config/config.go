package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"sigs.k8s.io/yaml"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/msgauth"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// File names within the config directory.
const (
	FileServer    = "server.yaml"
	FileRelays    = "relays.json"
	FileRouting   = "routing.yaml"
	FileRoutingJS = "routing.json" // legacy Haraka format
	FileFilter    = "ngmfilter.json"
	FileLogging   = "logging.json"
	FileAdmin     = "admin.json"
	FileAuth      = "auth.json"
)

// Duration accepts either a duration string ("300s", "4h") or a bare number of
// seconds, so hand-written YAML and machine-generated JSON both work.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		v, err := time.ParseDuration(strings.TrimSpace(str))
		if err != nil {
			return fmt.Errorf("duration %q: %w", str, err)
		}
		*d = Duration(v)
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("duration %s: not a number or duration string", s)
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D unwraps to a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Listener is one inbound socket.
type Listener struct {
	Addr string `json:"addr"`
	// ImplicitTLS wraps the socket in TLS from the first byte (submissions,
	// port 465) rather than advertising STARTTLS.
	ImplicitTLS bool `json:"implicit_tls,omitempty"`

	// ProxyProtocol reads a PROXY header from every connection, so the IP
	// allowlist and the conn.* rules see the client rather than the L4 load
	// balancer in front of them. Fail-closed: a connection with no valid header,
	// or from a peer outside ProxyTrusted, is dropped without a reply.
	//
	// A listener with this set MUST NOT be reachable directly. A PROXY header is
	// trivially forged, and ProxyTrusted is the only thing standing between a
	// forged one and an open relay.
	//
	// Note a v2 LOCAL command — which is what a balancer's own health check
	// usually sends — carries no client address, so that connection is judged on
	// the balancer's own address and needs to be in ngmfilter.json to be answered.
	ProxyProtocol bool `json:"proxy_protocol,omitempty"`
	// ProxyTrusted are the peers whose PROXY header is honoured: bare addresses
	// or CIDR blocks, the same syntax as ngmfilter.json's "allowed". Required,
	// and required non-empty, whenever ProxyProtocol is set.
	ProxyTrusted []string `json:"proxy_trusted,omitempty"`
}

// TLSConfig points at the certificate pair. Both fields empty disables TLS.
//
// Cert and Key are paths on the gateway's own filesystem, not material carried
// in a configuration bundle — a private key that travelled from the console
// would be stored there forever in an immutable ConfigVersions row, and would
// reach every gateway assigned that profile. A managed node with neither set
// generates its own self-signed pair into its data directory instead
// (internal/tlsx).
type TLSConfig struct {
	Cert string `json:"cert,omitempty"`
	Key  string `json:"key,omitempty"`
	// STARTTLS advertises the upgrade on plain listeners. It DEFAULTS TO TRUE,
	// so it is an opt-out: a gateway that has a certificate should offer
	// encryption, and every sending MTA that cannot use it simply will not.
	// Setting it false is for a submissions-only node whose plain port must stay
	// plain.
	STARTTLS bool `json:"starttls"`

	// AllowInsecureAuth offers AUTH on an unencrypted session. It DEFAULTS TO
	// FALSE and go-smtp answers 523 5.7.10 without it, so a password is never
	// invited onto a cleartext socket by accident.
	//
	// It lives here rather than beside the credentials because it is a property
	// of the listener, not of any user — the same distinction relays.Relay draws
	// with allow_insecure_auth on the outbound side. Being part of TLSConfig
	// also puts it on restartRequired's `tls` entry for free, which is correct:
	// it is read once when the go-smtp server is built.
	//
	// The legitimate case is an operator terminating TLS in front of the
	// gateway; that operator should also set proxy_protocol on the listener, or
	// the allowlist and the conn.* rules see the balancer instead of the client.
	AllowInsecureAuth bool `json:"allow_insecure_auth,omitempty"`
}

// Limits mirror the [max] and [headers] sections of Haraka's connection.ini.
//
// Must stay a COMPARABLE struct: restartRequired compares it with != and
// TestRestartRequired_SameBytesAreEqual is the guard.
type Limits struct {
	Bytes           int64 `json:"bytes"`
	LineLength      int   `json:"line_length"`
	Recipients      int   `json:"recipients"`
	ReceivedHeaders int   `json:"received_headers"`
	HeaderLines     int   `json:"header_lines"`

	// Connections caps concurrent inbound sessions across every listener. Over
	// it, a peer is answered 421 4.7.0 and closed — the code that says "come
	// back", not "give up".
	//
	// It has no Haraka equivalent: go-smtp spawns a goroutine per accepted
	// connection with no cap of its own, and each one costs a file descriptor
	// plus, once DATA starts, a spool temp file. Outbound has had concurrency
	// and per_group_connections since M1; this is the inbound side of that.
	//
	// It is worth nothing without inactivity_timeout. A cap over sessions that
	// never end is a denial-of-service primitive rather than a defence — one
	// slow peer per slot and the gateway is closed for business — which is why
	// that timeout is validated positive too.
	Connections int `json:"connections"`
}

// LogConfig controls the structured logger.
type LogConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"` // json | text
}

// Outbound configures the delivery queue.
type Outbound struct {
	Concurrency         int `json:"concurrency"`
	PerGroupConnections int `json:"per_group_connections"`
	// PollInterval is the longest the scheduler will sleep, not a fixed tick.
	// Queued work wakes it exactly when due, and a new message wakes it
	// immediately, so this only bounds how long an envelope placed in q/ by
	// something other than the gateway can sit unnoticed. Raising it is cheap.
	PollInterval      Duration   `json:"poll_interval"`
	SpoolDir          string     `json:"spool_dir"`
	Backoff           []Duration `json:"backoff"`
	MaxLifetime       Duration   `json:"max_lifetime"`
	DelayWarningAfter Duration   `json:"delay_warning_after"`
	Jitter            float64    `json:"jitter"`
	// ConnectTimeout and DataTimeout bound a single relay attempt.
	ConnectTimeout Duration `json:"connect_timeout"`
	DataTimeout    Duration `json:"data_timeout"`

	// ReuseConnections keeps relay connections open between envelopes.
	//
	// Off by default, and deliberately so. Turning it on changes what every
	// relay in the field sees from this gateway — many cap messages per
	// connection, many rate-limit per connection rather than per message — and
	// none of that is observable from here. Enable it per deployment, after
	// watching deliver_connfail, rather than inheriting it from a default.
	ReuseConnections bool `json:"reuse_connections,omitempty"`
	// MaxMessagesPerConnection retires a reused connection before a relay's own
	// limit turns the message after it into a 421 that looks like a failure.
	MaxMessagesPerConnection int `json:"max_messages_per_connection,omitempty"`
	// ConnectionIdleTimeout drops a pooled connection that has sat unused. A
	// relay closes idle connections on its own schedule; one we already know is
	// stale is cheaper to drop than to discover.
	ConnectionIdleTimeout Duration `json:"connection_idle_timeout,omitempty"`

	// MXCacheTTL is how long an MX answer is reused for a use_mx relay. Go's
	// resolver does not expose record TTLs, so this is a fixed interval rather
	// than the zone's own.
	MXCacheTTL Duration `json:"mx_cache_ttl,omitempty"`
}

// DSNConfig controls bounce generation.
type DSNConfig struct {
	Enabled bool   `json:"enabled"`
	Return  string `json:"return"` // headers | full
	// Postmaster is the address bounces are sent as. Empty resolves to
	// postmaster@<hostname> at bring-up — see PostmasterFor.
	Postmaster string `json:"postmaster,omitempty"`

	// RelayGroup is where a bounce goes when no route rule claims it.
	//
	// Bounces are routed by the same rule engine as ordinary mail, but they are
	// addressed to the original SENDER, and a rule set written around recipient
	// domains often has nothing that matches one. Falling back to the relay
	// group the failed message was using would be a guess: that group is the
	// smarthost for the recipient's domain and would usually answer "relay
	// denied", so the bounce would die in dead/ and the sender would learn
	// nothing. Naming the group is the only answer that is not a guess.
	RelayGroup string `json:"relay_group,omitempty"`

	// MaxReturnBytes caps how much of the original message a "full" return
	// quotes; past it the notification falls back to headers only, which RFC
	// 3461 §6.2 explicitly permits. Without a cap, one 25 MiB message failing
	// across three relay groups writes three 25 MiB bounces.
	MaxReturnBytes int64 `json:"max_return_bytes,omitempty"`
}

// FullReturn reports whether a bounce should quote the whole original message.
func (d DSNConfig) FullReturn() bool { return strings.EqualFold(d.Return, "full") }

// PostmasterFor resolves the address bounces are sent as.
//
// Defaulted here rather than in defaults() because it depends on the hostname,
// which is itself configurable — and a bounce with an empty From is worse than
// one from an address nobody reads.
func (d DSNConfig) PostmasterFor(hostname string) string {
	if p := strings.TrimSpace(d.Postmaster); p != "" {
		return p
	}
	return "postmaster@" + hostname
}

// AttachConfig controls the attachment MD5 blocklist check.
type AttachConfig struct {
	Enabled bool     `json:"enabled"`
	URL     string   `json:"url"`
	Timeout Duration `json:"timeout"`
	// Fail is "closed" or "open". Haraka's AttachChecker.js fails OPEN on any
	// error, which turns a scanner outage into a silent bypass; we default to
	// closed.
	Fail string `json:"fail"`
	// IncludeInline scans parts whose disposition is not "attachment".
	// AttachChecker.js:51 skips them, which is a bypass.
	IncludeInline bool `json:"include_inline"`
	// OnBlock is what happens to a message the blocklist rejected:
	// reject | tempfail | quarantine | discard.
	//
	// Defaults to reject. npFilterAttach.js:45 answers DENYSOFT, which makes a
	// sender retry for four days a message that will never be accepted — the
	// digest is on a blocklist and nothing about a retry changes that. Set it to
	// tempfail for literal Haraka parity, or to quarantine to hold the message
	// for an operator (`mailgw-go mailq release`) instead of refusing it.
	OnBlock string `json:"on_block,omitempty"`
}

// RateLimits bounds how OFTEN things may happen, per key.
//
// The block is `ratelimit:` and not `limits:` as M15's plan worded it, because
// `max:` already unmarshals into a type called Limits — and `max.connections` (a
// concurrency cap) beside `limits.connect_per_ip` (a rate) is precisely the pair
// an operator would set the wrong one of.
//
// Every limit is OFF by default. This gateway's defaults do not silently start
// refusing mail; attach.enabled, outbound.reuse_connections and every msgauth
// check ship off for the same reason.
//
// Rate limits are read LIVE, not captured at bring-up — see gateway.limiter.
// They are deliberately absent from restartRequired: a limit an operator cannot
// adjust without restarting a mail server during an incident is a limit they
// will not use.
type RateLimits struct {
	// ConnectPerIP is checked in the listener chain, once per accepted
	// connection, and answers 421. It is the cheapest of these and the one that
	// answers an abusive peer.
	ConnectPerIP RateLimit `json:"connect_per_ip,omitempty"`

	// MessagesPerSender and MessagesPerUser are checked at MAIL FROM and answer
	// 450. The first answers a runaway application; the second needs M13's
	// inbound AUTH to have a username at all.
	MessagesPerSender RateLimit `json:"messages_per_sender,omitempty"`
	MessagesPerUser   RateLimit `json:"messages_per_user,omitempty"`

	// RcptsPerDomain is checked at RCPT TO, per RECIPIENT, and answers 450 for
	// that recipient alone.
	//
	// This is an INBOUND control on where mail is going, and is not
	// outbound.per_group_connections, which bounds connections to a relay.
	RcptsPerDomain RateLimit `json:"rcpts_per_domain,omitempty"`

	// AuthFailuresPerIP is checked before the password comparison, so a refusal
	// costs no bcrypt. It is what makes AUTH safe to expose.
	AuthFailuresPerIP RateLimit `json:"auth_failures_per_ip,omitempty"`

	// MaxKeys bounds how many buckets are tracked across every dimension. Zero
	// means ratelimit.DefaultMaxKeys — not "unbounded", which is the state the
	// field exists to prevent.
	MaxKeys int `json:"max_keys,omitempty"`
}

// RateLimit is one configured limit: Rate events per Per, with Burst allowed at
// once. Rate 0 disables it.
type RateLimit struct {
	Rate  int      `json:"rate"`
	Per   Duration `json:"per,omitempty"`
	Burst int      `json:"burst,omitempty"`
}

// Enabled reports whether this limit limits anything.
func (r RateLimit) Enabled() bool { return r.Rate > 0 }

// Any reports whether any dimension is limited, which is what decides whether a
// gateway builds a limiter at all.
func (r RateLimits) Any() bool {
	for _, l := range r.all() {
		if l.limit.Enabled() {
			return true
		}
	}
	return false
}

// named pairs a limit with the configuration key it came from, so validation and
// `check` name the same thing an operator typed.
type namedLimit struct {
	key   string
	limit RateLimit
}

func (r RateLimits) all() []namedLimit {
	return []namedLimit{
		{"connect_per_ip", r.ConnectPerIP},
		{"messages_per_sender", r.MessagesPerSender},
		{"messages_per_user", r.MessagesPerUser},
		{"rcpts_per_domain", r.RcptsPerDomain},
		{"auth_failures_per_ip", r.AuthFailuresPerIP},
	}
}

// Enabled lists the limits that are on, as "key rate/per", for `check`.
func (r RateLimits) Enabled() []string {
	var out []string
	for _, l := range r.all() {
		if !l.limit.Enabled() {
			continue
		}
		s := fmt.Sprintf("%s %d/%s", l.key, l.limit.Rate, l.limit.Per.D())
		if l.limit.Burst > 0 {
			s += fmt.Sprintf(" burst %d", l.limit.Burst)
		}
		out = append(out, s)
	}
	return out
}

func (r RateLimits) validate() error {
	for _, l := range r.all() {
		if !l.limit.Enabled() {
			// A `per` or `burst` with no rate is not an error but it is almost
			// certainly a half-finished edit, and it limits nothing.
			continue
		}
		// Unlike max.connections and attach.timeout, a missing window here is
		// not an explicit zero overwriting a default — there IS no default,
		// because a rate has no sensible one. It has to be stated.
		if l.limit.Per <= 0 {
			return fmt.Errorf("%s: ratelimit.%s needs a positive 'per' "+
				"(a rate with no window is not a rate)", FileServer, l.key)
		}
		if l.limit.Burst < 0 {
			return fmt.Errorf("%s: ratelimit.%s: 'burst' cannot be negative "+
				"(0 means a full window's worth)", FileServer, l.key)
		}
	}
	if r.MaxKeys < 0 {
		return fmt.Errorf("%s: ratelimit.max_keys cannot be negative "+
			"(0 means the built-in ceiling)", FileServer)
	}
	return nil
}

// MsgAuthConfig controls message authentication: SPF, DKIM and DMARC on the way
// in, and DKIM signing on the way out.
//
// It lives inside Server rather than in a file of its own — unlike Admin and
// Auth — because it is mail-path policy an operator writes as YAML, so a
// `server` profile's raw text carries it and the console needs no new bundle key
// and no schema change. What the console CANNOT carry is the key material; see
// DKIMKey.
type MsgAuthConfig struct {
	SPF   MsgAuthCheck `json:"spf"`
	DKIM  MsgAuthCheck `json:"dkim"`
	DMARC MsgAuthCheck `json:"dmarc"`

	// AuthservID is the name this gateway signs Authentication-Results with, and
	// the name it strips forged fields under. Empty resolves to the hostname at
	// bring-up — the same treatment DSNConfig.PostmasterFor gets, and for the
	// same reason: it depends on a value that is itself configurable, so it
	// cannot be filled in by defaults().
	AuthservID string `json:"authserv_id,omitempty"`

	// MaxDKIMSignatures bounds how many signatures on one message are verified.
	// Each costs a DNS lookup and a public-key verification, so an unbounded
	// count is an amplification primitive pointed at this gateway.
	MaxDKIMSignatures int `json:"max_dkim_signatures"`

	// DNSTimeout bounds one DNS lookup. The whole walk is bounded by the SMTP
	// session's context; this stops a single black-holed nameserver from eating
	// all of it.
	DNSTimeout Duration `json:"dns_timeout"`

	Sign DKIMSignConfig `json:"sign"`
}

// MsgAuthCheck turns one inbound check on.
//
// A struct rather than a bool so a check can grow its own settings without
// changing the shape of a configuration already in the field.
type MsgAuthCheck struct {
	Enabled bool `json:"enabled"`
}

// DKIMSignConfig configures outbound signing.
type DKIMSignConfig struct {
	Enabled bool      `json:"enabled"`
	Keys    []DKIMKey `json:"keys,omitempty"`

	// Canonicalization is "header/body", e.g. "relaxed/relaxed". Empty means
	// relaxed/relaxed, NOT RFC 6376's simple/simple: simple breaks on any
	// whitespace change in transit and every hop between here and the recipient
	// is entitled to make one.
	Canonicalization string `json:"canonicalization,omitempty"`

	// Headers overrides which header fields are signed. Empty is
	// msgauth.DefaultHeaderKeys, the RFC 6376 §5.4.1 recommended set minus the
	// ones every hop rewrites.
	Headers []string `json:"headers,omitempty"`

	// Expiration sets the x= tag. Zero means the signature does not expire,
	// which is the usual choice: an expiry only matters against replay, and a
	// message replayed after it has already been delivered is a spam problem
	// rather than an authentication one.
	Expiration Duration `json:"expiration,omitempty"`
}

// DKIMKey is one signing identity.
//
// Key is a PATH on the gateway's own filesystem, never key material. This is
// the rule stated on TLSConfig and in plans/M8: the console stores every
// configuration version for ever and serves it to every gateway on the profile,
// so a private key placed in a bundle would be permanently retained and
// fleet-wide. The consequence is real and deliberate — a fleet cannot be given a
// signing key from the console, exactly as it cannot be given a real TLS
// certificate — and it is not worked around by generating one either, because a
// self-generated key whose public half is not in DNS produces signatures that
// FAIL verification, which is worse than not signing.
type DKIMKey struct {
	Domain   string `json:"domain"`
	Selector string `json:"selector"`
	Key      string `json:"key"`
}

// Wants reports whether anything at all is configured, which is what decides
// whether the gateway builds a msgauth checker at bring-up.
func (m MsgAuthConfig) Wants() bool {
	return m.SPF.Enabled || m.DKIM.Enabled || m.DMARC.Enabled || m.Sign.Enabled
}

// AuthservIDFor resolves the name this gateway uses in Authentication-Results.
func (m MsgAuthConfig) AuthservIDFor(hostname string) string {
	if id := strings.TrimSpace(m.AuthservID); id != "" {
		return id
	}
	return hostname
}

// Events configures the logservice event pipeline.
type Events struct {
	// URLs come from logging.json; these tune the transport.
	Timeout    Duration `json:"timeout"`
	Retries    int      `json:"retries"`
	BufferSize int      `json:"buffer_size"`
	Senders    int      `json:"senders"`
	APIKeyEnv  string   `json:"api_key_env"`

	// ReplayInterval is how often events parked in failed-events/ are resent.
	// Zero disables the background pass; `mailgw-go events replay` still works.
	//
	// Slow on purpose. This repairs an outage that has already ended — the
	// delivery path is Send, with its own retries — so a short interval would
	// only make a gateway hammer a logservice that is still down.
	ReplayInterval Duration `json:"replay_interval,omitempty"`

	// RejectedRetention is how long failed-events/rejected/ keeps an event a
	// replay gave up on. Zero never deletes.
	//
	// Those files are the evidence of what logservice refused, so keeping them
	// is deliberate — but nothing else drains that directory, and unlike dead/
	// no CLI lists it without -all. Thirty days is long enough to investigate a
	// schema mismatch and short enough that an unattended gateway does not fill
	// its disk with it.
	//
	// The sweep rides the replay pass, so replay_interval: 0 disables BOTH.
	// startReplayer warns rather than leaving this key silently inert.
	RejectedRetention Duration `json:"rejected_retention,omitempty"`
}

// Server is the parsed server.yaml.
type Server struct {
	Hostname     string     `json:"hostname"`
	Greeting     string     `json:"greeting,omitempty"`
	LocalDomains []string   `json:"local_domains,omitempty"`
	Listen       []Listener `json:"listen"`
	TLS          TLSConfig  `json:"tls"`
	Max          Limits     `json:"max"`
	SMTPUTF8     bool       `json:"smtputf8"`
	Inactivity   Duration   `json:"inactivity_timeout"`
	// ShutdownTimeout bounds the WHOLE teardown — draining SMTP sessions,
	// finishing the delivery attempt in flight, and flushing the audit events
	// behind them — not any one step of it.
	//
	// The container's stop grace period must exceed this or the ordering is
	// moot: the runtime SIGKILLs part-way through and the careful sequence buys
	// nothing. Both compose files set stop_grace_period accordingly.
	ShutdownTimeout Duration      `json:"shutdown_timeout"`
	Log             LogConfig     `json:"log"`
	Outbound        Outbound      `json:"outbound"`
	DSN             DSNConfig     `json:"dsn"`
	Attach          AttachConfig  `json:"attach"`
	MsgAuth         MsgAuthConfig `json:"msgauth"`
	RateLimit       RateLimits    `json:"ratelimit"`
	Events          Events        `json:"events"`
}

// Logging is the existing logging.json, plus the credential a centrally-managed
// gateway cannot get any other way.
type Logging struct {
	URLConn     string `json:"url_conn"`
	URLQueue    string `json:"url_queue"`
	URLDelivery string `json:"url_delivery"`

	// APIKey is sent to logservice as X-API-Key. A managed node has no
	// environment to read it from — that is the whole point of it — so Central
	// Management delivers it in the bundle. In file mode it is optional and
	// events.api_key_env remains the way to supply it; when both are set this
	// one wins, because an explicit value beats a name to look up.
	APIKey string `json:"api_key,omitempty"`
}

// Admin configures the local admin listener — the wizard, the status page and
// the three observability endpoints.
//
// It is separate from Server because it is the management plane rather than the
// mail path: nothing in here reaches an SMTP session, a rule or a delivery, and
// keeping it out of Server is also what keeps bundle_test's "a bundle and a
// directory produce the same Server" assertion meaningful.
type Admin struct {
	// MetricsToken is the bearer token /metrics and /readyz require. Empty
	// leaves both open, which is what every deployment that firewalled port
	// 8080 already has.
	//
	// A scraper needs a credential it can present without a browser, which is
	// why this exists at all rather than reusing the session the wizard runs on.
	MetricsToken string `json:"metrics_token,omitempty"`
}

// Auth holds the inbound SMTP AUTH credentials — auth.json, or the bundle's
// `auth` key.
//
// Separate from Server for the reason Admin is: the console composes it from
// rows in a table, not from the raw server.yaml text a `server` profile carries,
// so it has to be its own bundle key. An empty or absent Auth means AUTH is
// never advertised, which is what every deployment before M13 had.
type Auth struct {
	Users []AuthUser `json:"users,omitempty"`
}

// AuthUser is one submission credential.
//
// Hash, not password. The gateway is a VERIFIER — unlike relays.Relay, which
// has to present its credential to somebody else and therefore needs the
// plaintext back — so nothing here is reversible and the console's
// CONFIG_SECRET_KEY is not involved at all. A leaked bundle costs an offline
// bcrypt attack rather than a working password.
type AuthUser struct {
	User string `json:"user"`
	Hash string `json:"hash"`
}

// Lookup returns the stored hash for a username, and whether it exists.
//
// Usernames are compared case-sensitively: a submission credential is an opaque
// string an operator issued, not a mailbox, and folding case here would silently
// merge two rows the console's unique index considers distinct.
func (a *Auth) Lookup(user string) (string, bool) {
	if a == nil {
		return "", false
	}
	for _, u := range a.Users {
		if u.User == user {
			return u.Hash, true
		}
	}
	return "", false
}

// Enabled reports whether any credential exists. AUTH is advertised only then —
// offering a mechanism no credential can satisfy is an invitation to guess.
func (a *Auth) Enabled() bool { return a != nil && len(a.Users) > 0 }

// Config is everything loaded from the config directory.
type Config struct {
	Dir       string
	Server    Server
	Logging   Logging
	Admin     Admin
	Auth      Auth
	Relays    *relays.Table
	Allowlist *Allowlist
}

// DefaultSpoolDir is the compiled-in outbound spool location.
//
// Exported so a centrally-managed gateway can tell "the operator chose this"
// from "nobody said anything": a bundle with no server profile lands here, and
// a managed node given -data /var/lib/mailgw-go usually cannot write it.
const DefaultSpoolDir = "/opt/mailgw-go/queue"

// DefaultShutdownTimeout is the compiled-in teardown budget.
//
// Ten seconds because that is also Docker's default stop grace period, so a
// gateway that has never been told otherwise finishes just inside it. Raising
// this without raising stop_grace_period achieves nothing.
const DefaultShutdownTimeout = 10 * time.Second

// defaults fills every field that has a sensible non-zero default. Values are
// taken from mailgw/config/connection.ini where an equivalent exists.
func defaults() Server {
	return Server{
		Hostname: "localhost",
		Listen:   []Listener{{Addr: "0.0.0.0:2525"}},
		// Opt-out, not opt-in — but still inert without a keypair, so a
		// configuration that says nothing about TLS behaves exactly as it always
		// has.
		TLS: TLSConfig{STARTTLS: true},
		Max: Limits{
			Bytes:           26214400, // [max] bytes
			LineLength:      512,      // [max] line_length
			Recipients:      100,
			ReceivedHeaders: 100,  // [headers] max_received
			HeaderLines:     1000, // [headers] max_lines
			// High enough that nobody meets it by accident. A busy relay runs a
			// few dozen concurrent sessions; this is a ceiling on file
			// descriptors, not a throttle to tune.
			Connections: 1024,
		},
		SMTPUTF8:        true, // [main] smtputf8
		Inactivity:      Duration(300 * time.Second),
		ShutdownTimeout: Duration(DefaultShutdownTimeout),
		Log:             LogConfig{Level: "info", Format: "json"},
		Outbound: Outbound{
			Concurrency:         10,
			PerGroupConnections: 5,
			PollInterval:        Duration(5 * time.Second),
			SpoolDir:            DefaultSpoolDir,
			Backoff: []Duration{
				Duration(60 * time.Second),
				Duration(5 * time.Minute),
				Duration(15 * time.Minute),
				Duration(30 * time.Minute),
				Duration(time.Hour),
				Duration(2 * time.Hour),
				Duration(4 * time.Hour),
				Duration(8 * time.Hour),
			},
			MaxLifetime:       Duration(96 * time.Hour),
			DelayWarningAfter: Duration(4 * time.Hour),
			Jitter:            0.15,
			ConnectTimeout:    Duration(30 * time.Second),
			DataTimeout:       Duration(10 * time.Minute),
			// Only consulted when reuse_connections is on, but defaulted here so
			// enabling it is a one-line change rather than three.
			MaxMessagesPerConnection: 50,
			ConnectionIdleTimeout:    Duration(30 * time.Second),
			MXCacheTTL:               Duration(5 * time.Minute),
		},
		DSN: DSNConfig{Enabled: true, Return: "headers", MaxReturnBytes: 256 * 1024},
		Attach: AttachConfig{
			Fail: "closed", IncludeInline: true,
			OnBlock: AttachBlockReject, Timeout: Duration(3 * time.Second),
		},
		// Every check ships OFF. Turning one on costs DNS per message and, for
		// DKIM, a second pass over the spooled body — the same treatment
		// attach.enabled and outbound.reuse_connections get, for the same
		// reason: it changes what every message costs.
		MsgAuth: MsgAuthConfig{
			MaxDKIMSignatures: 10,
			DNSTimeout:        Duration(5 * time.Second),
			Sign:              DKIMSignConfig{Canonicalization: "relaxed/relaxed"},
		},
		// RateLimit is deliberately ABSENT here, not forgotten. Every rate limit
		// is off, and off is the zero value — there is no default rate a mail
		// gateway can pick on an operator's behalf without guessing at their
		// traffic. max_keys defaults inside internal/ratelimit, where the memory
		// argument for the number lives.
		Events: Events{
			Timeout:           Duration(3 * time.Second),
			Retries:           3,
			BufferSize:        4096,
			Senders:           4,
			APIKeyEnv:         "API_KEY",
			ReplayInterval:    Duration(5 * time.Minute),
			RejectedRetention: Duration(720 * time.Hour), // 30 days
		},
	}
}

// ParseServer builds a Server from server.yaml bytes: the defaults, the file
// unmarshalled over them, then validation. name appears only in error messages.
//
// strict rejects unknown keys. It is used only on the central path: that text
// came from a console textarea an operator typed into, which is exactly the
// case a silently-ignored typo should be caught in. The file path stays lax so
// an existing deployment carrying a stray key keeps booting — which is also why
// the two paths can disagree, and why `check` says which one it used.
func ParseServer(raw []byte, name string, strict bool) (Server, error) {
	s := defaults()

	unmarshal := yaml.Unmarshal
	if strict {
		unmarshal = yaml.UnmarshalStrict
	}
	if err := unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("parse %s: %w", name, err)
	}
	if err := s.validate(); err != nil {
		return s, err
	}
	return s, nil
}

// Load reads and validates the whole config directory.
func Load(dir string) (*Config, error) {
	cfg := &Config{Dir: dir, Server: defaults()}

	// server.yaml is optional; the defaults above are a working configuration
	// for everything except the relay table.
	serverPath := filepath.Join(dir, FileServer)
	if raw, err := os.ReadFile(serverPath); err == nil {
		srv, perr := ParseServer(raw, serverPath, false)
		if perr != nil {
			return nil, perr
		}
		cfg.Server = srv
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", serverPath, err)
	}

	loggingPath := filepath.Join(dir, FileLogging)
	if raw, err := os.ReadFile(loggingPath); err == nil {
		if err := json.Unmarshal(raw, &cfg.Logging); err != nil {
			return nil, fmt.Errorf("parse %s: %w", loggingPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", loggingPath, err)
	}

	// Optional, like logging.json above: a directory that predates the admin
	// token is a valid directory, and no token means the endpoints stay open.
	adminPath := filepath.Join(dir, FileAdmin)
	if raw, err := os.ReadFile(adminPath); err == nil {
		if err := json.Unmarshal(raw, &cfg.Admin); err != nil {
			return nil, fmt.Errorf("parse %s: %w", adminPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", adminPath, err)
	}

	// Optional for the same reason: a directory that predates inbound AUTH is a
	// valid directory, and no credentials means AUTH is never offered.
	authPath := filepath.Join(dir, FileAuth)
	if raw, err := os.ReadFile(authPath); err == nil {
		if err := json.Unmarshal(raw, &cfg.Auth); err != nil {
			return nil, fmt.Errorf("parse %s: %w", authPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", authPath, err)
	}

	tbl, err := relays.Load(filepath.Join(dir, FileRelays))
	if err != nil {
		return nil, err
	}
	cfg.Relays = tbl

	// A failed allowlist load is fatal: this is the only gate protecting the
	// relay, so refusing to start is the correct fail-closed response.
	list, err := LoadAllowlist(filepath.Join(dir, FileFilter))
	if err != nil {
		return nil, err
	}
	cfg.Allowlist = list

	if err := cfg.Server.validate(); err != nil {
		return nil, err
	}
	if err := cfg.validateRelayRefs(); err != nil {
		return nil, err
	}
	if err := cfg.validateAuth(); err != nil {
		return nil, err
	}
	if err := cfg.validateDKIM(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validateRelayRefs checks the parts of server.yaml that name a relay group.
//
// Separate from Server.validate because it needs the relay table, which is a
// different file. Both configuration sources call it, so a `check` that passes
// genuinely predicts a gateway that can deliver its own bounces — a group named
// here and misspelled would otherwise surface only the first time a message
// failed, which is the worst possible moment to discover it.
func (c *Config) validateRelayRefs() error {
	g := strings.TrimSpace(c.Server.DSN.RelayGroup)
	if g == "" || !c.Server.DSN.Enabled {
		return nil
	}
	if !c.Relays.Has(g) {
		return fmt.Errorf("%s: dsn.relay_group %q is not a relay group (have: %s)",
			FileServer, g, strings.Join(c.Relays.Names(), ", "))
	}
	return nil
}

// validateAuth checks the inbound credentials.
//
// Separate from Server.validate for the same reason validateRelayRefs is: it
// reads a different file, and both configuration sources call it so that `check`
// predicts what the gateway will do. Every failure here is fatal rather than a
// warning — a credential the gateway cannot parse is one that rejects a correct
// password at three in the morning, and the console has no way to find out.
// validateDKIM reads and parses every configured signing key.
//
// Separate from Server.validate for the same reason validateRelayRefs and
// validateAuth are: it touches the filesystem rather than the parsed document,
// and both configuration sources call it so `check` predicts what the gateway
// will actually do. It is fatal rather than a warning — a key the gateway cannot
// read is one whose mail goes out unsigned, and unsigned mail from a domain
// publishing DMARC is mail that gets quarantined at the far end while every log
// line here says "delivered".
//
// It is also what makes the key path a real contract: the bundle can name a file
// the gateway does not have, and this is where that is found out.
func (c *Config) validateDKIM() error {
	s := c.Server.MsgAuth.Sign
	if !s.Enabled {
		return nil
	}
	keys := make([]msgauth.Key, 0, len(s.Keys))
	for i, k := range s.Keys {
		if strings.TrimSpace(k.Domain) == "" {
			return fmt.Errorf("%s: msgauth.sign.keys[%d]: 'domain' is required", FileServer, i)
		}
		if strings.TrimSpace(k.Selector) == "" {
			return fmt.Errorf("%s: msgauth.sign.keys[%d] (%s): 'selector' is required",
				FileServer, i, k.Domain)
		}
		if strings.TrimSpace(k.Key) == "" {
			return fmt.Errorf("%s: msgauth.sign.keys[%d] (%s): 'key' is required and is a "+
				"PATH on this host — a private key never travels in a configuration bundle",
				FileServer, i, k.Domain)
		}
		keys = append(keys, msgauth.Key{Domain: k.Domain, Selector: k.Selector, Path: k.Key})
	}
	if _, err := msgauth.NewKeys(keys); err != nil {
		return fmt.Errorf("%s: msgauth.sign.keys: %w", FileServer, err)
	}
	return nil
}

func (c *Config) validateAuth() error {
	seen := make(map[string]struct{}, len(c.Auth.Users))
	for i, u := range c.Auth.Users {
		if strings.TrimSpace(u.User) == "" {
			return fmt.Errorf("%s: users[%d]: 'user' is required", FileAuth, i)
		}
		if _, dup := seen[u.User]; dup {
			return fmt.Errorf("%s: users[%d]: duplicate user %q", FileAuth, i, u.User)
		}
		seen[u.User] = struct{}{}

		if strings.TrimSpace(u.Hash) == "" {
			return fmt.Errorf("%s: users[%d] (%s): 'hash' is required — this field holds a "+
				"bcrypt hash, never a password", FileAuth, i, u.User)
		}
		// Rejected here rather than at AUTH time. bcrypt.Cost parses the hash's
		// own prefix, so this catches a password pasted where a hash belongs —
		// the one mistake that would otherwise look like a working configuration
		// that simply never lets anybody in.
		if _, err := bcrypt.Cost([]byte(u.Hash)); err != nil {
			return fmt.Errorf("%s: users[%d] (%s): 'hash' is not a bcrypt hash: %w",
				FileAuth, i, u.User, err)
		}
	}
	return nil
}

func (s *Server) validate() error {
	if strings.TrimSpace(s.Hostname) == "" {
		return fmt.Errorf("%s: 'hostname' is required", FileServer)
	}
	if len(s.Listen) == 0 {
		return fmt.Errorf("%s: at least one 'listen' entry is required", FileServer)
	}
	for i, l := range s.Listen {
		if strings.TrimSpace(l.Addr) == "" {
			return fmt.Errorf("%s: listen[%d]: 'addr' is required", FileServer, i)
		}
		if l.ImplicitTLS && !s.TLS.configured() {
			return fmt.Errorf("%s: listen[%d]: implicit_tls needs tls.cert and tls.key", FileServer, i)
		}
		if l.ProxyProtocol {
			// An empty list is refused rather than treated as "trust nobody":
			// silently dropping every connection on a listener the operator just
			// turned on is indistinguishable from the gateway being down.
			if len(l.ProxyTrusted) == 0 {
				return fmt.Errorf("%s: listen[%d]: proxy_protocol needs a non-empty "+
					"proxy_trusted — a PROXY header is trivially forged, so it is only "+
					"honoured from named peers", FileServer, i)
			}
			if _, err := ParsePrefixes(l.ProxyTrusted); err != nil {
				return fmt.Errorf("%s: listen[%d]: proxy_trusted: %w", FileServer, i, err)
			}
		} else if len(l.ProxyTrusted) > 0 {
			// Same reasoning as max.received_headers: a key that looks like it
			// does something and does nothing is worse than an absent one.
			return fmt.Errorf("%s: listen[%d]: proxy_trusted has no effect without "+
				"proxy_protocol", FileServer, i)
		}
	}
	// tls.starttls is deliberately NOT validated against the keypair the way
	// implicit_tls is: it defaults to true, so requiring a certificate for it
	// would reject every configuration that never mentions TLS. Without a
	// keypair it is simply inert. implicit_tls stays an error because a listener
	// that cannot complete a handshake accepts connections and then fails every
	// one of them, which looks healthy from outside.
	if s.Max.Bytes <= 0 {
		// go-smtp only advertises SIZE when MaxMessageBytes > 0, and
		// tests/smtp asserts SIZE is advertised.
		return fmt.Errorf("%s: max.bytes must be positive (SIZE is advertised from it)", FileServer)
	}
	// Feeds ReadTimeout and WriteTimeout straight onto the SMTP server, where a
	// zero means "no deadline" — an unbounded slowloris. The default is 300s, and
	// a bundle is not a file an operator proofreads, so an explicit 0 arriving
	// from the console has to be refused rather than applied.
	if s.Inactivity <= 0 {
		return fmt.Errorf("%s: inactivity_timeout must be positive", FileServer)
	}
	// Same reasoning as max.bytes and inactivity_timeout: ParseServer starts
	// from defaults() and unmarshals over it, so an explicit 0 overwrites 1024
	// and would mean "no cap" — which is the state this key exists to end. A
	// deployment that genuinely wants no ceiling sets a very large number, and
	// says so on purpose.
	if s.Max.Connections <= 0 {
		return fmt.Errorf("%s: max.connections must be positive "+
			"(set it high rather than 0 to lift the cap)", FileServer)
	}
	if s.Outbound.Concurrency < 1 {
		return fmt.Errorf("%s: outbound.concurrency must be at least 1", FileServer)
	}
	if len(s.Outbound.Backoff) == 0 {
		return fmt.Errorf("%s: outbound.backoff must have at least one entry", FileServer)
	}
	if s.Outbound.Jitter < 0 || s.Outbound.Jitter >= 1 {
		return fmt.Errorf("%s: outbound.jitter must be in [0,1)", FileServer)
	}
	// An explicit zero disables all three eviction paths at once — Get's lazy
	// check, the sweep and the reaper — so pooled sockets would then be held
	// until the process exits, and the reaper would exit without saying so. The
	// key that looks like the ceiling would be the thing removing it; same
	// reasoning as max.connections and attach.timeout.
	if s.Outbound.ReuseConnections && s.Outbound.ConnectionIdleTimeout <= 0 {
		return fmt.Errorf("%s: outbound.connection_idle_timeout must be positive "+
			"when outbound.reuse_connections is set", FileServer)
	}
	if strings.TrimSpace(s.Outbound.SpoolDir) == "" {
		return fmt.Errorf("%s: outbound.spool_dir is required", FileServer)
	}
	switch strings.ToLower(s.Attach.Fail) {
	case "", "closed", "open":
	default:
		return fmt.Errorf("%s: attach.fail must be \"closed\" or \"open\"", FileServer)
	}
	if s.Attach.Enabled && strings.TrimSpace(s.Attach.URL) == "" {
		return fmt.Errorf("%s: attach.enabled needs attach.url", FileServer)
	}
	// An explicit zero makes both the context deadline and the HTTP client's own
	// timeout no-ops, and the scan runs inside the DATA reply — so it is not a
	// slow scanner, it is a session that never answers. The default is 3s;
	// writing 0 has to be an error rather than a way to disable the bound.
	if s.Attach.Enabled && s.Attach.Timeout <= 0 {
		return fmt.Errorf("%s: attach.timeout must be positive when attach.enabled", FileServer)
	}
	switch strings.ToLower(s.Attach.OnBlock) {
	case "", AttachBlockReject, AttachBlockTempfail, AttachBlockQuarantine, AttachBlockDiscard:
	default:
		return fmt.Errorf("%s: attach.on_block must be one of %s, %s, %s, %s", FileServer,
			AttachBlockReject, AttachBlockTempfail, AttachBlockQuarantine, AttachBlockDiscard)
	}
	switch strings.ToLower(s.DSN.Return) {
	case "", "headers", "full":
	default:
		return fmt.Errorf("%s: dsn.return must be \"headers\" or \"full\"", FileServer)
	}
	// Zero is the documented off switch for both. Negative is not: it parses
	// (Duration accepts "-720h"), it reads as "disabled" everywhere downstream,
	// and it produces no warning at all — so the gateway would silently never
	// replay and never sweep, which is exactly the failure mode these checks
	// exist to prevent elsewhere.
	if s.Events.ReplayInterval < 0 {
		return fmt.Errorf("%s: events.replay_interval cannot be negative "+
			"(0 disables the background pass)", FileServer)
	}
	if s.Events.RejectedRetention < 0 {
		return fmt.Errorf("%s: events.rejected_retention cannot be negative "+
			"(0 keeps rejected events for ever)", FileServer)
	}
	if err := s.MsgAuth.validate(); err != nil {
		return err
	}
	if err := s.RateLimit.validate(); err != nil {
		return err
	}
	return nil
}

func (m MsgAuthConfig) validate() error {
	// Same rule as max.connections and attach.timeout: ParseServer unmarshals
	// over defaults(), so an explicit 0 overwrites the default and means "no
	// bound" — which is the state the key exists to end. A bundle is not a file
	// an operator proofreads, so this has to be an error and not a fallback.
	if m.Wants() && m.DNSTimeout <= 0 {
		return fmt.Errorf("%s: msgauth.dns_timeout must be positive when any msgauth "+
			"check is enabled (set it high rather than 0)", FileServer)
	}
	if m.DKIM.Enabled && m.MaxDKIMSignatures <= 0 {
		return fmt.Errorf("%s: msgauth.max_dkim_signatures must be positive when "+
			"msgauth.dkim.enabled (each signature costs a DNS lookup and a key verification)",
			FileServer)
	}
	// DMARC is alignment over an SPF result and a DKIM result. It is legal to
	// enable it alone — a rule reading dmarc.* turns the other two on by itself,
	// see Ruleset.NeedsDMARC — but a configuration that names dmarc and nothing
	// else has almost certainly not understood that, and every message would
	// answer "fail". Say so rather than let it ship.
	if m.DMARC.Enabled && !m.SPF.Enabled && !m.DKIM.Enabled {
		return fmt.Errorf("%s: msgauth.dmarc.enabled needs msgauth.spf.enabled or "+
			"msgauth.dkim.enabled — DMARC is alignment over their results, so on its "+
			"own it would fail every message", FileServer)
	}

	if strings.ContainsAny(m.AuthservID, " \t;\r\n") {
		return fmt.Errorf("%s: msgauth.authserv_id must be a single token "+
			"(it is the first field of every Authentication-Results header)", FileServer)
	}

	if !m.Sign.Enabled {
		return nil
	}
	if len(m.Sign.Keys) == 0 {
		return fmt.Errorf("%s: msgauth.sign.enabled needs at least one msgauth.sign.keys entry", FileServer)
	}
	if m.Sign.Expiration < 0 {
		return fmt.Errorf("%s: msgauth.sign.expiration cannot be negative "+
			"(0 means the signature does not expire)", FileServer)
	}
	if _, _, err := msgauth.ParseCanonicalization(m.Sign.Canonicalization); err != nil {
		return fmt.Errorf("%s: msgauth.sign.canonicalization: %w", FileServer, err)
	}
	if len(m.Sign.Headers) > 0 && !containsFold(m.Sign.Headers, "From") {
		// RFC 6376 §5.4 requires it, and go-msgauth refuses to build a signer
		// without it — caught here so the failure is a configuration error at
		// bring-up rather than every message going unsigned at delivery.
		return fmt.Errorf("%s: msgauth.sign.headers must include From", FileServer)
	}
	return nil
}

func containsFold(ss []string, want string) bool {
	for _, s := range ss {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}

func (t TLSConfig) configured() bool { return t.Cert != "" && t.Key != "" }

// ImplicitTLSWanted reports whether any listener wraps its socket in TLS from
// the first byte, which needs a certificate even when starttls is off.
func (s *Server) ImplicitTLSWanted() bool {
	for _, l := range s.Listen {
		if l.ImplicitTLS {
			return true
		}
	}
	return false
}

// WantsTLS reports whether this configuration would use a certificate if it had
// one. It is what decides whether a managed node bothers generating a
// self-signed pair.
func (s *Server) WantsTLS() bool { return s.TLS.STARTTLS || s.ImplicitTLSWanted() }

// ConfiguredPublic reports whether a certificate pair is available, for callers
// outside this package.
func (t TLSConfig) ConfiguredPublic() bool { return t.configured() }

// What attach.on_block may say.
const (
	AttachBlockReject     = "reject"
	AttachBlockTempfail   = "tempfail"
	AttachBlockQuarantine = "quarantine"
	AttachBlockDiscard    = "discard"
)

// FailClosed reports whether an attachment-scan error should block the message.
func (a AttachConfig) FailClosed() bool { return !strings.EqualFold(a.Fail, "open") }

// BlockAction normalises on_block, so an unset value and an explicit "reject"
// take the same code path. Validation has already rejected anything else.
func (a AttachConfig) BlockAction() string {
	if v := strings.ToLower(strings.TrimSpace(a.OnBlock)); v != "" {
		return v
	}
	return AttachBlockReject
}

// Backoff returns the delay before attempt n (1-based); the final entry repeats.
func (o Outbound) BackoffFor(attempt int) time.Duration {
	if len(o.Backoff) == 0 {
		return time.Minute
	}
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(o.Backoff) {
		i = len(o.Backoff) - 1
	}
	return o.Backoff[i].D()
}
