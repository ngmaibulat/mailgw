// Package api is the Fiber v3 HTTP surface, and the composition root for a
// running logservice-fiber.
//
// # It is a port, not a rewrite
//
// Every decision here is logservice-go/internal/api's decision, reproduced. The
// route table, the per-route middleware, the response bodies, the status codes
// and the timeouts are the same; what differs is the framework underneath. Where
// Fiber's default disagrees with net/http's behaviour, the default is defeated
// and the reason is written at the point of defeat — those comments are the
// substance of this package, and the contract suite is what proves each one
// still holds.
//
// # The route table is the policy
//
// Middleware is wrapped around each route rather than declared as a chain, so
// App() reads as the access-control policy and a forgotten `s.auth` is a visible
// omission on one line.
//
// # What must not change
//
// Three callers were written against the Bun implementation, and two of them
// make irreversible decisions from a status code:
//
//   - mailgw-go/internal/events treats ANY 4xx as terminal and spills the event
//     to the gateway's disk rather than retrying. A transient failure here must
//     be a 5xx.
//   - mailgw-go/internal/attach turns any non-2xx, unparseable body, missing
//     `action` or unknown `action` value from /filter/md5 into an error, which
//     becomes SMTP 451 and defers real mail.
//   - webui-fastify proxies the searches verbatim and maps a non-2xx to 502.
//
// All of it is asserted by logservice-go/apitest, which this package runs
// against itself in contract_test.go and which logservice-go runs against its
// own. A difference between the two is a bug in whichever one moved.
package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ngmaibulat/mailgw/logservice-go/query"
	"github.com/ngmaibulat/mailgw/logservice-go/store"
)

const (
	// readTimeout is 10s here where logservice-go uses 30s, and that is the one
	// deliberate difference in this file.
	//
	// net/http splits the budget: ReadHeaderTimeout 10s for the headers,
	// ReadTimeout 30s for headers plus body. fasthttp has no header-only
	// timeout — ReadTimeout covers both — so one number has to serve for both,
	// and the choice is between leaving slowloris bounded three times looser
	// than the service beside this one, or giving a body 10s instead of 30s.
	// The largest legitimate body is capped at 1 MiB, and 10 seconds to deliver
	// 1 MiB is generous over any link a gateway runs on.
	//
	// Not on the wire, and nothing asserts it; it is in the README's
	// known-differences table.
	readTimeout  = 10 * time.Second
	writeTimeout = 30 * time.Second
	idleTimeout  = 60 * time.Second

	// queryTimeout bounds a single database call, so a slow COUNT(*) over a
	// large table cannot hold a connection past writeTimeout — at which point
	// the client has already given up and the work is pure waste.
	queryTimeout = 15 * time.Second

	// shutdownTimeout bounds the drain. In-flight inserts are short; anything
	// still running after this has already exceeded queryTimeout.
	shutdownTimeout = 10 * time.Second

	// maxBodyBytes bounds a request body. The largest legitimate body is a
	// /filter/md5 array, one small object per MIME part.
	maxBodyBytes = 1 << 20

	// hardBodyLimit is fasthttp's own ceiling, and it is NOT the cap above.
	//
	// Fiber's BodyLimit refuses at the protocol layer, before routing, with a
	// 413 — and the contract is a 400 on the three ingest routes and a 500 on
	// /filter/md5, which are decisions only a route can make. So the real cap
	// lives in readBody and this is a memory backstop set well above it: a body
	// between 1 and 8 MiB is refused by readBody with the right status, and one
	// past 8 MiB is refused by fasthttp before this process buffers it.
	//
	// The gap is deliberate. Setting them equal would hand every oversized body
	// to fasthttp and produce the 413 this whole arrangement exists to avoid.
	hardBodyLimit = 8 << 20
)

// Server answers the HTTP API.
//
// Field-for-field the same as logservice-go's, because cmd/ wires both the same
// way and an operator swapping the images must not have to change anything but
// the tag.
type Server struct {
	// DB is the request-serving pool. Multi-statement support is deliberately
	// off on it; see logservice-go's package db.
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

// App builds the route table.
//
// Read the wrappers, not just the paths: `s.auth` is the API-key gate and its
// absence is deliberate in exactly three places — GET / is the healthcheck and
// has always been open, and /healthz and /readyz are probes that carry no
// credential.
func (s *Server) App() *fiber.App {
	app := fiber.New(fiber.Config{
		// Both of these exist to match net/http's ServeMux, whose defaults are
		// the opposite of Fiber's. ServeMux is case-sensitive and does not fold
		// a trailing slash into an exact pattern, so without these two
		// `GET /API/Connection` and `POST /api/queue/` would be answered here
		// and 404'd next door.
		CaseSensitive: true,
		StrictRouting: true,

		// Streaming, so an oversized body is refused by readBody after routing
		// rather than by fasthttp before it. See hardBodyLimit.
		StreamRequestBody: true,
		BodyLimit:         hardBodyLimit,

		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,

		ErrorHandler: s.errorHandler,

		// ServerHeader deliberately unset: net/http announces nothing, and the
		// differential test compares the full header set.
		//
		// SkipUnmatchedRoutes deliberately unset (false, the default). Turning
		// it on answers 404/405 BEFORE the middleware chain runs, which would
		// bypass the catch-all below and restore Fiber's 405.
	})

	// The panic net, registered first so it wraps every route below it.
	app.Use(s.recoverPanics)

	// The health check. No auth, matching the Bun service: a monitoring probe
	// has no credential, and the e2e suite asserts this body byte-for-byte.
	// app.Get("/") is an exact match in Fiber — only app.Use is prefix-matched —
	// so this cannot swallow /nope.
	app.Get("/", s.handleRoot)

	// Ingest. Only /api/delivery validates; the other two default every absent
	// field. That asymmetry is the contract every deployed gateway was built
	// against — see logservice-go's package validate.
	app.Post("/api/connection", s.auth(s.handlePostConnection))
	app.Post("/api/queue", s.auth(s.handlePostQueue))
	app.Post("/api/delivery", s.auth(s.handlePostDelivery))

	// Search. Read-only; the console proxies these verbatim.
	app.Get("/api/connection", s.auth(s.handleSearchConnection))
	app.Get("/api/delivery", s.auth(s.handleSearchDelivery))
	app.Get("/api/transaction", s.auth(s.handleSearchTransaction))
	app.Get("/api/hashlookup", s.auth(s.handleSearchHashLookup))

	// The attachment blocklist check.
	app.Post("/filter/md5", s.auth(s.handleFilterMD5))

	// Liveness and readiness. Both open: a probe has no credential, and neither
	// reveals anything an unauthenticated caller could act on.
	app.Get("/healthz", s.handleHealthz)
	app.Get("/readyz", s.handleReadyz)

	// The catch-all, and the first place Fiber has to be talked out of a
	// default.
	//
	// Registered LAST and through Use, so it matches every method and every path
	// nothing above claimed. That is what suppresses Fiber's automatic
	// 405 + Allow: the router returns as soon as a route matches and never
	// reaches its ErrMethodNotAllowed branch. It is the same trick
	// logservice-go plays with mux.HandleFunc("/", ...), and it is why a wrong
	// method here is the plain-text 404 the Bun service answered.
	//
	// errorHandler covers the same three codes independently, so a Fiber
	// upgrade that changed the matching order would still produce the right
	// bytes — but the contract asserts the ABSENCE of an Allow header, which
	// only this path gives, so a regression here is still caught.
	app.Use(s.handleNotFound)

	return app
}

// errorHandler is the last thing between a framework-generated error and the
// wire.
//
// It exists because Fiber has opinions net/http does not: 405 with an Allow
// header for a known path under the wrong method, 501 for a method it does not
// recognise, and its own JSON shape for anything a handler returns. ServeMux
// answers its catch-all for the first two, so all three land here as the same
// plain-text 404.
//
// Anything else is a handler that returned a non-nil error. No handler here
// does — they all write and return nil, exactly as their net/http counterparts
// write and return nothing — so reaching this is a bug, and it is reported as a
// 500 rather than as whatever Fiber would have said.
func (s *Server) errorHandler(c fiber.Ctx, err error) error {
	var fe *fiber.Error
	if errors.As(err, &fe) {
		switch fe.Code {
		case fiber.StatusNotFound, fiber.StatusMethodNotAllowed, fiber.StatusNotImplemented:
			return s.handleNotFound(c)
		}
	}
	s.log().Error("unhandled error serving request",
		"method", c.Method(), "path", c.Path(), "err", err)
	return writeJSON(c, fiber.StatusInternalServerError, bodyError)
}

// ListenAndServe opens the listener and serves until ctx is cancelled.
//
// The listener is opened SYNCHRONOUSLY, as in logservice-go, so that a port
// already in use is a startup error the operator sees rather than a goroutine
// failing after main has reported success — and so a test can ask for :0 and
// read the port back from Addr(). app.Listen would do neither, which is why
// this uses app.Listener.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	bound := ln.Addr().String()
	s.addr.Store(&bound)

	app := s.App()

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.Listener(ln, fiber.ListenConfig{
			// slog is the only output stream this service has; Fiber's ASCII
			// banner on stdout would be the one line in the log that is not
			// structured.
			DisableStartupMessage: true,
			ShutdownTimeout:       shutdownTimeout,
		})
	}()

	s.log().Info("listening", "addr", bound, "version", s.Version,
		"auth", s.APIKey != "")

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Deliberately not ListenConfig.GracefulContext: this keeps the shape
		// identical to logservice-go's, including returning ctx.Err() so main's
		// context.Canceled check still turns `docker compose down` into exit 0.
		if err := app.ShutdownWithTimeout(shutdownTimeout); err != nil {
			s.log().Warn("shutdown did not complete cleanly", "err", err)
		}
		return ctx.Err()
	}
}

// dbCtx bounds one database call, without outliving the request it serves.
//
// The parent is c.RequestCtx() rather than c.Context(). In Fiber v3 the Ctx
// itself satisfies context.Context, but c.Context() returns context.Background()
// unless SetContext was called, so a query parented there would be cancellable
// by nothing at all.
//
// What c.RequestCtx() gives, and what it does not, is worth stating plainly
// because it is a real difference from net/http:
//
//   - fasthttp's RequestCtx.Done() is closed when the SERVER shuts down, so an
//     in-flight query is cancelled by a shutdown, as it is next door.
//   - It is NOT closed when the client disconnects, and Deadline() is a
//     documented no-op. A query for a caller who has already hung up therefore
//     runs to the 15s ceiling instead of being cut short. The ceiling still
//     holds, so this bounds resources; it just does not reclaim them early.
//
// The returned context must not outlive the handler — fasthttp recycles the
// RequestCtx — which every call site satisfies with `defer cancel()`.
func dbCtx(c fiber.Ctx) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.RequestCtx(), queryTimeout)
}
