// Package api is the HTTP surface, and the composition root for a running
// logservice.
//
// # The route table is the policy
//
// Middleware is wrapped around each route rather than around the mux, following
// mailgw-go/internal/adminui: Handler() can then be read as the access-control
// policy, and a forgotten wrapper is a visible omission on one line rather than
// an invisible gap in an ordering.
//
// # What must not change
//
// This service has three callers that were written against the Bun
// implementation, and two of them make irreversible decisions from a status
// code:
//
//   - mailgw-go/internal/events treats ANY 4xx as terminal and spills the event
//     to the gateway's disk rather than retrying. A transient failure here must
//     be a 5xx.
//   - mailgw-go/internal/attach turns any non-2xx, unparseable body, missing
//     `action` or unknown `action` value from /filter/md5 into an error, which
//     becomes SMTP 451 and defers real mail.
//   - webui-fastify proxies the searches verbatim and maps a non-2xx to 502.
//
// The exact bodies — {"status":"OK"}, {"status":"Fail"}, {"action":"allow"} —
// and the plain-text 404 are asserted byte-for-byte by
// tests/api/logservice.e2e.test.ts, which runs unmodified against this binary.
package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ngmaibulat/mailgw/logservice-go/internal/query"
	"github.com/ngmaibulat/mailgw/logservice-go/internal/store"
)

// Timeouts. The Bun service set none of these; Bun.serve has its own defaults
// and no per-request query deadline at all.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second

	// queryTimeout bounds a single database call, so a slow COUNT(*) over a
	// large table cannot hold a connection past writeTimeout — at which point
	// the client has already given up and the work is pure waste.
	queryTimeout = 15 * time.Second

	// maxBodyBytes bounds a request body. The largest legitimate body is a
	// /filter/md5 array, one small object per MIME part.
	maxBodyBytes = 1 << 20
)

// Server answers the HTTP API.
type Server struct {
	// DB is the request-serving pool. Multi-statement support is deliberately
	// off on it; see internal/db.
	DB *sql.DB

	// APIKey is compared against X-API-Key. EMPTY MEANS OPEN — every request is
	// accepted — which is the Bun service's behaviour and what the dev compose
	// stack relies on (API_KEY defaults to empty there). Changing this to
	// fail-closed would lock out every existing deployment that never set it.
	APIKey string

	// Version is reported by /healthz.
	Version string

	Log *slog.Logger

	// ready reports whether migrations have completed. Read by /readyz, set
	// once by MarkReady before the listener binds.
	ready atomic.Bool

	// addr holds the bound address once ListenAndServe has opened the listener,
	// so a test can ask for :0 and find the port.
	addr atomic.Pointer[string]
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// MarkReady records that the schema is migrated and the service can serve.
func (s *Server) MarkReady() { s.ready.Store(true) }

// Addr returns the bound listen address, or "" before the listener opens.
func (s *Server) Addr() string {
	if p := s.addr.Load(); p != nil {
		return *p
	}
	return ""
}

func (s *Server) searcher() query.Searcher { return query.Searcher{DB: s.DB} }
func (s *Server) store() store.Store       { return store.Store{DB: s.DB} }

// Handler builds the route table.
//
// Read the wrappers, not just the paths: `s.auth` is the API-key gate and its
// absence on a route is deliberate in exactly one place — GET / is the
// healthcheck and has always been open.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The health check. No auth and no error wrapper, matching the Bun service:
	// a monitoring probe has no credential, and the e2e suite asserts this body
	// byte-for-byte.
	mux.HandleFunc("GET /{$}", s.handleRoot)

	// Ingest. Only /api/delivery validates; the other two default every absent
	// field. That asymmetry is the contract every deployed gateway was built
	// against — see internal/validate.
	mux.HandleFunc("POST /api/connection", s.auth(s.handlePostConnection))
	mux.HandleFunc("POST /api/queue", s.auth(s.handlePostQueue))
	mux.HandleFunc("POST /api/delivery", s.auth(s.handlePostDelivery))

	// Search. Read-only; the console proxies these verbatim.
	mux.HandleFunc("GET /api/connection", s.auth(s.handleSearchConnection))
	mux.HandleFunc("GET /api/delivery", s.auth(s.handleSearchDelivery))
	mux.HandleFunc("GET /api/transaction", s.auth(s.handleSearchTransaction))
	mux.HandleFunc("GET /api/hashlookup", s.auth(s.handleSearchHashLookup))

	// The attachment blocklist check.
	mux.HandleFunc("POST /filter/md5", s.auth(s.handleFilterMD5))

	// Liveness and readiness. Both new, both open: a probe has no credential,
	// and neither reveals anything an unauthenticated caller could act on.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// The catch-all. Registered last and matched only when nothing above did,
	// it reproduces the Bun service's plain-text 404 body exactly.
	mux.HandleFunc("/", s.handleNotFound)

	return s.recoverPanics(mux)
}

// ListenAndServe opens the listener and serves until ctx is cancelled.
//
// The listener is opened SYNCHRONOUSLY so that a port already in use is a
// startup error the operator sees, rather than a goroutine failing after main
// has reported success.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	bound := ln.Addr().String()
	s.addr.Store(&bound)

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		// Bridge net/http's own logger into slog, so a TLS handshake error or a
		// malformed request line lands in the same stream as everything else
		// instead of on a bare stderr line with no fields.
		ErrorLog: slog.NewLogLogger(s.log().Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	s.log().Info("listening", "addr", bound, "version", s.Version,
		"auth", s.APIKey != "")

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// A bounded drain. In-flight inserts are short; anything still running
		// after this has already exceeded queryTimeout.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.log().Warn("shutdown did not complete cleanly", "err", err)
		}
		return ctx.Err()
	}
}

// dbCtx bounds one database call, without outliving the request it serves.
func dbCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), queryTimeout)
}
