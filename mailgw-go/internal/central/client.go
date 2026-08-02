// Package central is the gateway's client for the Central Management machine
// API served by webui-fastify at /agent/*.
//
// # Identity
//
// There are no enrollment tokens and no shared secrets. The gateway generates
// an Ed25519 keypair on first boot (see internal/store), registers the public
// half, and signs every request thereafter. Registration is open — anything
// that can reach the console may ask to join — so the key alone grants nothing:
// the gateway lands `pending` until an operator approves its fingerprint.
//
// # Signing
//
// Every request carries X-GW-Timestamp and X-GW-Signature, and every request
// except register also carries X-GW-Id. The signature is over:
//
//	<METHOD>\n<request-target>\n<unix-seconds>\n<sha256-hex of the raw body>
//
// The console verifies exactly this string (webui-fastify/src/agent/verify.ts).
// Two details there are easy to get wrong and produce a bare 401 with no other
// clue; both are enforced in do() and explained at sign().
package central

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds a single call to the console. The mail path never waits
// on management, so this only has to be short enough to keep the poll loop
// responsive.
const DefaultTimeout = 10 * time.Second

// maxErrorBody caps how much of a non-2xx body is kept for the error message.
// A misconfigured URL can return a whole HTML page.
const maxErrorBody = 2048

// maxResponseBody caps a whole console response.
//
// A bundle is five config-file texts and a relay table; the largest real one is
// measured in kilobytes. Eight mebibytes is a size a working deployment cannot
// approach and a broken or hostile console cannot exceed without being told so
// by name.
const maxResponseBody = 8 << 20

// Client talks to one Central Management console.
type Client struct {
	// BaseURL is the console root, e.g. https://console.example:4000. A path
	// prefix is honoured and signed; a reverse proxy that REWRITES the path
	// between here and the console would invalidate every signature.
	BaseURL string
	// ID is the gateway_uid assigned at registration. Empty for Register.
	ID  string
	Key ed25519.PrivateKey

	HTTP *http.Client
	Log  *slog.Logger

	// now is a seam for tests; the clock-skew path is otherwise only
	// reachable by changing the system clock. Mirrors events.Options.now.
	now func() time.Time
}

// NewHTTPClient builds the transport used to reach the console.
//
// The console serves HTTP/2 over TLS with, in the shipped configuration, a
// self-signed certificate from the certs/ project — so one of caFile or
// insecure is usually required for registration to work at all. Go's default
// transport negotiates h2 via ALPN, so nothing extra is needed for HTTP/2.
func NewHTTPClient(insecure bool, caFile string, timeout time.Duration) (*http.Client, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA bundle %s contains no certificates", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	// Checked after the CA bundle so that supplying both is not silently
	// weaker than supplying only the bundle.
	if insecure {
		tlsCfg.InsecureSkipVerify = true
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.ForceAttemptHTTP2 = true

	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// SystemInfo is what a gateway declares about itself at registration.
//
// It is display and diagnostic data only — the console never gates a decision
// on it. Field limits mirror webui-fastify/src/validation/agent.ts, which
// answers 400 rather than truncating: every field is optional but none is
// nullable, so each needs omitempty.
type SystemInfo struct {
	Hostname string   `json:"hostname,omitempty"`
	OS       string   `json:"os,omitempty"`
	Arch     string   `json:"arch,omitempty"`
	CPUs     int      `json:"cpus,omitempty"`
	MemBytes int64    `json:"mem_bytes,omitempty"`
	IPAddrs  []string `json:"ip_addrs,omitempty"`
	Version  string   `json:"version,omitempty"`
}

type registerRequest struct {
	PubKey string `json:"pubkey"`
	SystemInfo
}

// RegisterResponse is the answer to a registration attempt.
type RegisterResponse struct {
	GatewayUID  string `json:"gateway_uid"`
	Fingerprint string `json:"fingerprint"`
	Approval    string `json:"approval"`
	// New reports whether this created a row (HTTP 201) or matched an existing
	// fingerprint (HTTP 200). Both are success.
	New bool `json:"-"`
}

// StatusResponse is the cheap poll. The console answers a pending gateway too —
// that is how it learns it is still waiting.
type StatusResponse struct {
	Approval         string  `json:"approval"`
	DesiredVersionID *int64  `json:"desired_version_id"`
	DesiredVersion   *int    `json:"desired_version"`
	BundleSHA256     *string `json:"bundle_sha256"`
	AppliedVersionID *int64  `json:"applied_version_id"`
}

// ConfigResponse carries one immutable configuration version.
type ConfigResponse struct {
	VersionID    int64           `json:"version_id"`
	Version      int             `json:"version"`
	BundleSHA256 string          `json:"bundle_sha256"`
	Bundle       json.RawMessage `json:"bundle"`
}

// Report is what the gateway is actually running, and why not, if not.
//
// It is a full state snapshot, not a patch. AppliedVersionID and ApplyError
// deliberately lack omitempty: the console merges on `!== undefined`
// (webui-fastify/src/routes/agent.ts), so a nil pointer has to reach it as an
// explicit null in order to CLEAR a value. With omitempty a stale apply_error
// could never be cleared and would haunt the console for the life of the row.
type Report struct {
	AppliedVersionID *int64  `json:"applied_version_id"`
	ApplyError       *string `json:"apply_error"`

	RestartRequired *bool            `json:"restart_required,omitempty"`
	Version         string           `json:"version,omitempty"`
	Metrics         map[string]int64 `json:"metrics,omitempty"`
}

// Register presents the public key and asks to join. It is signed with the
// matching private key but carries no X-GW-Id: the console verifies against the
// key in the body, which proves possession without granting anything.
//
// It is idempotent per fingerprint and can never reset an existing approval, so
// calling it again after a restart is safe.
func (c *Client) Register(ctx context.Context, pub ed25519.PublicKey, info SystemInfo) (*RegisterResponse, error) {
	body := registerRequest{
		PubKey:     base64.StdEncoding.EncodeToString(pub),
		SystemInfo: info,
	}
	var out RegisterResponse
	status, err := c.do(ctx, "register", http.MethodPost, "/agent/register", body, &out)
	if err != nil {
		return nil, err
	}
	out.New = status == http.StatusCreated
	return &out, nil
}

// Status reports approval and which configuration version the console wants
// this gateway to be running.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var out StatusResponse
	if _, err := c.do(ctx, "status", http.MethodGet, "/agent/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Config fetches the deployed bundle.
//
// This is the one route approval gates. Callers should expect ErrNotApproved
// and ErrNoConfig as ordinary states rather than faults.
func (c *Client) Config(ctx context.Context) (*ConfigResponse, error) {
	var out ConfigResponse
	if _, err := c.do(ctx, "config", http.MethodGet, "/agent/config", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Report posts the gateway's current state.
func (c *Client) Report(ctx context.Context, r Report) error {
	_, err := c.do(ctx, "report", http.MethodPost, "/agent/report", r, nil)
	return err
}

// sign builds the canonical string the console verifies and returns the
// headers for it.
//
//	<METHOD>\n<target>\n<unix-seconds>\n<sha256-hex of body>
//
// target must be the request target exactly as it goes on the wire —
// u.RequestURI(), which includes the /agent prefix, any BaseURL path prefix and
// any query string. The console signs Fastify's request.url, which is the raw
// request line.
//
// The timestamp is formatted once and both signed and sent: the console signs
// the raw header text, not a reparsed number, so any reformatting between the
// two breaks verification.
func (c *Client) sign(method, target string, body []byte) http.Header {
	ts := strconv.FormatInt(c.clock().Unix(), 10)
	sum := sha256.Sum256(body)
	canonical := method + "\n" + target + "\n" + ts + "\n" + hex.EncodeToString(sum[:])
	sig := ed25519.Sign(c.Key, []byte(canonical))

	h := http.Header{}
	h.Set("Content-Type", "application/json")
	// Without this the console's error handler renders a Pug HTML page for
	// framework-level failures (415, 413, a malformed body), because /agent/*
	// does not match its /api/ prefix rule. Asking for JSON keeps every
	// failure parseable.
	h.Set("Accept", "application/json")
	h.Set("X-GW-Timestamp", ts)
	h.Set("X-GW-Signature", base64.StdEncoding.EncodeToString(sig))
	if c.ID != "" {
		h.Set("X-GW-Id", c.ID)
	}
	return h
}

// do performs one signed request. out may be nil when the response body is not
// needed. It returns the HTTP status so Register can distinguish 201 from 200.
func (c *Client) do(ctx context.Context, op, method, path string, in, out any) (int, error) {
	if len(c.Key) != ed25519.PrivateKeySize {
		return 0, fmt.Errorf("central: %s: no signing key", op)
	}
	base, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil {
		return 0, fmt.Errorf("central: %s: invalid base URL %q: %w", op, c.BaseURL, err)
	}
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + path

	// Marshal ONCE and send exactly these bytes. Re-marshalling between
	// signing and sending is the classic way to break this, and it surfaces as
	// a bare 401 with nothing else to go on.
	var raw []byte
	if in != nil {
		if raw, err = json.Marshal(in); err != nil {
			return 0, fmt.Errorf("central: %s: encode request: %w", op, err)
		}
	}

	// A GET must never carry a body. Fastify classifies GET as bodyless and
	// never runs its content-type parser, so the console always verifies
	// against sha256("") no matter what was sent — signing a body here would
	// guarantee an unexplainable 401.
	var reader io.Reader
	if method != http.MethodGet && method != http.MethodHead && raw != nil {
		reader = bytes.NewReader(raw)
	} else {
		raw = nil
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return 0, fmt.Errorf("central: %s: %w", op, err)
	}
	req.Header = c.sign(method, u.RequestURI(), raw)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// No HTTPError in the chain: the console was never reached. That is
		// what lets a caller tell an outage from a rejection.
		return 0, fmt.Errorf("central: %s: %w", op, err)
	}
	// Bound the body ONCE, here, so every read of it below is bounded by the
	// same cap — including the drain in the defer. http.Client.Timeout caps
	// seconds, not bytes, and central_insecure_tls means the peer on the other
	// end is not always authenticated, so an unbounded decode is a way to drive
	// this process out of memory. Capping only the decoder would fix the memory
	// and not the bytes read: the defer below runs on the failure path too.
	body := io.LimitReader(resp.Body, maxResponseBody+1)
	defer func() {
		_, _ = io.Copy(io.Discard, body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &HTTPError{
			Op:     op,
			Status: resp.StatusCode,
			Method: method,
			Target: u.RequestURI(),
			// Its own 2 KiB cap, now nested inside this one.
			Reason: reasonFrom(body),
		}
	}
	if out != nil {
		// Read then unmarshal, rather than streaming into a decoder: it is the
		// only way to tell "the body was too big" from "the JSON was truncated",
		// and a gateway that cannot say which is which sends an operator after
		// the wrong problem.
		raw, err := io.ReadAll(body)
		if err != nil {
			return resp.StatusCode, fmt.Errorf("central: %s: read response: %w", op, err)
		}
		if len(raw) > maxResponseBody {
			return resp.StatusCode, fmt.Errorf("central: %s: %w (over %d bytes)",
				op, ErrResponseTooLarge, maxResponseBody)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("central: %s: decode response: %w", op, err)
		}
	}
	return resp.StatusCode, nil
}

// reasonFrom extracts the console's error message, falling back to a truncated
// body when the answer was not the JSON shape we expect (a proxy error page,
// for instance).
func reasonFrom(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, maxErrorBody))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Message != "" {
		return body.Message
	}
	return strings.TrimSpace(string(raw))
}

// IsUnreachable reports whether the console could not be reached at all, as
// opposed to answering with a rejection.
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *HTTPError
	return !errors.As(err, &httpErr)
}
