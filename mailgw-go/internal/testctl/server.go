package testctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/adminui"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/node"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// maxBodyBytes bounds a request body.
//
// A configuration bundle is a few kilobytes; a megabyte is generous and is here
// because this listener is still a socket, and an unbounded read on a socket is
// an unbounded allocation whatever the binary is for.
const maxBodyBytes = 1 << 20

// Control is what this API drives. *node.Node satisfies it exactly, with no
// adapter — which is why the signatures below use node's own types rather than
// tidier local ones.
//
// An interface rather than the concrete type so the handler tests can run
// against a fake: routing, decoding and error mapping are then covered without
// bringing a gateway up, and the cases worth covering here are the malformed
// ones a real Node would never produce.
//
// Note the containment is one-directional and this interface is not part of it.
// This package importing node is harmless; what must never happen is node or
// cmd/mailgw-go importing THIS.
type Control interface {
	ApplyBundle(ctx context.Context, raw []byte) (node.Applied, error)
	AppliedBundle() (*store.CachedConfig, error)
	Enroll(ctx context.Context, s adminui.Settings) error
	Status() node.Status
	Queue() ([]node.QueueEntry, error)
	Flush(uuids []string) (int, error)
	Reset(ctx context.Context) error
}

// *node.Node is the only real implementation, asserted here so that a change to
// either side is a compile error in this package rather than a failure to build
// cmd/mailgw-go-test, where it would read as the binary being broken.
var _ Control = (*node.Node)(nil)

// CachedBundle is the applied configuration as this API reports it.
//
// Declared here rather than reusing store.CachedConfig so the wire shape is this
// package's own: the store type carries fields that mean nothing to a caller and
// would become an accidental contract the moment a test asserted on them.
type CachedBundle struct {
	VersionID int64           `json:"version_id"`
	Version   int             `json:"version"`
	SHA256    string          `json:"sha256"`
	Bundle    json.RawMessage `json:"bundle"`
}

// Server serves the control API.
type Server struct {
	Control Control
	Log     *slog.Logger
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler is the routing table.
//
// Read it as the API surface: there is no middleware deciding anything by path
// prefix, because there is nothing to decide — every route here is open. That is
// the opposite of internal/adminui's Handler, where the per-route wrapping IS
// the policy, and the difference is the point: this table would be a hole in
// that binary and is the whole feature in this one.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /testctl/config", s.applyBundle)
	mux.HandleFunc("POST /testctl/config/profiles", s.applyProfiles)
	mux.HandleFunc("GET /testctl/config", s.showConfig)
	mux.HandleFunc("GET /testctl/status", s.status)
	mux.HandleFunc("POST /testctl/enroll", s.enroll)
	mux.HandleFunc("POST /testctl/reset", s.reset)
	mux.HandleFunc("GET /testctl/queue", s.queue)
	mux.HandleFunc("POST /testctl/queue/flush", s.flush)
	return s.recoverPanics(mux)
}

// ListenAndServe runs the API until ctx is cancelled.
//
// The listener is opened synchronously so "port already in use" is a startup
// failure rather than a log line: a test harness that proceeded against a
// gateway whose control API silently never came up would fail somewhere far
// less informative.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log().Handler(), slog.LevelWarn),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	s.log().Warn("TEST CONTROL API listening — unauthenticated; this build must never be deployed",
		"addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// recoverPanics keeps a handler bug from taking the process down mid-suite,
// where it would look like the gateway crashed rather than the harness.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log().Error("test control API panic", "path", r.URL.Path, "err", v)
				writeErr(w, http.StatusInternalServerError, fmt.Sprintf("panic: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers with the reason as text in a JSON envelope.
//
// The message is the underlying error verbatim — the rule compiler's complaint
// about which relay group does not exist is the single most useful thing this
// API returns, and paraphrasing it would throw away the reason a test failed.
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readBody reads a bounded request body.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
}

// decode reads a bounded JSON request body into v.
func decode(r *http.Request, v any) error {
	raw, err := readBody(r)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("empty request body")
	}
	return decodeBytes(raw, v)
}

// decodeBytes is the half of decode a handler that tolerates an empty body
// needs after it has already read one.
func decodeBytes(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("request body is not valid JSON: %w", err)
	}
	return nil
}
