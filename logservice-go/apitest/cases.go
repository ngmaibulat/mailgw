package apitest

import (
	"net/http"
	"testing"
)

// NotFoundBody is the catch-all response, plain text and keeping its trailing
// newline. The Bun service answered exactly this and both Go implementations
// reproduce it; it lives here because it is the contract rather than either
// one's detail.
const NotFoundBody = "Resource does not exist\n"

// The two /readyz reason strings. A fixed set, never driver text: the endpoint
// is unauthenticated and a database error can carry a host, user or schema
// name.
const (
	ReasonMigrating = "schema not migrated"
	ReasonNoDB      = "database unreachable"
)

// Cases returns every contract assertion.
//
// Exported rather than only reachable through Run so a differential runner can
// iterate the same table against two live processes.
func Cases() []Case {
	var cases []Case
	cases = append(cases, routingCases()...)
	cases = append(cases, authCases()...)
	cases = append(cases, ingestCases()...)
	cases = append(cases, filterCases()...)
	cases = append(cases, searchCases()...)
	cases = append(cases, healthCases()...)
	return cases
}

// --- routing ----------------------------------------------------------------

func routingCases() []Case {
	return []Case{{
		Name:    "root is open and answers OK",
		Why:     "both compose stacks and the e2e suite use GET / as the health check, and it must work without a credential",
		Fixture: Fixture{APIKey: "a-key-that-would-otherwise-be-required"},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/", "") },
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusOK, `{"status":"OK"}`)
		},
	}, {
		Name:    "an unknown path is a plain-text 404",
		Why:     "the Bun catch-all answered plain text, not JSON, and the e2e suite checks the status",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/does-not-exist", "") },
		Assert:  func(t *testing.T, res *Response) { wantPlainText404(t, res) },
	}, {
		Name:    "the root route does not prefix-match",
		Why:     "GET / is anchored; a framework whose root route is a prefix would answer OK for every unknown path",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/nope", "") },
		Assert:  func(t *testing.T, res *Response) { wantPlainText404(t, res) },
	}, {
		Name: "a wrong method on a known path is the plain-text 404, not a 405",
		Why: "verified against the frozen Bun service: a method not in a route's method object fell " +
			"through to the catch-all. Go's ServeMux suppresses its own 405 the same way, via a " +
			"catch-all pattern, and Fiber must suppress its 405 + Allow header. Do not 'fix' this " +
			"into a 405 — it would be more correct in the abstract and a behaviour change in practice",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/filter/md5", "") },
		Assert:  func(t *testing.T, res *Response) { wantPlainText404(t, res) },
	}, {
		Name:    "an unknown method is the plain-text 404, not a 501",
		Why:     "net/http routes an unrecognised method to the catch-all like any other miss",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "FOO", "/api/queue", "") },
		Assert:  func(t *testing.T, res *Response) { wantPlainText404(t, res) },
		KnownDifference: map[string]string{
			ImplFiber: "Fiber answers 501 with the body \"Not Implemented\". The check is in " +
				"router.go's defaultRequestHandler, BEFORE routing and before ErrorHandler runs, " +
				"so neither the catch-all nor the error handler can reach it. The only fix would " +
				"be to wrap app.Handler() in a fasthttp handler of our own — but app.Test serves " +
				"through app.server.ServeConn, so that wrapper would be invisible to this suite, " +
				"and untested wiring is what M16 was about. Recorded instead. No caller sends an " +
				"unrecognised method: two are Go clients using GET and POST, the third is the " +
				"console using GET",
		},
	}, {
		Name:    "a trailing slash does not match a route",
		Why:     "ServeMux does not fold a trailing slash into an exact pattern; Fiber does unless StrictRouting is set",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "POST", "/api/queue/", `{"uuid":"x"}`) },
		Assert:  func(t *testing.T, res *Response) { wantPlainText404(t, res) },
	}, {
		Name:    "paths are case-sensitive",
		Why:     "ServeMux is case-sensitive; Fiber folds case unless CaseSensitive is set",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/API/Connection", "") },
		Assert:  func(t *testing.T, res *Response) { wantPlainText404(t, res) },
	}}
}

// --- auth -------------------------------------------------------------------

func authCases() []Case {
	cases := []Case{{
		Name: "an empty API key accepts every request",
		Why: "the Bun service behaved this way, the dev compose stack relies on it (API_KEY defaults " +
			"to empty), and making it fail-closed would lock out every deployment that never set one",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/api/delivery", "") },
		Assert: func(t *testing.T, res *Response) {
			wantNotStatus(t, res, http.StatusUnauthorized,
				"an unset API_KEY must accept every request")
		},
	}, {
		Name: "auth runs before the handler",
		Why:  "an unauthenticated caller must be refused before the database is touched",
		// No DB is wired, so reaching the handler would produce a 500 instead.
		Fixture: Fixture{APIKey: "secret"},
		Request: func(t *testing.T) *http.Request { return req(t, "POST", "/api/delivery", `{}`) },
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusUnauthorized, `{"status":"Unauthorized"}`)
		},
	}}

	// The prefix and suffix cases exist because the comparison is crypto/subtle:
	// a length check that early-exits, or a compare that stops at the first
	// differing byte, is a timing oracle for a key that travels on every request
	// from every gateway.
	for _, tc := range []struct {
		name string
		key  string
		set  bool
	}{
		{"absent", "", false},
		{"empty", "", true},
		{"wrong", "nope", true},
		{"a prefix of the real key", "sec", true},
		{"the real key plus a suffix", "secretx", true},
	} {
		cases = append(cases, Case{
			Name:    "the API key is rejected when it is " + tc.name,
			Why:     "a wrong key must be a 401 with the Unauthorized envelope, never a partial match",
			Fixture: Fixture{APIKey: "secret"},
			Request: func(t *testing.T) *http.Request {
				r := req(t, "POST", "/api/queue", `{"uuid":"x"}`)
				if tc.set {
					r = withHeader(r, "X-API-Key", tc.key)
				}
				return r
			},
			Assert: func(t *testing.T, res *Response) {
				wantJSON(t, res, http.StatusUnauthorized, `{"status":"Unauthorized"}`)
			},
		})
	}
	return cases
}

// --- ingest -----------------------------------------------------------------

func ingestCases() []Case {
	return []Case{{
		Name: "an invalid delivery is refused with a bare Fail",
		Why: "the 400 is terminal for mailgw-go's event client, and the body must carry exactly one " +
			"key — a validation detail here would map the schema for an unauthenticated caller",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request {
			return req(t, "POST", "/api/delivery", `{"uuid":"x","sender":"not-an-email"}`)
		},
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusBadRequest, `{"status":"Fail"}`)
		},
	}, {
		Name: "a malformed ingest body is a 400",
		Why:  "an unparseable body can never become parseable, which is precisely what the 4xx rule is for",
		Fixture: Fixture{
			// A DB is wired so a 400 cannot be the accidental result of the
			// handler failing later for want of one.
			DB: OKDB(),
		},
		Request: func(t *testing.T) *http.Request {
			return req(t, "POST", "/api/connection", `{not json`)
		},
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusBadRequest, `{"status":"Fail"}`)
		},
	}, {
		Name: "an oversized ingest body is a 400, not a 413",
		Why: "Fiber's BodyLimit answers 413 from the protocol layer before routing; the cap here is " +
			"enforced per route because /filter/md5 needs the opposite answer",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request {
			return req(t, "POST", "/api/delivery", oversizedBody())
		},
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusBadRequest, `{"status":"Fail"}`)
		},
	}, {
		Name: "a request whose database call fails is a 500 with the Error envelope",
		Why: "mailgw-go retries a 5xx and spills only after exhausting them; a 4xx here would make it " +
			"discard the audit event permanently for what is a transient outage",
		// The fake database pings but cannot run a statement, so the request
		// gets past auth and parsing and fails in the store — which is the shape
		// of a real outage. Deliberately NOT a nil pool: that fails by nil-
		// pointer panic, and "the panic recoverer catches it" is each
		// implementation's own business. What is contract is the 500 and its
		// body.
		Fixture: Fixture{DB: OKDB()},
		Request: func(t *testing.T) *http.Request {
			// /api/connection validates nothing, so nothing between the route
			// and the store can refuse this first.
			return req(t, "POST", "/api/connection", `{"uuid":"X"}`)
		},
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusInternalServerError,
				`{"message":"Internal server error","status":"Error"}`)
		},
	}}
}

// --- /filter/md5 ------------------------------------------------------------

func filterCases() []Case {
	var cases []Case
	for _, body := range []string{`{}`, `"x"`, `5`, `null`, `{not json`} {
		cases = append(cases, Case{
			Name: "a /filter/md5 body that is not an attachment array is allowed: " + body,
			Why: "mailgw-go turns any non-2xx or unexpected body from here into an SMTP 451 under " +
				"attach.fail: closed, so refusing a bad body defers real mail",
			Fixture: Fixture{},
			Request: func(t *testing.T) *http.Request {
				return req(t, "POST", "/filter/md5", body)
			},
			Assert: func(t *testing.T, res *Response) {
				wantJSON(t, res, http.StatusOK, `{"action":"allow"}`)
			},
		})
	}
	cases = append(cases, Case{
		Name: "an oversized /filter/md5 body is a 500, not a 400",
		Why: "the asymmetry is deliberate: every 4xx from this endpoint defers mail permanently, so a " +
			"body this service could not read is reported as this service's problem",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request {
			return req(t, "POST", "/filter/md5", oversizedBody())
		},
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusInternalServerError,
				`{"message":"Internal server error","status":"Error"}`)
		},
	})
	return cases
}

// --- search -----------------------------------------------------------------

func searchCases() []Case {
	var cases []Case
	for _, q := range []string{`{not json`, `[]`, `"x"`, `{"limit":1000000}`, `{"search":"not-an-array"}`} {
		cases = append(cases, Case{
			Name: "a malformed or extreme q is never a 400: " + q,
			Why: "query.Parse degrades to the defaults and the limit is clamped rather than rejected. " +
				"The console has been built against that for as long as it has existed, and a 400 " +
				"means something specific to the gateway",
			Fixture: Fixture{},
			Request: func(t *testing.T) *http.Request {
				r := req(t, "GET", "/api/connection", "")
				v := r.URL.Query()
				v.Set("q", q)
				r.URL.RawQuery = v.Encode()
				return r
			},
			Assert: func(t *testing.T, res *Response) {
				wantNotStatus(t, res, http.StatusBadRequest,
					"a malformed q yields the defaults, never an error")
			},
		})
	}
	return cases
}

// --- health -----------------------------------------------------------------

func healthCases() []Case {
	cases := []Case{{
		Name:    "healthz needs no database and echoes the version",
		Why:     "liveness must not fail on a database outage, or an orchestrator restarts every replica during exactly the incident where restarting helps least",
		Fixture: Fixture{Version: "1.2.3"},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/healthz", "") },
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusOK, `{"status":"ok","version":"1.2.3"}`)
		},
	}, {
		Name:    "readyz is not ready before migrations complete",
		Why:     "readiness gates a load balancer; serving before the schema exists means a 500 per audit event",
		Fixture: Fixture{},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/readyz", "") },
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusServiceUnavailable,
				`{"reasons":["`+ReasonMigrating+`","`+ReasonNoDB+`"],"status":"unavailable"}`)
		},
	}, {
		Name:    "readyz is ready when migrated and the database answers",
		Why:     "the 200 body is what an orchestrator reads; it must not carry anything else",
		Fixture: Fixture{Ready: true, DB: OKDB()},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/readyz", "") },
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusOK, `{"status":"ready"}`)
		},
	}, {
		Name:    "readyz reports an unreachable database without echoing the driver",
		Why:     "this endpoint is unauthenticated, and a driver error can carry a host, user or schema name",
		Fixture: Fixture{Ready: true, DB: FailDB()},
		Request: func(t *testing.T) *http.Request { return req(t, "GET", "/readyz", "") },
		Assert: func(t *testing.T, res *Response) {
			wantJSON(t, res, http.StatusServiceUnavailable,
				`{"reasons":["`+ReasonNoDB+`"],"status":"unavailable"}`)
		},
	}}

	for _, path := range []string{"/healthz", "/readyz"} {
		cases = append(cases, Case{
			Name:    "the probe endpoint is open: " + path,
			Why:     "a monitoring probe carries no credential",
			Fixture: Fixture{APIKey: "secret", Ready: true, DB: OKDB()},
			Request: func(t *testing.T) *http.Request { return req(t, "GET", path, "") },
			Assert: func(t *testing.T, res *Response) {
				wantNotStatus(t, res, http.StatusUnauthorized, "probes have no credential")
			},
		})
	}
	return cases
}
