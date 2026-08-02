package central

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

// verifyLikeConsole is webui-fastify/src/agent/verify.ts reimplemented: rebuild
// the canonical string from what actually arrived and check the signature over
// it. If the client and the console ever disagree about what gets signed, this
// is what notices.
func verifyLikeConsole(t *testing.T, pub ed25519.PublicKey, r *http.Request) []byte {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Fastify treats GET as bodyless: its content-type parser never runs, so
	// the console always digests "" regardless of what was sent.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		body = nil
	}

	ts := r.Header.Get("X-GW-Timestamp")
	if ts == "" {
		t.Error("missing X-GW-Timestamp")
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-GW-Signature"))
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}

	sum := sha256.Sum256(body)
	canonical := r.Method + "\n" + r.URL.RequestURI() + "\n" + ts + "\n" + hex.EncodeToString(sum[:])
	if !ed25519.Verify(pub, []byte(canonical), sig) {
		t.Errorf("signature does not verify over %q", canonical)
	}
	return body
}

func newClient(t *testing.T, srv *httptest.Server, priv ed25519.PrivateKey, id string) *Client {
	t.Helper()
	return &Client{BaseURL: srv.URL, ID: id, Key: priv, HTTP: srv.Client()}
}

func writeJSON(t *testing.T, w http.ResponseWriter, code int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestClient_RegisterSignsPresentedKeyAndOmitsGWId(t *testing.T) {
	pub, priv := testKey(t)

	var gotPubKey, gotHostname string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.RequestURI(); got != "/agent/register" {
			t.Errorf("target = %q, want /agent/register", got)
		}
		// Registration proves possession of the presented key; an id would be
		// meaningless because the gateway does not have one yet.
		if id := r.Header.Get("X-GW-Id"); id != "" {
			t.Errorf("X-GW-Id = %q, want it omitted on register", id)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if ac := r.Header.Get("Accept"); ac != "application/json" {
			t.Errorf("Accept = %q, want application/json", ac)
		}

		body := verifyLikeConsole(t, pub, r)
		var req struct {
			PubKey   string `json:"pubkey"`
			Hostname string `json:"hostname"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		gotPubKey, gotHostname = req.PubKey, req.Hostname

		writeJSON(t, w, http.StatusCreated, map[string]any{
			"status": "ok", "gateway_uid": "uid-1",
			"fingerprint": "fp", "approval": "pending",
		})
	}))
	defer srv.Close()

	c := newClient(t, srv, priv, "")
	resp, err := c.Register(context.Background(), pub, SystemInfo{Hostname: "gw1"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if resp.GatewayUID != "uid-1" || resp.Approval != "pending" {
		t.Errorf("response = %+v", resp)
	}
	if !resp.New {
		t.Error("New = false for a 201")
	}
	if gotHostname != "gw1" {
		t.Errorf("hostname = %q", gotHostname)
	}
	// The console's zod schema is exactly base64 of 32 raw bytes.
	if !regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`).MatchString(gotPubKey) {
		t.Errorf("pubkey %q does not match the console's accepted form", gotPubKey)
	}
}

// A known fingerprint answers 200 rather than 201. Both are success, and the
// approval field is authoritative — a gateway that treated 200 as failure
// would re-register forever.
func TestClient_RegisterAcceptsExistingFingerprint(t *testing.T) {
	pub, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyLikeConsole(t, pub, r)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status": "ok", "gateway_uid": "uid-1",
			"fingerprint": "fp", "approval": "approved",
		})
	}))
	defer srv.Close()

	resp, err := newClient(t, srv, priv, "").Register(context.Background(), pub, SystemInfo{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.New {
		t.Error("New = true for a 200")
	}
	if resp.Approval != "approved" {
		t.Errorf("approval = %q", resp.Approval)
	}
}

func TestClient_StatusSignsEmptyBodyDigestAndSendsID(t *testing.T) {
	pub, priv := testKey(t)
	const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-GW-Id"); got != "uid-9" {
			t.Errorf("X-GW-Id = %q, want uid-9", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("GET carried a %d-byte body; the console would digest \"\" instead", len(body))
		}

		// Recompute the way the console does and confirm the empty digest.
		ts := r.Header.Get("X-GW-Timestamp")
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-GW-Signature"))
		canonical := "GET\n/agent/status\n" + ts + "\n" + emptyDigest
		if !ed25519.Verify(pub, []byte(canonical), sig) {
			t.Errorf("signature does not verify over %q", canonical)
		}

		writeJSON(t, w, http.StatusOK, map[string]any{
			"status": "ok", "approval": "approved",
			"desired_version_id": 42, "desired_version": 7,
			"bundle_sha256": "deadbeef", "applied_version_id": nil,
		})
	}))
	defer srv.Close()

	got, err := newClient(t, srv, priv, "uid-9").Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Approval != "approved" {
		t.Errorf("approval = %q", got.Approval)
	}
	if got.DesiredVersionID == nil || *got.DesiredVersionID != 42 {
		t.Errorf("desired_version_id = %v", got.DesiredVersionID)
	}
	if got.AppliedVersionID != nil {
		t.Errorf("applied_version_id = %v, want nil", got.AppliedVersionID)
	}
}

// A BaseURL carrying a path prefix must be part of the signed target.
func TestClient_TargetIncludesBasePathPrefix(t *testing.T) {
	pub, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.RequestURI(); got != "/mgmt/agent/status" {
			t.Errorf("target = %q, want /mgmt/agent/status", got)
		}
		verifyLikeConsole(t, pub, r)
		writeJSON(t, w, http.StatusOK, map[string]any{"status": "ok", "approval": "pending"})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/mgmt", ID: "uid", Key: priv, HTTP: srv.Client()}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
}

// The regression guard for "sign the bytes you send". A console that verified
// against a re-serialised body would reject us, which is what would happen if
// the client ever marshalled twice.
func TestClient_SignsTheExactBytesItSends(t *testing.T) {
	pub, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		ts := r.Header.Get("X-GW-Timestamp")
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-GW-Signature"))

		// Verifying over the raw bytes must succeed...
		sum := sha256.Sum256(body)
		exact := r.Method + "\n" + r.URL.RequestURI() + "\n" + ts + "\n" + hex.EncodeToString(sum[:])
		if !ed25519.Verify(pub, []byte(exact), sig) {
			t.Error("signature does not verify over the bytes actually received")
		}

		// ...and over a re-marshalled copy must NOT, or the test proves nothing.
		var round map[string]any
		if err := json.Unmarshal(body, &round); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		remarshalled, err := json.Marshal(map[string]any{"reordered": true, "x": round})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sum2 := sha256.Sum256(remarshalled)
		other := r.Method + "\n" + r.URL.RequestURI() + "\n" + ts + "\n" + hex.EncodeToString(sum2[:])
		if ed25519.Verify(pub, []byte(other), sig) {
			t.Error("signature verified over different bytes; the guard is not testing anything")
		}

		writeJSON(t, w, http.StatusOK, map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	err := newClient(t, srv, priv, "uid").Report(context.Background(), Report{Version: "1.2.3"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
}

// The console merges on `!== undefined`, so clearing a stale apply_error
// requires an explicit null on the wire.
func TestClient_ReportSendsExplicitNullsToClearState(t *testing.T) {
	_, priv := testKey(t)

	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	err := newClient(t, srv, priv, "uid").Report(context.Background(), Report{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	for _, key := range []string{"applied_version_id", "apply_error"} {
		v, ok := raw[key]
		if !ok {
			t.Errorf("%s was omitted; the console would keep its previous value forever", key)
			continue
		}
		if string(v) != "null" {
			t.Errorf("%s = %s, want null", key, v)
		}
	}
}

func TestClient_ConfigNotApprovedIsDistinguishable(t *testing.T) {
	_, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusForbidden, map[string]any{
			"status": "error", "message": "gateway is pending", "approval": "pending",
		})
	}))
	defer srv.Close()

	_, err := newClient(t, srv, priv, "uid").Config(context.Background())
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("err = %v, want ErrNotApproved", err)
	}
	// errors.As must still yield the detail for the log line.
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatal("expected an *HTTPError in the chain")
	}
	if httpErr.Status != 403 || httpErr.Reason != "gateway is pending" {
		t.Errorf("httpErr = %+v", httpErr)
	}
}

func TestClient_ConfigNoConfigDeployedIsDistinguishable(t *testing.T) {
	_, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{
			"status": "error", "message": "no configuration has been deployed yet",
		})
	}))
	defer srv.Close()

	_, err := newClient(t, srv, priv, "uid").Config(context.Background())
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("err = %v, want ErrNoConfig", err)
	}
	if errors.Is(err, ErrNotApproved) {
		t.Error("a 404 must not read as 'not approved'")
	}
}

// Every auth failure is a 401, so the body message is the only discriminator.
// "Fix the clock" and "your key is wrong" are very different actions.
func TestClient_ClockSkewIsDistinguishableFromABadKey(t *testing.T) {
	_, priv := testKey(t)

	cases := []struct {
		name    string
		message string
		want    error
	}{
		{"skew", "timestamp outside the accepted window", ErrClockSkew},
		{"missing timestamp", "missing X-GW-Timestamp", ErrClockSkew},
		{"unknown gateway", "unknown gateway", ErrUnknownGateway},
		{"bad signature", "signature verification failed", ErrUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusUnauthorized, map[string]any{
					"status": "error", "message": tc.message,
				})
			}))
			defer srv.Close()

			_, err := newClient(t, srv, priv, "uid").Status(context.Background())
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// The injected clock is what makes the skew path testable without touching the
// system clock.
func TestClient_UsesInjectedClockForTheTimestamp(t *testing.T) {
	pub, priv := testKey(t)
	fixed := time.Unix(1700000000, 0)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-GW-Timestamp"); got != strconv.FormatInt(fixed.Unix(), 10) {
			t.Errorf("timestamp = %q, want %d", got, fixed.Unix())
		}
		verifyLikeConsole(t, pub, r)
		writeJSON(t, w, http.StatusOK, map[string]any{"status": "ok", "approval": "pending"})
	}))
	defer srv.Close()

	c := newClient(t, srv, priv, "uid")
	c.now = func() time.Time { return fixed }
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
}

// An outage must be distinguishable from a rejection: one means "keep running
// on the last-good config", the other means something needs an operator.
func TestClient_UnreachableConsoleIsNotAnHTTPError(t *testing.T) {
	_, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := newClient(t, srv, priv, "uid")
	srv.Close() // nothing is listening now

	_, err := c.Status(context.Background())
	if err == nil {
		t.Fatal("expected an error against a closed server")
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		t.Errorf("got an *HTTPError (%+v) for an unreachable console", httpErr)
	}
	if !IsUnreachable(err) {
		t.Error("IsUnreachable = false for an unreachable console")
	}
}

// A non-JSON body (a proxy error page) must still yield a usable message.
func TestClient_NonJSONErrorBodyStillReportsSomething(t *testing.T) {
	_, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
	}))
	defer srv.Close()

	_, err := newClient(t, srv, priv, "uid").Status(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected an *HTTPError, got %v", err)
	}
	if httpErr.Status != 502 || httpErr.Reason == "" {
		t.Errorf("httpErr = %+v", httpErr)
	}
	// A 502 is not an auth failure and must not map onto one.
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNotApproved) {
		t.Error("a 502 was classified as an auth failure")
	}
}

func TestClient_ConfigReturnsTheBundleVerbatim(t *testing.T) {
	pub, priv := testKey(t)
	const bundle = `{"format":1,"server":null,"allowlist":{"allowed":["10.0.0.0/8"]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyLikeConsole(t, pub, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","version_id":42,"version":7,`+
			`"bundle_sha256":"abc","bundle":`+bundle+`}`)
	}))
	defer srv.Close()

	got, err := newClient(t, srv, priv, "uid").Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got.VersionID != 42 || got.Version != 7 || got.BundleSHA256 != "abc" {
		t.Errorf("response = %+v", got)
	}
	// The bundle stays raw: M5 parses it, and the digest is the console's to
	// compute, so nothing here should reshape it.
	if string(got.Bundle) != bundle {
		t.Errorf("bundle was reshaped in transit:\n got %s\nwant %s", got.Bundle, bundle)
	}
}

func TestClient_MissingKeyIsAClearError(t *testing.T) {
	c := &Client{BaseURL: "https://example.invalid"}
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("expected an error when no signing key is configured")
	}
}

// A console that streams an unbounded body must not be able to take this
// process out on memory. central_insecure_tls means the peer is not always
// authenticated, so "the console would never do that" is not an argument.
func TestClient_RefusesAnOversizeResponse(t *testing.T) {
	pub, priv := testKey(t)

	// Valid JSON that never ends: a decoder with no cap buffers all of it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyLikeConsole(t, pub, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","bundle":"`)
		chunk := strings.Repeat("A", 64<<10)
		for written := 0; written < maxResponseBody+(1<<20); written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	done := make(chan error, 1)
	go func() { _, err := newClient(t, srv, priv, "uid").Config(context.Background()); done <- err }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("err = %v, want ErrResponseTooLarge", err)
		}
		// Deliberately not an *HTTPError: the poll loop should treat this the
		// way it treats an outage — keep the last-good configuration and retry.
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			t.Error("an oversize body was classified as a console rejection")
		}
		if !IsUnreachable(err) {
			t.Error("IsUnreachable = false; the poll loop would treat this as a real answer")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Config never returned; the body is not bounded")
	}
}

// The cap must bound the whole body, not just the decoder: the deferred drain
// that exists for connection reuse reads from the same reader, and on the
// failure path it would otherwise pull the rest of an unbounded response.
func TestClient_BoundsTheBodyOnTheErrorPathToo(t *testing.T) {
	pub, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyLikeConsole(t, pub, r)
		w.Header().Set("Content-Type", "application/json")
		// Not JSON at all, so the decode fails immediately and the defer runs.
		chunk := strings.Repeat("x", 64<<10)
		for written := 0; written < maxResponseBody+(1<<20); written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	done := make(chan error, 1)
	go func() { _, err := newClient(t, srv, priv, "uid").Status(context.Background()); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a garbage body was accepted")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Status never returned; the drain is not bounded")
	}
}

// The ordinary case must be untouched: a real bundle is kilobytes.
func TestClient_NormalResponsesAreUnaffectedByTheCap(t *testing.T) {
	pub, priv := testKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifyLikeConsole(t, pub, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","version_id":1,"version":1,`+
			`"bundle_sha256":"d","bundle":{"format":1}}`)
	}))
	defer srv.Close()

	got, err := newClient(t, srv, priv, "uid").Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got.VersionID != 1 || string(got.Bundle) != `{"format":1}` {
		t.Errorf("response = %+v", got)
	}
}
