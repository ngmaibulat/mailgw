package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every request goes through Handler(), never a bare handler func, so the route
// table and the middleware wrapping are themselves under test. A test that
// called s.handleRoot directly would pass with the auth wrapper deleted.
func do(t *testing.T, s *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return m
}

// The health check is open and answers a fixed body. The e2e suite asserts this
// body, and both compose stacks may point a probe at it.
func TestRoot_IsOpenAndAnswersOK(t *testing.T) {
	s := &Server{APIKey: "a-key-that-would-otherwise-be-required"}

	rec := do(t, s, http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decode(t, rec); got["status"] != "OK" {
		t.Errorf("body = %v, want {\"status\":\"OK\"}", got)
	}
}

// The Bun catch-all answered plain text, not JSON. Reproduced exactly.
func TestUnknownPath_IsAPlainText404(t *testing.T) {
	s := &Server{}

	rec := do(t, s, http.MethodGet, "/does-not-exist", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); got != notFoundBody {
		t.Errorf("body = %q, want %q", got, notFoundBody)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// GET / is registered with {$} so it matches only the root; a longer path must
// fall through to the catch-all rather than being answered OK.
func TestRootPattern_DoesNotSwallowOtherPaths(t *testing.T) {
	s := &Server{}
	rec := do(t, s, http.MethodGet, "/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — GET / must not match a prefix", rec.Code)
	}
}

// An empty APIKey means open. This is the Bun service's behaviour and what the
// dev compose stack relies on; making it fail-closed would lock out every
// deployment that never set a key.
func TestAuth_EmptyKeyAcceptsEveryRequest(t *testing.T) {
	s := &Server{}

	rec := do(t, s, http.MethodGet, "/api/delivery", "", nil)
	// No DB is wired, so this reaches the handler and fails there — a 500. What
	// matters is that it was NOT a 401.
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("an unset API_KEY rejected a request; it must accept every request")
	}
}

func TestAuth_RejectsAMissingOrWrongKey(t *testing.T) {
	s := &Server{APIKey: "secret"}

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"absent", nil},
		{"empty", map[string]string{"X-API-Key": ""}},
		{"wrong", map[string]string{"X-API-Key": "nope"}},
		{"prefix of the real key", map[string]string{"X-API-Key": "sec"}},
		{"real key plus a suffix", map[string]string{"X-API-Key": "secretx"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, "/api/queue", `{"uuid":"x"}`, tc.headers)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := decode(t, rec); got["status"] != "Unauthorized" {
				t.Errorf("body = %v, want {\"status\":\"Unauthorized\"}", got)
			}
		})
	}
}

// Auth is the OUTER wrapper: an unauthenticated caller is refused before the
// handler — and therefore before the database — is touched. This Server has no
// DB, so reaching the handler would panic and produce a 500 instead.
func TestAuth_RunsBeforeTheHandler(t *testing.T) {
	s := &Server{APIKey: "secret"}
	rec := do(t, s, http.MethodPost, "/api/delivery", `{}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — auth must run before the handler", rec.Code)
	}
}

// The one endpoint that validates. A body that can never be stored is a 400 with
// a bare Fail — no schema detail leaks to the caller.
func TestPostDelivery_RejectsAnInvalidBodyBeforeTouchingTheDatabase(t *testing.T) {
	s := &Server{} // no DB: reaching the store would panic

	rec := do(t, s, http.MethodPost, "/api/delivery",
		`{"uuid":"x","sender":"not-an-email"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	got := decode(t, rec)
	if got["status"] != "Fail" {
		t.Errorf("body = %v, want {\"status\":\"Fail\"}", got)
	}
	if len(got) != 1 {
		t.Errorf("body = %v, want exactly one key — validation detail must not leak", got)
	}
}

// A body that is not an attachment array must be ALLOWED, not refused. mailgw-go
// turns any non-2xx from here into an SMTP 451, so a 400 would defer real mail.
func TestFilterMD5_ANonArrayBodyIsAllowedNotRejected(t *testing.T) {
	s := &Server{}

	for _, body := range []string{`{}`, `"x"`, `5`, `null`, `{not json`} {
		t.Run(body, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, "/filter/md5", body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — a bad body must not defer mail", rec.Code)
			}
			if got := decode(t, rec); got["action"] != "allow" {
				t.Errorf("action = %v, want allow", got["action"])
			}
		})
	}
}

// A panic becomes a 500, not a dropped connection: mailgw-go reads a dropped
// connection as a transport error and eventually spills the event to disk.
func TestRecoverPanics_TurnsAPanicIntoA500(t *testing.T) {
	s := &Server{}
	h := s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := decode(t, rec); got["status"] != "Error" {
		t.Errorf("body = %v, want the Error envelope", got)
	}
}

// Liveness performs no I/O, so it answers even with no database wired at all.
func TestHealthz_NeedsNoDatabase(t *testing.T) {
	s := &Server{Version: "1.2.3"}

	rec := do(t, s, http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decode(t, rec); got["version"] != "1.2.3" {
		t.Errorf("version = %v, want 1.2.3", got["version"])
	}
}

// Readiness is false until migrations have run, and says so without echoing any
// driver text — this endpoint is unauthenticated.
func TestReadyz_IsNotReadyBeforeMigrationsComplete(t *testing.T) {
	s := &Server{}

	rec := do(t, s, http.MethodGet, "/readyz", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	got := decode(t, rec)
	reasons, ok := got["reasons"].([]any)
	if !ok || len(reasons) == 0 {
		t.Fatalf("body = %v, want a non-empty reasons array", got)
	}
	if reasons[0] != reasonMigrating {
		t.Errorf("first reason = %v, want %q", reasons[0], reasonMigrating)
	}
}

// Health and readiness carry no credential, so a probe works without one.
func TestHealthEndpoints_AreOpen(t *testing.T) {
	s := &Server{APIKey: "secret"}

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := do(t, s, http.MethodGet, path, "", nil)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s answered 401; probes have no credential", path)
		}
	}
}

// A wrong method on a known path answers the same plain-text 404 as an unknown
// path — NOT a 405.
//
// That is not an accident of Go's ServeMux, it is the Bun service's behaviour,
// verified directly against it: a request whose method is not in a route's
// method object falls through to the catch-all `fetch`, which answered
// "Resource does not exist\n" with a 404. Registering the catch-all here
// suppresses ServeMux's automatic 405 in exactly the same way, so the two
// implementations agree without either of them special-casing anything.
//
// Do not "fix" this into a 405. It would be more correct in the abstract and a
// behaviour change in practice.
func TestWrongMethod_IsThePlainText404JustAsInBun(t *testing.T) {
	s := &Server{}

	rec := do(t, s, http.MethodGet, "/filter/md5", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /filter/md5 = %d, want 404 (the Bun service answers 404 here)", rec.Code)
	}
	if got := rec.Body.String(); got != notFoundBody {
		t.Errorf("body = %q, want %q", got, notFoundBody)
	}
}

// A body larger than the cap is refused rather than read into memory.
func TestReadBody_IsBounded(t *testing.T) {
	s := &Server{}
	huge := `{"uuid":"` + strings.Repeat("x", maxBodyBytes+1) + `"}`

	rec := do(t, s, http.MethodPost, "/api/delivery", huge, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversized body", rec.Code)
	}
}

// writeJSON must never emit a partial body under a 200.
func TestWriteJSON_SetsTheContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, bodyOK)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	if strings.TrimSpace(string(body)) != `{"status":"OK"}` {
		t.Errorf("body = %q", body)
	}
}
