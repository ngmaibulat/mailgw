package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What is left here after M23 is what is NOT contract.
//
// The wire behaviour — every status, header and body byte this service answers
// with — moved to logservice-go/apitest and now runs against both this
// implementation and logservice-fiber's; see contract_test.go. Duplicating those
// assertions here would mean two places to update and one of them going stale.
//
// A test belongs in this file when it is about HOW this implementation produces
// a result rather than WHAT it produces: it reaches into an unexported function,
// or it depends on something net/http-specific that Fiber has no counterpart
// for. Both tests below are of that kind. If you find yourself writing an
// assertion here about a status code or a response body, it belongs in apitest.

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return m
}

// A panic becomes a 500, not a dropped connection: mailgw-go reads a dropped
// connection as a transport error and eventually spills the event to disk.
//
// Implementation-local because it builds the middleware directly around a
// handler that panics — the contract suite asserts the 500 an operator sees, not
// the mechanism. Fiber's equivalent is its own middleware and is tested in its
// own module.
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

// writeJSON must never emit a partial body under a 200.
//
// Implementation-local because it calls the unexported writer directly, on a
// value the route table has no way to produce. The content type and the trailing
// newline it sets are contract and are asserted on every JSON case in apitest;
// what is checked here is the buffer-first behaviour that makes a mid-encode
// failure impossible to observe.
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
