// Package apitest is the logservice HTTP wire contract, written as executable
// assertions that every implementation must satisfy.
//
// # Why this is a package and not a test file
//
// Since M23 there are two implementations — net/http in logservice-go and
// Fiber v3 in logservice-fiber — and "they behave the same" is a claim until
// something runs the same assertions against both. The assertions here are the
// ones other systems make irreversible decisions from:
//
//   - mailgw-go/internal/events treats ANY 4xx as terminal and spills the audit
//     event to the gateway's disk rather than retrying;
//   - mailgw-go/internal/attach turns any non-2xx from /filter/md5 into an
//     error, which becomes SMTP 451 and defers real mail;
//   - webui-fastify maps a non-2xx to 502 and a network failure to 504.
//
// So a framework's default 405 where this answers 404, or 413 where it answers
// 400, is not a cosmetic difference. It destroys audit rows or defers mail.
//
// # This package imports neither implementation, and must not
//
// It is the specification; they are the subjects. It depends on nothing but the
// standard library, which is also what lets it live here and be imported from a
// sibling module.
//
// # What belongs here, and what does not
//
// Contract: anything observable on the wire — status, headers, body bytes.
// Implementation-local: how a result is produced. The panic recoverer is the
// clearest example. Each implementation keeps its own direct test that a
// panicking handler yields a 500, because one does it with a deferred recover
// over an http.Handler and the other with a Fiber middleware; what belongs HERE
// is that a request which cannot reach a database answers 500 with the Error
// envelope, which is the part any caller can see and the part that matters.
package apitest

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Response is the flattened result of one request, so no assertion has to know
// whether it arrived through httptest.ResponseRecorder or Fiber's app.Test.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Target is one implementation under test, already built from a Fixture.
type Target interface {
	// Do serves req and returns the response with its body fully read.
	Do(t *testing.T, req *http.Request) *Response
}

// Fixture is everything a Target is constructed from.
//
// Deliberately tiny. The contract is about the HTTP layer, so the only things
// it varies are the API key, the version string, the database — where nil is a
// legal and tested state, being what a caller sees when the pool is gone — and
// whether migrations have completed.
type Fixture struct {
	APIKey  string
	Version string
	DB      *sql.DB
	Ready   bool
}

// Builder constructs a Target from a Fixture. One per implementation.
type Builder func(t *testing.T, f Fixture) Target

// Case is one contract assertion.
type Case struct {
	// Name is used as the subtest name.
	Name string

	// Why is printed when the case fails: what breaks in production if this
	// behaviour changes. A failing contract test should not require reading the
	// git history to understand the stakes.
	Why string

	Fixture Fixture
	Request func(t *testing.T) *http.Request
	Assert  func(t *testing.T, res *Response)

	// KnownDifference names implementations that provably cannot satisfy this
	// case, mapped to why. Keyed by the name passed to Run.
	//
	// This exists so a divergence is RECORDED rather than deleted. Removing the
	// case would make the suite quietly weaker for both implementations; an
	// entry here keeps the assertion running everywhere else, keeps the reason
	// next to the thing it is about, and turns "we gave up on this one" into a
	// line somebody can delete when the upstream reason goes away.
	//
	// Every entry must also appear in the implementation's known-differences
	// table. Do not add one to make a test pass — add one when the behaviour is
	// genuinely unreachable, and say what makes it unreachable.
	KnownDifference map[string]string
}

// Run executes every case against build.
//
// impl names the implementation under test — "net/http" or "fiber" — and is
// used only to resolve Case.KnownDifference.
func Run(t *testing.T, impl string, build Builder) {
	t.Helper()
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			if why, ok := c.KnownDifference[impl]; ok {
				t.Skipf("known difference in %s: %s", impl, why)
			}
			target := build(t, c.Fixture)
			res := target.Do(t, c.Request(t))
			// The Why is attached to the subtest rather than printed on every
			// run, so a green suite stays quiet and a red one explains itself.
			defer func() {
				if t.Failed() && c.Why != "" {
					t.Logf("why this matters: %s", c.Why)
				}
			}()
			c.Assert(t, res)
		})
	}
}

// Implementation names for Case.KnownDifference and Run.
const (
	ImplNetHTTP = "net/http"
	ImplFiber   = "fiber"
)

// ReadResponse is a helper for a Target built on *http.Response: it reads and
// closes the body so an adapter is three lines rather than eight.
func ReadResponse(t *testing.T, res *http.Response) *Response {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = res.Body.Close()
	return &Response{Status: res.StatusCode, Header: res.Header, Body: body}
}

// host is the authority every request carries.
//
// Requests are built with http.NewRequest rather than httptest.NewRequest, and
// that is not a style choice: httptest.NewRequest produces a SERVER-side
// request (RequestURI set, a synthetic RemoteAddr), while Fiber's app.Test
// serialises the request with req.Write, which is a client-side operation and
// needs a URL with a host. http.NewRequest is valid for both — net/http's
// ServeMux routes on URL.Path and does not care about the authority.
const host = "http://logservice.test"

// req builds a request. A non-empty body carries the JSON content type, which
// is what every real caller sends.
func req(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, host+target, nil)
	} else {
		r, err = http.NewRequest(method, host+target, strings.NewReader(body))
	}
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, target, err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
		// Set explicitly: strings.Reader gives net/http a known length, but an
		// implementation that streams should still see one, and the oversized
		// cases depend on it being there.
		r.ContentLength = int64(len(body))
	}
	return r
}

func withHeader(r *http.Request, k, v string) *http.Request {
	r.Header.Set(k, v)
	return r
}

// --- assertions -------------------------------------------------------------

func wantStatus(t *testing.T, res *Response, want int) {
	t.Helper()
	if res.Status != want {
		t.Errorf("status = %d, want %d (body %q)", res.Status, want, res.Body)
	}
}

// wantJSON asserts the status, the exact content type and the exact body bytes.
//
// Byte-exact, including the trailing newline, because that is what the two
// implementations have to agree on and the e2e suite's res.json() would not
// notice either. Pass want WITHOUT the newline; it is added here so no case has
// to remember it.
func wantJSON(t *testing.T, res *Response, status int, want string) {
	t.Helper()
	wantStatus(t, res, status)
	// Exactly application/json — no "; charset=utf-8". Fiber's helpers differ
	// from each other on this and net/http sends the bare form.
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want exactly %q", ct, "application/json")
	}
	if got := string(res.Body); got != want+"\n" {
		t.Errorf("body = %q, want %q", got, want+"\n")
	}
}

// wantPlainText404 asserts the catch-all response, including the absence of an
// Allow header — which is what tells a 404 produced deliberately apart from a
// framework's 405 that was rewritten after the fact.
func wantPlainText404(t *testing.T, res *Response) {
	t.Helper()
	wantStatus(t, res, http.StatusNotFound)
	if got := string(res.Body); got != NotFoundBody {
		t.Errorf("body = %q, want %q", got, NotFoundBody)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want a text/plain prefix", ct)
	}
	if v := res.Header.Get("Allow"); v != "" {
		t.Errorf("Allow = %q; net/http sends none here, and a 405-shaped response "+
			"is exactly what this case exists to prevent", v)
	}
}

// wantNotStatus is for the cases whose contract is a negative — "never a 400",
// "never a 401" — where what the implementation answers instead is its own
// business.
func wantNotStatus(t *testing.T, res *Response, unwanted int, why string) {
	t.Helper()
	if res.Status == unwanted {
		t.Errorf("status = %d, and it must never be: %s", unwanted, why)
	}
}

// oversizedBody builds a JSON body past any reasonable cap.
//
// 4 MiB rather than one byte over 1 MiB, deliberately: it is past Fiber's
// default BodyLimit too, so a case that passes cannot be passing because the
// framework happened to allow it through to the handler's own check.
func oversizedBody() string {
	return fmt.Sprintf(`{"uuid":%q}`, strings.Repeat("x", 4<<20))
}
