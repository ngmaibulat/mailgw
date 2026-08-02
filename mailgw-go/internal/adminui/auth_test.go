package adminui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
)

// managed builds a claimable gateway and returns it with its claim code.
func managed(t *testing.T, st State) (*Server, string) {
	t.Helper()
	s := &Server{
		Store:    testStore(t),
		Version:  "1.0.0",
		State:    func() State { return st },
		Register: func(context.Context, Settings) error { return nil },
	}
	claim, err := s.Store.EnsureClaimCode()
	if err != nil {
		t.Fatalf("EnsureClaimCode: %v", err)
	}
	return s, claim.Code
}

// An unclaimed node shows one page and refuses everything else. This is the
// defect M12 exists to close: before it, this same POST re-pointed the gateway
// at any Central Manager the caller named.
func TestAuth_UnclaimedRefusesRegisterAndRendersTheClaimPage(t *testing.T) {
	s, _ := managed(t, State{Fingerprint: "fp-abc"})

	rec := postForm(t, s, "/register", "central_url=https://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /register = %d, want 403", rec.Code)
	}

	page := get(t, s, "/")
	if page.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, `action="/claim"`) {
		t.Error("the claim page has no claim form")
	}
	if strings.Contains(body, `action="/register"`) {
		t.Error("the claim page offers the registration form")
	}
}

// The fingerprint exists before registration so this gateway can be
// pre-approved in the console. A bare login page would break that silently.
func TestAuth_ClaimPageShowsTheFingerprintAndNothingElse(t *testing.T) {
	s, _ := managed(t, State{
		Fingerprint: "fp-abc",
		GatewayUID:  "uid-1",
		CentralURL:  "https://console.example:4000",
		Approval:    "pending",
		LastError:   "dial tcp: connection refused",
		ApplyError:  "rule 3: unknown field",
	})

	body := get(t, s, "/").Body.String()

	if !strings.Contains(body, "fp-abc") {
		t.Error("the claim page does not show the fingerprint")
	}
	for _, leaked := range []string{
		"https://console.example:4000", "uid-1", "pending",
		"connection refused", "unknown field",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("the unauthenticated claim page leaked %q", leaked)
		}
	}
}

// baseData stats the spool directories. An unauthenticated caller must not be
// able to ask for that work in a loop.
func TestAuth_ClaimPageDoesNotReadTheSpool(t *testing.T) {
	s, _ := managed(t, State{Fingerprint: "fp"})
	called := 0
	s.SpoolFn = func() *queue.Spool { called++; return nil }

	if rec := get(t, s, "/"); rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if called != 0 {
		t.Errorf("the claim page read the spool %d times", called)
	}
}

func TestAuth_ClaimCodeSignsInAndSetsACookie(t *testing.T) {
	s, code := managed(t, State{Fingerprint: "fp"})

	rec := postForm(t, s, "/claim", "code="+code)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /claim = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName || cookies[0].Value == "" {
		t.Fatalf("no session cookie was set: %+v", cookies)
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	// Over plain HTTP a Secure cookie is simply never sent back, so setting it
	// unconditionally would make the UI unusable on the shipped node.
	if c.Secure {
		t.Error("the session cookie is Secure over a plain-HTTP request")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}

	// And the cookie actually works.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	page := httptest.NewRecorder()
	s.Handler().ServeHTTP(page, req)
	if !strings.Contains(page.Body.String(), `action="/register"`) {
		t.Error("a signed-in visitor did not get the wizard")
	}
}

// The code is NOT single-use. A code consumed on first presentation, with a
// cookie as the only other credential, leaves this gateway reachable by exactly
// one browser for ever.
func TestAuth_ClaimCodeWorksForASecondOperator(t *testing.T) {
	s, code := managed(t, State{Fingerprint: "fp"})

	first := postForm(t, s, "/claim", "code="+code)
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first claim = %d, want 303", first.Code)
	}
	second := postForm(t, s, "/claim", "code="+code)
	if second.Code != http.StatusSeeOther {
		t.Fatalf("second claim = %d, want 303: the code must not be consumed", second.Code)
	}
	if a, b := first.Result().Cookies(), second.Result().Cookies(); a[0].Value == b[0].Value {
		t.Error("two sign-ins share one session id")
	}
}

func TestAuth_WrongClaimCodeIsRefusedAndDoesNotInvalidateTheRealOne(t *testing.T) {
	s, code := managed(t, State{Fingerprint: "fp"})

	rec := postForm(t, s, "/claim", "code=WRONG-WRONG-WRONG-WRONG")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad code = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a refused claim set a cookie")
	}
	if !strings.Contains(rec.Body.String(), `action="/claim"`) {
		t.Error("a refused claim did not re-render the claim form")
	}

	if again := postForm(t, s, "/claim", "code="+code); again.Code != http.StatusSeeOther {
		t.Errorf("the real code stopped working after a failed guess: %d", again.Code)
	}
}

// Dashes and case are presentation. An operator retyping a code from a log line
// must not be refused over them.
func TestAuth_ClaimCodeAcceptsADictatedForm(t *testing.T) {
	s, code := managed(t, State{Fingerprint: "fp"})
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))

	if rec := postForm(t, s, "/claim", "code="+typed); rec.Code != http.StatusSeeOther {
		t.Errorf("POST /claim with %q = %d, want 303", typed, rec.Code)
	}
}

// The bucket exists so a scanner cannot fill the log; it must refuse fast
// rather than sleep, or an attacker parks goroutines until a real operator's
// claim queues behind them.
func TestAuth_ClaimAttemptsAreThrottled(t *testing.T) {
	s, code := managed(t, State{Fingerprint: "fp"})

	for i := range claimBurst {
		if rec := postForm(t, s, "/claim", "code=nope"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := postForm(t, s, "/claim", "code=nope"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d, want 429", claimBurst+1, rec.Code)
	}
	// And a correct code is refused too while the bucket is empty — the limit
	// is on attempts, not on failures, or it would not bound anything.
	if rec := postForm(t, s, "/claim", "code="+code); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a valid code during the cooldown = %d, want 429", rec.Code)
	}
}

func TestAuth_RegisterNeedsACSRFToken(t *testing.T) {
	s, _ := managed(t, State{Fingerprint: "fp"})
	sess := signIn(t, s)

	post := func(form string) int {
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: sess.ID})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if got := post("central_url=https://c.example"); got != http.StatusForbidden {
		t.Errorf("no csrf token = %d, want 403", got)
	}
	if got := post("central_url=https://c.example&csrf=nonsense"); got != http.StatusForbidden {
		t.Errorf("wrong csrf token = %d, want 403", got)
	}
	if got := post("central_url=https://c.example&csrf=" + sess.CSRF); got != http.StatusSeeOther {
		t.Errorf("correct csrf token = %d, want 303", got)
	}
}

// The token has to reach the form, or every submission is a forgery.
func TestAuth_FormsCarryTheCSRFToken(t *testing.T) {
	s, _ := managed(t, State{Fingerprint: "fp"})
	sess := signIn(t, s)

	// The wizard, and then the status page's separate "Re-register" form.
	if body := getAs(t, s, sess, "/").Body.String(); !strings.Contains(body, sess.CSRF) {
		t.Error("the wizard form carries no csrf token")
	}

	provisioned := &Server{
		Store: s.Store,
		State: func() State {
			return State{Fingerprint: "fp", GatewayUID: "uid", CentralURL: "https://c.example",
				Approval: "approved"}
		},
	}
	if body := getAs(t, provisioned, sess, "/").Body.String(); !strings.Contains(body, sess.CSRF) {
		t.Error("the status page's re-register form carries no csrf token")
	}
}

func TestAuth_LogoutEndsTheSession(t *testing.T) {
	s, _ := managed(t, State{Fingerprint: "fp"})
	sess := signIn(t, s)

	rec := postFormAs(t, s, sess, "/logout", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /logout = %d, want 303", rec.Code)
	}
	if body := getAs(t, s, sess, "/").Body.String(); !strings.Contains(body, `action="/claim"`) {
		t.Error("the session still works after signing out")
	}
}

// claim reset is the recovery path, so it has to invalidate what came before —
// including a cookie held by whoever the operator is locking out.
func TestAuth_ClaimResetRevokesLiveSessions(t *testing.T) {
	s, code := managed(t, State{Fingerprint: "fp"})
	sess := signIn(t, s)

	next, err := s.Store.ResetClaimCode()
	if err != nil {
		t.Fatalf("ResetClaimCode: %v", err)
	}

	if body := getAs(t, s, sess, "/").Body.String(); !strings.Contains(body, `action="/claim"`) {
		t.Error("a session survived claim reset")
	}
	if rec := postForm(t, s, "/claim", "code="+code); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old code still signs in: %d", rec.Code)
	}
	if rec := postForm(t, s, "/claim", "code="+next); rec.Code != http.StatusSeeOther {
		t.Errorf("the new code does not sign in: %d", rec.Code)
	}
}

// The stylesheet is on the claim page, so it cannot be behind the session.
func TestAuth_StaticIsOpenWhenUnclaimed(t *testing.T) {
	s, _ := managed(t, State{Fingerprint: "fp"})

	if rec := get(t, s, "/static/style.css"); rec.Code != http.StatusOK {
		t.Errorf("GET /static/style.css = %d, want 200", rec.Code)
	}
}

// File mode owns its configuration on disk: there is nothing to provision, so
// there is nothing to sign in to and the status page stays open.
func TestAuth_FileModeIsUnauthenticated(t *testing.T) {
	s := &Server{Version: "1.0.0", ConfigDir: "/opt/mailgw-go/config"}

	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `action="/claim"`) {
		t.Error("file mode rendered a claim form")
	}
	if !strings.Contains(body, "/opt/mailgw-go/config") {
		t.Error("file mode did not show the config directory")
	}
	// The shared footer must not claim an authentication this page does not
	// have.
	if strings.Contains(body, "It is authenticated") {
		t.Error("the file-mode page claims to be authenticated")
	}
}

func TestAuth_BearerGuardsMetricsAndReadyz(t *testing.T) {
	newServer := func(token string) *Server {
		return &Server{
			Metrics:      obs.New(),
			State:        readyState,
			MetricsToken: func() string { return token },
		}
	}

	withAuth := func(s *Server, path, header string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	for _, path := range []string{"/metrics", "/readyz"} {
		// Empty means open: that is what every scraper aimed at a firewalled
		// port sees today, and it is the only thing that can be true before the
		// first bundle applies.
		if got := withAuth(newServer(""), path, ""); got != http.StatusOK {
			t.Errorf("%s with no token configured = %d, want 200", path, got)
		}

		s := newServer("s3cret")
		if got := withAuth(s, path, ""); got != http.StatusUnauthorized {
			t.Errorf("%s with no credential = %d, want 401", path, got)
		}
		if got := withAuth(s, path, "Bearer wrong"); got != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token = %d, want 401", path, got)
		}
		// A token in a query parameter would be written to every access log
		// between the scraper and here, so it is deliberately not accepted.
		if got := withAuth(s, path+"?token=s3cret", ""); got != http.StatusUnauthorized {
			t.Errorf("%s with a query-parameter token = %d, want 401", path, got)
		}
		if got := withAuth(s, path, "Bearer s3cret"); got != http.StatusOK {
			t.Errorf("%s with the right token = %d, want 200", path, got)
		}
	}

	// A scraper arriving with no credential should be told to present one.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	newServer("s3cret").Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
}

// Liveness must never need a secret: a container runtime probing it has no way
// to present one, and a gateway killed for want of a token is a mail outage.
func TestAuth_HealthzIsOpenEvenWithATokenAndNoSession(t *testing.T) {
	s, _ := managed(t, State{Fingerprint: "fp"})
	s.MetricsToken = func() string { return "s3cret" }

	if rec := get(t, s, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

// A managed gateway with no store is not a thing main can build, but a nil
// MetricsToken is: file mode with no admin.json, and every existing test.
func TestAuth_NilMetricsTokenLeavesTheEndpointsOpen(t *testing.T) {
	s := &Server{State: readyState}

	if rec := get(t, s, "/metrics"); rec.Code != http.StatusOK {
		t.Errorf("GET /metrics = %d, want 200", rec.Code)
	}
	if rec := get(t, s, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", rec.Code)
	}
}
