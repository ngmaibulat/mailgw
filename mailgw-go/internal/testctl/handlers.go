package testctl

import (
	"net/http"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/node"
)

// applyBundle makes a configuration bundle the running configuration.
//
// The body is the bundle document verbatim — byte-identical to what the console
// serves from GET /agent/config. That is deliberate and is what keeps this
// honest: a test drives the same wire format a real deployment does, so a change
// to the bundle shape breaks the suite instead of quietly diverging from it.
//
// 400 for a bundle that will not apply, and the compiler's own message with it.
// Not 422: the caller sent something the gateway cannot use, and every test
// harness in the world already special-cases 400.
func (s *Server) applyBundle(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	applied, err := s.Control.ApplyBundle(r.Context(), raw)
	if err != nil {
		// The gateway is still serving its last good configuration here; that is
		// the existing fail-closed contract and this path does not change it.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, applied)
}

// applyProfiles composes a bundle from the profile texts an operator would paste
// into the console, then applies it.
//
// Sugar over applyBundle, not a second format: the composed document goes
// through the same ApplyBundle. It exists because tests/provision.ts already
// carries these texts as inline constants, so a test can hand over the same
// strings rather than learning the bundle wire shape.
func (s *Server) applyProfiles(w http.ResponseWriter, r *http.Request) {
	var p node.Profiles
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	raw, err := p.Bundle()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	applied, err := s.Control.ApplyBundle(r.Context(), raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, applied)
}

// showConfig returns the applied bundle, unredacted.
//
// Unredacted because this is a build whose purpose is to be inspected: a test
// asserting on a relay password it just injected cannot do that against
// "[redacted]". `mailgw-go config show` keeps redacting, since that is the
// command the docs tell an operator to paste into an issue.
func (s *Server) showConfig(w http.ResponseWriter, _ *http.Request) {
	c, err := s.Control.AppliedBundle()
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CachedBundle{
		VersionID: c.VersionID,
		Version:   c.Version,
		SHA256:    c.SHA256,
		Bundle:    c.Bundle,
	})
}

// status reports what the node is doing, including the addresses SMTP actually
// bound — which is the only way to find the port after asking for :0.
func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Control.Status())
}

// enrollRequest points this gateway at a console.
type enrollRequest struct {
	CentralURL  string `json:"central_url"`
	InsecureTLS bool   `json:"insecure_tls"`
	CAFile      string `json:"ca_file"`
}

// enroll registers this gateway with Central Management.
//
// It replaces the one step of provisioning that had no automation: the wizard
// form behind M12's claim code. Everything after it is unchanged — the console
// still lands the row pending, and an operator or a test still has to approve
// the fingerprint, assign profiles and deploy.
//
// 502 rather than 400 when the console refuses or cannot be reached: the request
// was well-formed and the failure is upstream, and a test that cannot tell those
// apart will spend its time debugging the wrong end.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.CentralURL == "" {
		writeErr(w, http.StatusBadRequest, "central_url is required")
		return
	}

	err := s.Control.Enroll(r.Context(), adminui.Settings{
		CentralURL:  req.CentralURL,
		InsecureTLS: req.InsecureTLS,
		CAFile:      req.CAFile,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// The fingerprint and uid are what a test needs next, to approve this
	// gateway on the console.
	writeJSON(w, http.StatusOK, s.Control.Status())
}

// reset returns the node to the state a fresh data volume would give it.
//
// It cannot stop the gateway serving and does not claim to: listeners, the relay
// table and the spool are bound for the life of the process. The response says
// so rather than leaving a caller to discover it, because a test that believed
// otherwise would get a false pass.
func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	if err := s.Control.Reset(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reset":            true,
		"restart_required": true,
		"note": "the configuration cache and the console URL are cleared; " +
			"listeners and the spool stay bound until this process restarts",
	})
}

// queue lists every envelope in the spool.
func (s *Server) queue(w http.ResponseWriter, _ *http.Request) {
	entries, err := s.Control.Queue()
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if entries == nil {
		entries = []node.QueueEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// flushRequest names envelopes to make due now; empty means the whole ready
// queue.
type flushRequest struct {
	UUIDs []string `json:"uuids"`
}

// flush makes envelopes due now and wakes the delivery scheduler.
//
// This is what replaces sleeping through a retry backoff in an end-to-end test.
// The schedule itself is real and correct; a test that waited it out would be
// asserting on the clock rather than on the gateway.
//
// An empty body is allowed and means "everything ready", so `curl -XPOST` with
// no data does the obvious thing.
func (s *Server) flush(w http.ResponseWriter, r *http.Request) {
	var req flushRequest
	raw, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(raw) > 0 {
		if err := decodeBytes(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	n, err := s.Control.Flush(req.UUIDs)
	if err != nil {
		// Partial success is real: some uuids may have flushed before one
		// failed, so the count goes out with the error rather than being lost.
		writeJSON(w, http.StatusConflict, map[string]any{
			"flushed": n,
			"error":   err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flushed": n})
}
