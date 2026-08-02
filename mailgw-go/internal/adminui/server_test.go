package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "gw"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func postForm(t *testing.T, s *Server, path string, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// signIn mints a session directly in the store, which this test file can do
// because it is in package adminui — so authenticating a test does not need a
// test-only exported API, and does not depend on the claim flow it is often
// there to set up.
func signIn(t *testing.T, s *Server) *store.AdminSession {
	t.Helper()
	sess, err := s.Store.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return sess
}

func getAs(t *testing.T, s *Server, sess *store.AdminSession, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// postFormAs appends the session's CSRF token, so a test that is not about CSRF
// does not have to think about it. A test that IS about CSRF builds its own
// request.
func postFormAs(t *testing.T, s *Server, sess *store.AdminSession, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(form+"&csrf="+url.QueryEscape(sess.CSRF)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// Healthz must stay a true liveness signal, so it does not touch the store.
func TestAdminUI_HealthzNeedsNoStore(t *testing.T) {
	rec := get(t, &Server{}, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// A nil store means file mode: no form, and nothing about registration.
func TestAdminUI_FileModeHasNoRegisterForm(t *testing.T) {
	s := &Server{Version: "1.0.0", ConfigDir: "/opt/mailgw-go/config"}

	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if strings.Contains(body, `action="/register"`) {
		t.Error("file mode rendered a registration form")
	}
	if !strings.Contains(body, "/opt/mailgw-go/config") {
		t.Error("file mode did not show the config directory")
	}
}

// Registering is meaningless in file mode and must not half-work.
func TestAdminUI_FileModeRefusesRegister(t *testing.T) {
	rec := postForm(t, &Server{}, "/register", "central_url=https://c.example")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAdminUI_UnprovisionedRendersWizardWithFingerprint(t *testing.T) {
	s := &Server{
		Store: testStore(t),
		State: func() State { return State{Fingerprint: "abc123fingerprint"} },
	}

	rec := getAs(t, s, signIn(t, s), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `action="/register"`) {
		t.Error("wizard has no registration form")
	}
	// The fingerprint exists before registration precisely so it can be
	// pre-approved in the console.
	if !strings.Contains(body, "abc123fingerprint") {
		t.Error("wizard does not show the fingerprint")
	}
}

func TestAdminUI_ProvisionedRendersStatus(t *testing.T) {
	applied, desired := 3, 4
	s := &Server{
		Store: testStore(t),
		State: func() State {
			return State{
				Fingerprint: "fp", GatewayUID: "uid-1",
				CentralURL: "https://console.example:4000",
				Approval:   "approved",
				//nolint:gosec // test fixture
				AppliedVersion: &applied, DesiredVersion: &desired,
			}
		},
	}

	rec := getAs(t, s, signIn(t, s), "/")
	body := rec.Body.String()

	if !strings.Contains(body, "https://console.example:4000") {
		t.Error("status page does not show the central URL")
	}
	if !strings.Contains(body, "v3") || !strings.Contains(body, "v4") {
		t.Error("status page does not show applied/desired versions")
	}
	// Nothing left to watch for once approved.
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("an approved gateway should not keep meta-refreshing")
	}
}

// The refresh is what makes approval appear without a manual reload, which is
// the whole point of the pending screen.
func TestAdminUI_PendingStatusRefreshes(t *testing.T) {
	s := &Server{
		Store: testStore(t),
		State: func() State {
			return State{Fingerprint: "fp", GatewayUID: "uid-1",
				CentralURL: "https://c.example", Approval: "pending"}
		},
	}

	if !strings.Contains(getAs(t, s, signIn(t, s), "/").Body.String(), `http-equiv="refresh"`) {
		t.Error("a pending gateway should meta-refresh")
	}
}

// An insecure escape hatch that is invisible is worse than none.
func TestAdminUI_InsecureTLSIsSurfacedOnlyWhenSet(t *testing.T) {
	newServer := func(insecure bool) *Server {
		return &Server{
			Store: testStore(t),
			State: func() State {
				return State{Fingerprint: "fp", GatewayUID: "uid",
					CentralURL: "https://c.example", Approval: "approved",
					InsecureTLS: insecure}
			},
		}
	}

	shown := func(insecure bool) string {
		s := newServer(insecure)
		return getAs(t, s, signIn(t, s), "/").Body.String()
	}

	if body := shown(true); !strings.Contains(body, "TLS verification is disabled") {
		t.Error("insecure TLS is not surfaced on the status page")
	}
	if body := shown(false); strings.Contains(body, "TLS verification is disabled") {
		t.Error("the insecure warning appears when verification is enabled")
	}
}

func TestAdminUI_RegisterRejectsBadURLWithoutCallingTheHook(t *testing.T) {
	called := 0
	s := &Server{
		Store:    testStore(t),
		State:    func() State { return State{} },
		Register: func(context.Context, Settings) error { called++; return nil },
	}

	sess := signIn(t, s)
	for _, form := range []string{
		"central_url=",
		"central_url=ftp://console.example",
		"central_url=not-a-url",
		"central_url=https://",
	} {
		rec := postFormAs(t, s, sess, "/register", form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", form, rec.Code)
		}
	}
	if called != 0 {
		t.Errorf("Register was called %d times for invalid input", called)
	}
}

func TestAdminUI_RegisterNormalizesAndRedirects(t *testing.T) {
	var got Settings
	s := &Server{
		Store:    testStore(t),
		State:    func() State { return State{} },
		Register: func(_ context.Context, set Settings) error { got = set; return nil },
	}

	rec := postFormAs(t, s, signIn(t, s), "/register",
		"central_url=https://console.example:4000/&insecure_tls=1")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	if got.CentralURL != "https://console.example:4000" {
		t.Errorf("central url = %q, want the trailing slash stripped", got.CentralURL)
	}
	if !got.InsecureTLS {
		t.Error("the insecure checkbox was not carried through")
	}
}

// The operator must see why it failed, with their input preserved.
func TestAdminUI_RegisterSurfacesHookError(t *testing.T) {
	s := &Server{
		Store:    testStore(t),
		State:    func() State { return State{} },
		Register: func(context.Context, Settings) error { return errors.New("connection refused") },
	}

	rec := postFormAs(t, s, signIn(t, s), "/register", "central_url=https://console.example:4000")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "connection refused") {
		t.Error("the failure reason was not shown")
	}
	if !strings.Contains(body, "https://console.example:4000") {
		t.Error("the submitted URL was not preserved for a retry")
	}
}

// Catching an unreadable bundle here turns an opaque TLS failure into a
// legible form error.
func TestAdminUI_RegisterRejectsUnreadableCABundle(t *testing.T) {
	called := 0
	s := &Server{
		Store:    testStore(t),
		State:    func() State { return State{} },
		Register: func(context.Context, Settings) error { called++; return nil },
	}

	rec := postFormAs(t, s, signIn(t, s), "/register",
		"central_url=https://c.example&ca_file=/nonexistent/ca.pem")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if called != 0 {
		t.Error("Register was called despite an unreadable CA bundle")
	}
}

// State from the console is untrusted text that lands in HTML.
func TestAdminUI_EscapesUntrustedText(t *testing.T) {
	s := &Server{
		Store: testStore(t),
		State: func() State {
			return State{
				Fingerprint: "fp", GatewayUID: "uid",
				CentralURL: "https://c.example", Approval: "approved",
				ApplyError: `<script>alert(1)</script>`,
			}
		},
	}

	body := getAs(t, s, signIn(t, s), "/").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("apply_error was rendered unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("expected the escaped form to appear")
	}
}

// GET /{$} is an exact match, so the dashboard does not answer every path.
func TestAdminUI_UnknownPathIs404(t *testing.T) {
	s := &Server{Store: testStore(t), State: func() State { return State{} }}

	if rec := getAs(t, s, signIn(t, s), "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAdminUI_ServesEmbeddedStylesheet(t *testing.T) {
	rec := get(t, &Server{}, "/static/style.css")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("stylesheet is empty")
	}
}

func TestNormalizeCentralURL(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "plain https", in: "https://console.example:4000", want: "https://console.example:4000"},
		{name: "trailing slash", in: "https://console.example:4000/", want: "https://console.example:4000"},
		{name: "several trailing slashes", in: "https://c.example///", want: "https://c.example"},
		{name: "path prefix kept", in: "https://c.example/mgmt/", want: "https://c.example/mgmt"},
		{name: "surrounding space", in: "  https://c.example  ", want: "https://c.example"},
		{name: "http allowed", in: "http://10.0.0.5:4000", want: "http://10.0.0.5:4000"},
		// Credentials would be echoed into every log line.
		{name: "credentials stripped", in: "https://u:p@c.example", want: "https://c.example"},
		{name: "query stripped", in: "https://c.example?x=1", want: "https://c.example"},
		{name: "fragment stripped", in: "https://c.example#frag", want: "https://c.example"},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "no scheme", in: "console.example:4000", wantErr: true},
		{name: "wrong scheme", in: "ftp://c.example", wantErr: true},
		{name: "no host", in: "https://", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeCentralURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("NormalizeCentralURL(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeCentralURL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeCentralURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
