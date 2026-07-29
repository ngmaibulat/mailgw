package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

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
}

// TLSConfig points at the certificate pair. Both fields empty disables TLS.
type TLSConfig struct {
	Cert     string `json:"cert,omitempty"`
	Key      string `json:"key,omitempty"`
	STARTTLS bool   `json:"starttls,omitempty"`
}

// Limits mirror the [max] and [headers] sections of Haraka's connection.ini.
type Limits struct {
	Bytes           int64 `json:"bytes"`
	LineLength      int   `json:"line_length"`
	Recipients      int   `json:"recipients"`
	ReceivedHeaders int   `json:"received_headers"`
	HeaderLines     int   `json:"header_lines"`
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
}

// DSNConfig controls bounce generation.
type DSNConfig struct {
	Enabled    bool   `json:"enabled"`
	Return     string `json:"return"` // headers | full
	Postmaster string `json:"postmaster"`
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
}

// Events configures the logservice event pipeline.
type Events struct {
	// URLs come from logging.json; these tune the transport.
	Timeout    Duration `json:"timeout"`
	Retries    int      `json:"retries"`
	BufferSize int      `json:"buffer_size"`
	Senders    int      `json:"senders"`
	APIKeyEnv  string   `json:"api_key_env"`
}

// Server is the parsed server.yaml.
type Server struct {
	Hostname     string       `json:"hostname"`
	Greeting     string       `json:"greeting,omitempty"`
	LocalDomains []string     `json:"local_domains,omitempty"`
	Listen       []Listener   `json:"listen"`
	TLS          TLSConfig    `json:"tls"`
	Max          Limits       `json:"max"`
	SMTPUTF8     bool         `json:"smtputf8"`
	Inactivity   Duration     `json:"inactivity_timeout"`
	Log          LogConfig    `json:"log"`
	Outbound     Outbound     `json:"outbound"`
	DSN          DSNConfig    `json:"dsn"`
	Attach       AttachConfig `json:"attach"`
	Events       Events       `json:"events"`
}

// Logging is the existing logging.json, unchanged.
type Logging struct {
	URLConn     string `json:"url_conn"`
	URLQueue    string `json:"url_queue"`
	URLDelivery string `json:"url_delivery"`
}

// Config is everything loaded from the config directory.
type Config struct {
	Dir       string
	Server    Server
	Logging   Logging
	Relays    *relays.Table
	Allowlist *Allowlist
}

// defaults fills every field that has a sensible non-zero default. Values are
// taken from mailgw/config/connection.ini where an equivalent exists.
func defaults() Server {
	return Server{
		Hostname: "localhost",
		Listen:   []Listener{{Addr: "0.0.0.0:2525"}},
		Max: Limits{
			Bytes:           26214400, // [max] bytes
			LineLength:      512,      // [max] line_length
			Recipients:      100,
			ReceivedHeaders: 100,  // [headers] max_received
			HeaderLines:     1000, // [headers] max_lines
		},
		SMTPUTF8:   true, // [main] smtputf8
		Inactivity: Duration(300 * time.Second),
		Log:        LogConfig{Level: "info", Format: "json"},
		Outbound: Outbound{
			Concurrency:         10,
			PerGroupConnections: 5,
			PollInterval:        Duration(5 * time.Second),
			SpoolDir:            "/opt/mailgw-go/queue",
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
		},
		DSN:    DSNConfig{Enabled: true, Return: "headers"},
		Attach: AttachConfig{Fail: "closed", IncludeInline: true, Timeout: Duration(3 * time.Second)},
		Events: Events{
			Timeout:    Duration(3 * time.Second),
			Retries:    3,
			BufferSize: 4096,
			Senders:    4,
			APIKeyEnv:  "API_KEY",
		},
	}
}

// Load reads and validates the whole config directory.
func Load(dir string) (*Config, error) {
	cfg := &Config{Dir: dir, Server: defaults()}

	// server.yaml is optional; the defaults above are a working configuration
	// for everything except the relay table.
	serverPath := filepath.Join(dir, FileServer)
	if raw, err := os.ReadFile(serverPath); err == nil {
		if err := yaml.Unmarshal(raw, &cfg.Server); err != nil {
			return nil, fmt.Errorf("parse %s: %w", serverPath, err)
		}
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
	return cfg, nil
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
	}
	if s.TLS.STARTTLS && !s.TLS.configured() {
		return fmt.Errorf("%s: tls.starttls needs tls.cert and tls.key", FileServer)
	}
	if s.Max.Bytes <= 0 {
		// go-smtp only advertises SIZE when MaxMessageBytes > 0, and
		// tests/smtp asserts SIZE is advertised.
		return fmt.Errorf("%s: max.bytes must be positive (SIZE is advertised from it)", FileServer)
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
	switch strings.ToLower(s.DSN.Return) {
	case "", "headers", "full":
	default:
		return fmt.Errorf("%s: dsn.return must be \"headers\" or \"full\"", FileServer)
	}
	return nil
}

func (t TLSConfig) configured() bool { return t.Cert != "" && t.Key != "" }

// ConfiguredPublic reports whether a certificate pair is available, for callers
// outside this package.
func (t TLSConfig) ConfiguredPublic() bool { return t.configured() }

// FailClosed reports whether an attachment-scan error should block the message.
func (a AttachConfig) FailClosed() bool { return !strings.EqualFold(a.Fail, "open") }

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
