package api

import (
	"context"
	"net/http"
	"time"
)

// The reasons /readyz can report. A fixed set, never an error string from the
// driver: this endpoint is unauthenticated, and a database error can carry a
// host name, a user name or a schema name. mailgw-go's /readyz makes the same
// choice for the same reason.
const (
	reasonMigrating = "schema not migrated"
	reasonNoDB      = "database unreachable"
)

// readyProbeTimeout bounds the readiness ping. Short on purpose: a probe that
// blocks for fifteen seconds has already failed as far as any orchestrator is
// concerned, and holding the connection makes a struggling database worse.
const readyProbeTimeout = 2 * time.Second

// handleHealthz is liveness. It performs no I/O and answers 200 as long as the
// process can serve a request at all.
//
// Deliberately separate from /readyz: a liveness probe that failed on a database
// outage would have the orchestrator restart every replica during exactly the
// incident where restarting helps least.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.Version,
	})
}

// handleReadyz reports whether this instance can actually serve.
//
// Ready means: migrations have completed, and the pool answers. The second half
// does perform I/O, unlike mailgw-go's /readyz — the difference is that a
// gateway's readiness must not depend on reaching its console (one console
// outage would 503 the whole fleet and turn a management failure into a mail
// failure), whereas a logservice that cannot reach its database can do nothing
// at all. Answering 200 in that state would keep a load balancer sending it
// audit events that it can only 500.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	var reasons []string

	if !s.ready.Load() {
		reasons = append(reasons, reasonMigrating)
	}

	// A nil pool is unreachable in a wired-up serve, but this endpoint must
	// answer 503 rather than panic in every state it can be reached in — it is
	// the endpoint an operator hits precisely when something is wrong, and a
	// panic here would be recovered into a 500 that says nothing.
	if s.DB == nil {
		reasons = append(reasons, reasonNoDB)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), readyProbeTimeout)
		defer cancel()
		if err := s.DB.PingContext(ctx); err != nil {
			// Logged with the real error; reported without it.
			s.log().Warn("readiness probe could not reach the database", "err", err)
			reasons = append(reasons, reasonNoDB)
		}
	}

	if len(reasons) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "unavailable",
			"reasons": reasons,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}
