package api

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
)

// The response bodies. Constants because they are a contract:
// tests/api/logservice.e2e.test.ts asserts several of them byte-for-byte, and
// mailgw-go decides whether to keep or discard an audit event from the status
// beside them.
var (
	bodyOK           = map[string]string{"status": "OK"}
	bodyFail         = map[string]string{"status": "Fail"}
	bodyUnauthorized = map[string]string{"status": "Unauthorized"}
	bodyError        = map[string]string{"status": "Error", "message": "Internal server error"}
)

// notFoundBody is plain text, not JSON, and keeps its trailing newline. The Bun
// service's catch-all answered exactly this and the e2e suite checks the status;
// there is no reason to differ and one reason not to.
const notFoundBody = "Resource does not exist\n"

// auth gates a route on X-API-Key.
//
// When APIKey is empty EVERY REQUEST IS ACCEPTED. That is the Bun service's
// behaviour, it is what the dev compose stack depends on (API_KEY defaults to
// empty), and it is warned about once at startup rather than per request.
// Making this fail-closed would silently break every deployment that never set
// a key — including, on the first upgrade, any that had not noticed it was open.
//
// Auth is the OUTER wrapper, as it was in Bun's handle() =
// withAuth(withErrorHandling(...)): an unauthenticated caller gets a 401 and
// nothing else runs.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.APIKey != "" {
			got := r.Header.Get("X-API-Key")
			// Constant-time, unlike the Bun service's `!==`. The key travels on
			// every request from every gateway, so it is exactly the kind of
			// secret a timing oracle gets many samples of. Compare lengths via
			// the same primitive to avoid an early-exit on length.
			if subtle.ConstantTimeEq(int32(len(got)), int32(len(s.APIKey))) != 1 ||
				subtle.ConstantTimeCompare([]byte(got), []byte(s.APIKey)) != 1 {
				writeJSON(w, http.StatusUnauthorized, bodyUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// recoverPanics turns a panic into a 500 rather than a dropped connection.
//
// Wrapped around the whole mux rather than per route: a panic is not a policy
// decision, and a dropped connection would look to mailgw-go's event client like
// a transport error — retried, then spilled — while a 500 is at least an honest
// "this failed, try again".
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log().Error("panic serving request",
					"method", r.Method, "path", r.URL.Path, "panic", v)
				writeJSON(w, http.StatusInternalServerError, bodyError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// fail500 logs the real reason and tells the caller nothing about it.
//
// The 500 matters as much as the silence: this is the status mailgw-go retries.
// Answering 4xx for a database that is merely down would make the gateway
// discard the event permanently.
func (s *Server) fail500(w http.ResponseWriter, r *http.Request, err error) {
	s.log().Error("request failed",
		"method", r.Method, "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, bodyError)
}

// writeJSON encodes v with the JSON content type.
//
// The body is encoded into a buffer first by json.Marshal so a mid-encode
// failure cannot leave a 200 carrying half an object — the pattern
// mailgw-go/internal/adminui uses for the same reason.
func writeJSON(w http.ResponseWriter, code int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		// Only reachable if a handler builds an unencodable value. Nothing here
		// does, and if one ever did the caller deserves a 500 rather than a
		// truncated 200.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"Error","message":"Internal server error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

// readBody reads a bounded request body.
//
// The Bun service read bodies unbounded. A cap is not a policy change anybody
// notices: the largest legitimate body here is a /filter/md5 array with one
// small object per MIME part, and mailgw-go bounds its own MIME walk long
// before this.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
}
