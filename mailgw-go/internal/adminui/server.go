// Package adminui is the gateway's local provisioning and status UI.
//
// A gateway can boot with no configuration at all. The wizard this package
// serves is how an operator points it at a Central Management console; after
// that it is the status page that answers "what is this box doing, and why is
// it not relaying mail?".
//
// # Access control
//
// A managed gateway generates a CLAIM CODE on first boot and writes it to its
// log. Until somebody presents that code the UI shows one page — this
// gateway's version and fingerprint, and a field to type the code into — and
// every state-changing route refuses. Presenting it mints a session; the
// session cookie and a per-session CSRF token are what the wizard runs on
// afterwards. `mailgw-go claim reset` mints a new code and revokes every
// session, and needs filesystem access to the data directory, which is already
// game over for this gateway.
//
// The code is not consumed by being used. A code that worked exactly once,
// with a cookie as the only other credential, would leave the node reachable by
// exactly one browser for ever. What must not reopen is the unauthenticated
// window, and that closes the moment a code exists.
//
// This does not retire the firewall. The listener still binds a real network
// interface by default, because a wizard reachable only over loopback is
// useless on a headless server, and on an edge node the process is root with no
// port mapping to narrow — so reducing the attack surface is still worth doing
// even now that the surface is authenticated.
// deploy/gateway/05-firewall.sh remains required.
//
// In file mode (Store == nil) none of this applies: that gateway owns its
// configuration on disk, has nothing to provision, and the UI is a read-only
// status page.
//
// The listener is plain HTTP, deliberately. Serving it with the self-signed
// pair internal/tlsx already generates would authenticate nothing and would
// teach an operator to click through a certificate warning on the exact page
// where they type a secret. Real TLS here means real certificate paths on the
// gateway's own filesystem, never in a bundle, and is tracked as -admin-tls.
//
// Stdlib only — net/http, html/template and embed. The module's dependency
// budget does not need a web framework, a session library or a CSRF package.
package adminui

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// State is a snapshot of the gateway's relationship with Central Management,
// as the status page renders it.
type State struct {
	Fingerprint string
	GatewayUID  string
	CentralURL  string
	InsecureTLS bool
	CAFile      string

	// Approval is the console's word for this gateway: pending, approved,
	// rejected or revoked. Empty before the first successful poll.
	Approval string
	// LastError is why the most recent exchange with the console failed, if it
	// did. Rendered to the operator, so it must be treated as untrusted text.
	LastError string

	AppliedVersion *int
	DesiredVersion *int
	ApplyError     string

	// Serving reports whether the SMTP listeners are up. A managed gateway
	// refuses to listen until a configuration applies, and "why is this box not
	// relaying mail?" is the question this page exists to answer.
	Serving bool
	// RestartRequired names what the running configuration changed that only a
	// restart can pick up — the relay table, the listeners, TLS, the spool.
	RestartRequired []string
}

// Settings is what the wizard form collects.
type Settings struct {
	CentralURL  string
	InsecureTLS bool
	CAFile      string
}

// Server serves the admin UI.
type Server struct {
	// Store is nil in file mode, which is what selects the read-only page:
	// a file-mode gateway is not centrally managed and must not generate an
	// identity it will never use.
	Store   *store.Store
	Version string
	Commit  string
	// ConfigDir is shown in file mode.
	ConfigDir string
	// SpoolFn returns the outbound spool, or nil when the gateway is not
	// serving mail yet. It is a function rather than a field because a managed
	// gateway creates its spool when its first configuration applies, which is
	// after this server is already answering requests — a plain field would be
	// written by the pull loop while a request goroutine read it.
	SpoolFn func() *queue.Spool
	Log     *slog.Logger

	// Register and State are injected by main so this package never has to
	// import the registration or polling logic.
	Register func(ctx context.Context, s Settings) error
	State    func() State

	// Metrics is the process-wide counter set. Nil is tolerated: /metrics then
	// exposes zeroes and the gauge series, which is still a useful answer to
	// "which build is this and is it serving?".
	Metrics *obs.Metrics
	// Gauges reads the values that are sampled rather than counted. A function
	// for the same reason as SpoolFn: a managed gateway's spool and applied
	// version do not exist when this server starts answering requests.
	Gauges func() obs.Gauges

	// MetricsToken is the bearer token /metrics and /readyz require, or "" for
	// no token, which leaves both open.
	//
	// A function rather than a field for the same reason as the two above, and
	// more sharply: it arrives in a configuration bundle, and this server is
	// already answering requests before the first bundle applies. Nil is
	// tolerated and means "no token" — which is what file mode with no
	// admin.json, and a managed node before its first apply, both are.
	MetricsToken func() string

	// Claim-attempt bucket. On the Server rather than inside Handler, which is
	// rebuilt per call, and in memory rather than in the store — see
	// allowClaimAttempt.
	claimMu     sync.Mutex
	claimTokens float64
	claimLast   time.Time

	// addr is what ListenAndServe actually bound, which is not what it was
	// asked for whenever it was asked for port 0.
	//
	// An atomic because ListenAndServe runs on its own goroutine while the
	// control API answers Status from a request goroutine. Nil-safe: an unset
	// pointer reads as "not listening", which is the truthful answer before
	// ListenAndServe has run and after it has returned.
	addr atomic.Pointer[string]
}

// Addr is the address the admin UI is listening on, or "" before it binds.
//
// It exists for the same reason node.Status reports the SMTP listeners rather
// than the configured ones: an address may be requested as :0, and a caller
// that cannot discover the bound port cannot reach /metrics, /readyz or
// /healthz at all.
func (s *Server) Addr() string {
	if a := s.addr.Load(); a != nil {
		return *a
	}
	return ""
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler builds the routing table.
//
// The middleware is wrapped around each route rather than around the mux, so
// this table IS the access-control policy and can be read as one. A route added
// later that forgets s.session is a visible omission here; the same route added
// under a middleware that decides by path prefix would be an invisible hole.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Unauthenticated and deliberately free of any database access, so it
	// stays a true liveness signal even when the store is unhappy. A container
	// runtime probing it must never need a secret.
	mux.HandleFunc("GET /healthz", s.healthz)
	// Open when no token is configured, which is what every existing scraper
	// and load-balancer probe sees today.
	mux.HandleFunc("GET /readyz", s.bearer(s.readyz))
	mux.HandleFunc("GET /metrics", s.bearer(s.metrics))
	// Open: it is the stylesheet, and the claim page needs it.
	mux.Handle("GET /static/", http.FileServerFS(assets))
	// Open by definition — this route is the sign-in.
	mux.HandleFunc("POST /claim", s.handleClaim)
	mux.HandleFunc("POST /logout", s.session(s.handleLogout))
	mux.HandleFunc("POST /register", s.session(s.handleRegister))
	// Session-AWARE rather than session-gated: with no session it renders the
	// claim page. A 403 here would leave an operator with nowhere to sign in,
	// and would hide the fingerprint that exists to be pre-approved.
	//
	// {$} is an exact match, so /anything-else 404s instead of rendering the
	// dashboard at every path.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return s.recoverPanics(mux)
}

// ListenAndServe runs the UI until ctx is cancelled.
//
// The listener is opened synchronously so that "port already in use" is a
// startup failure rather than a log line — in managed mode this is the only
// thing the process does, and a gateway that silently has no UI cannot be
// provisioned at all.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	bound := ln.Addr().String()
	s.addr.Store(&bound)
	// Cleared on the way out so Addr() cannot keep pointing at a socket that is
	// no longer accepting.
	defer s.addr.Store(nil)

	srv := &http.Server{
		Handler: s.Handler(),
		// Bounded because this is a listener on a real network.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log().Handler(), slog.LevelWarn),
	}

	go func() {
		<-ctx.Done()
		// Short, and deliberately shorter than the SMTP drain: the admin UI
		// has no long-lived requests, and it must not add to the container's
		// stop budget.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// The bound address, not the requested one: they differ whenever the port
	// was 0, and the log line naming ":0" tells an operator nothing.
	s.log().Info("admin UI listening", "addr", bound)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// recoverPanics keeps a template or handler bug from taking the process down.
// In managed mode the process is the UI, so a panic here would strand the
// gateway with no way to provision it.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log().Error("admin UI panic", "path", r.URL.Path, "err", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
