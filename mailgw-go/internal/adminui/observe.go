package adminui

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
)

// The readiness reasons, as a fixed set.
//
// They are scraped and stored downstream, so they are stable strings rather
// than free text — and deliberately none of them echoes State.LastError, which
// is operator-visible untrusted content.
const (
	reasonUnprovisioned = "not provisioned"
	reasonNotApproved   = "not approved"
	reasonNoConfig      = "no configuration applied"
	reasonNotListening  = "SMTP is not listening"
)

// readyz answers whether this gateway should be sent traffic.
//
// It performs NO I/O — no store, no spool, no call to the console. Everything
// it reads is the snapshot the last successful poll left behind. That is the
// whole design: if readiness depended on reaching Central Management, a single
// console outage would turn every gateway in the fleet 503 at the same moment,
// every load balancer would drain them together, and a management-plane outage
// would become a mail outage.
//
// For the same reason a failed poll, a failed deploy of a NEW version, and a
// pending restart are all deliberately not conditions: in each case the gateway
// is still serving mail on a configuration that works, and that is what the
// question is asking.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	var st State
	if s.State != nil {
		st = s.State()
	}

	var reasons []string
	// A file-mode gateway owns its configuration, so provisioning and approval
	// are not merely satisfied — they are meaningless.
	if s.Store != nil {
		if st.GatewayUID == "" || st.CentralURL == "" {
			reasons = append(reasons, reasonUnprovisioned)
		}
		if st.Approval != approvalApproved {
			reasons = append(reasons, reasonNotApproved)
		}
		if st.AppliedVersion == nil {
			reasons = append(reasons, reasonNoConfig)
		}
	}
	if !st.Serving {
		reasons = append(reasons, reasonNotListening)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if len(reasons) == 0 {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
		return
	}

	// 503 rather than 500: it is what every load balancer and orchestrator
	// reads as "drain me and retry", which is exactly the request.
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "not ready",
		"reasons": reasons,
	})
}

// approvalApproved is Central Management's word for a gateway that may fetch a
// configuration. Every other value means "wait".
const approvalApproved = "approved"

// metrics serves the Prometheus exposition.
//
// Behind a bearer token when one is configured, and open when none is — a
// scraper needs a credential it can present without a browser, which is why it
// is not the session the rest of the UI runs on, and open-when-unset is what
// every install that firewalled this port already has. The exposition carries
// traffic volumes, queue depth and the running version, so the port belongs on
// a management network either way. See bearer in auth.go.
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	g := obs.Gauges{}
	if s.Gauges != nil {
		g = s.Gauges()
	}
	// Filled here rather than by every caller that builds a Gauges closure, so
	// build_info is right even when Gauges is nil.
	g.Version, g.Commit = s.Version, s.Commit

	// Rendered into a buffer first: a mid-write failure must not leave half an
	// exposition under a 200, which a scraper would parse as real data.
	var buf bytes.Buffer
	if err := s.Metrics.WritePrometheus(&buf, g); err != nil {
		s.log().Error("cannot render metrics", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}

// SpoolGauges fills the queue depths from sp, leaving QueueOK false when there
// is nothing to read — a managed gateway before its first apply, or a spool
// that cannot be listed.
//
// Both serveFile and serveManaged build the same Gauges closure, and the
// "omit rather than report zero" rule is easy to get wrong twice, so it lives
// here once.
func SpoolGauges(g *obs.Gauges, sp *queue.Spool) {
	if sp == nil {
		return
	}
	c, err := sp.LenAll()
	if err != nil {
		return
	}
	g.QueueOK = true
	g.QueueReady, g.QueueInflight = c.Ready, c.Inflight
	g.QueueQuarantine, g.QueueDead = c.Quarantine, c.Dead
	g.FailedEvents, g.RejectedEvents = c.FailedEvents, c.RejectedEvents
}
