package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"

	"github.com/gofiber/fiber/v3"
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

// notFoundBody is plain text, not JSON, and keeps its trailing newline. It must
// equal apitest.NotFoundBody, which contract_test.go asserts.
const notFoundBody = "Resource does not exist\n"

// headerAPIKey is the only request header this service reads.
const headerAPIKey = "X-API-Key"

// auth gates a route on X-API-Key.
//
// When APIKey is empty EVERY REQUEST IS ACCEPTED. That is the Bun service's
// behaviour, it is what the dev compose stack depends on (API_KEY defaults to
// empty), and it is warned about once at startup rather than per request.
//
// Fiber ships middleware/keyauth and it is deliberately not used: it is
// fail-closed, so an empty configured key is a misconfiguration to it rather
// than the documented "accept everything", and it has its own 401 body. Neither
// is adjustable into what this needs.
//
// The comparison is crypto/subtle, ported verbatim: the key travels on every
// request from every gateway, so it is exactly the kind of secret a timing
// oracle gets many samples of. Length is compared with the same primitive to
// avoid an early exit on it.
//
// c.Get may return a string aliasing fasthttp's read buffer, which is only
// valid for the length of the handler. Safe here — the value is compared
// synchronously and never retained. The same care is why the handlers read
// bytes and json.Unmarshal them, which allocates fresh strings, rather than
// holding a slice into the buffer.
func (s *Server) auth(next fiber.Handler) fiber.Handler {
	return func(c fiber.Ctx) error {
		if s.APIKey != "" {
			got := c.Get(headerAPIKey)
			if subtle.ConstantTimeEq(int32(len(got)), int32(len(s.APIKey))) != 1 ||
				subtle.ConstantTimeCompare([]byte(got), []byte(s.APIKey)) != 1 {
				return writeJSON(c, fiber.StatusUnauthorized, bodyUnauthorized)
			}
		}
		return next(c)
	}
}

// recoverPanics turns a panic into a 500 rather than a dropped connection.
//
// Registered around the whole app rather than per route: a panic is not a policy
// decision, and a dropped connection would look to mailgw-go's event client like
// a transport error — retried, then spilled — while a 500 is at least an honest
// "this failed, try again".
//
// Hand-written rather than Fiber's middleware/recover for the same reason auth
// is: that one routes its result through the default error handler and does not
// produce this body.
func (s *Server) recoverPanics(c fiber.Ctx) (err error) {
	defer func() {
		if v := recover(); v != nil {
			s.log().Error("panic serving request",
				"method", c.Method(), "path", c.Path(), "panic", v)
			// Discard anything a half-run handler wrote. net/http cannot do
			// this — those bytes are already on the wire — but fasthttp buffers
			// the response, so a panic here cannot produce a 500 with another
			// handler's body glued to it.
			c.Response().ResetBody()
			err = writeJSON(c, fiber.StatusInternalServerError, bodyError)
		}
	}()
	return c.Next()
}

// fail500 logs the real reason and tells the caller nothing about it.
//
// The 500 matters as much as the silence: this is the status mailgw-go retries.
// Answering 4xx for a database that is merely down would make the gateway
// discard the event permanently.
func (s *Server) fail500(c fiber.Ctx, err error) error {
	s.log().Error("request failed",
		"method", c.Method(), "path", c.Path(), "err", err)
	return writeJSON(c, fiber.StatusInternalServerError, bodyError)
}

// writeJSON encodes v with the JSON content type.
//
// A line-for-line port of logservice-go's, and deliberately NOT c.JSON.
//
// Three reasons, any one of them sufficient. c.JSON sets its own content type;
// it appends no trailing newline, which the body bytes carry; and it routes
// through fiber.Config.JSONEncoder, so a future drop-in like goccy/go-json
// would silently change the HTML escaping of <, > and & — characters that
// appear in `response` strings and subject lines, i.e. in every search result
// this service returns. encoding/json, always. CI asserts no other encoder is
// linked.
//
// The body is marshalled into a buffer first so a mid-encode failure cannot
// leave a 200 carrying half an object.
func writeJSON(c fiber.Ctx, code int, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		// Only reachable if a handler builds an unencodable value. Nothing here
		// does, and if one ever did the caller deserves a 500 rather than a
		// truncated 200.
		c.Set(fiber.HeaderContentType, contentTypeJSON)
		return c.Status(fiber.StatusInternalServerError).
			SendString(`{"status":"Error","message":"Internal server error"}` + "\n")
	}
	c.Set(fiber.HeaderContentType, contentTypeJSON)
	return c.Status(code).Send(append(buf, '\n'))
}

// contentTypeJSON is bare, with no "; charset=utf-8". net/http's
// w.Header().Set produces exactly this, several of Fiber's helpers do not, and
// the contract asserts equality rather than a prefix.
const contentTypeJSON = "application/json"

// errBodyTooLarge is what readBody returns past the cap. Each handler decides
// what it means, which is the whole reason the cap is not fasthttp's.
var errBodyTooLarge = errors.New("request body exceeds the cap")

// readBody reads a bounded request body.
//
// Deliberately not fasthttp's own MaxRequestBodySize: that answers 413 from the
// protocol layer, before routing, and the contract depends on the ROUTE — 400 on
// the three ingest endpoints, 500 on /filter/md5, because a 4xx there defers
// real mail. This mirrors what http.MaxBytesReader does next door: it errors,
// and the handler decides.
//
// Content-Length is checked first because every real caller sends one, so the
// common refusal costs nothing and never buffers. A chunked or unknown-length
// body falls through to the streaming read.
func readBody(c fiber.Ctx) ([]byte, error) {
	if n := c.Request().Header.ContentLength(); n > maxBodyBytes {
		refuseWithoutReading(c)
		return nil, errBodyTooLarge
	}

	// With StreamRequestBody set, fasthttp exposes the body as a reader for
	// anything it did not already buffer. A nil stream means it is in memory
	// and small enough that fasthttp saw no reason to stream it.
	r := c.Request().BodyStream()
	if r == nil {
		b := c.Body()
		if len(b) > maxBodyBytes {
			return nil, errBodyTooLarge
		}
		return b, nil
	}

	// maxBodyBytes+1, so a body exactly at the cap is accepted and one byte
	// over is refused — the same boundary http.MaxBytesReader draws.
	b, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBodyBytes {
		refuseWithoutReading(c)
		return nil, errBodyTooLarge
	}
	return b, nil
}

// refuseWithoutReading marks the connection to close after this response.
//
// It is required, not tidiness. StreamRequestBody means the body is still on
// the socket when a handler decides not to read it, and refusing an oversized
// body is exactly that decision — the whole point is not to buffer it. fasthttp
// would then go back to parse the NEXT request on the keep-alive connection and
// find the unread body where a request line should be, answering "small read
// buffer" and corrupting every subsequent request on that connection.
//
// net/http has the same hazard and resolves it the same way: it closes a
// connection whose request body was not drained rather than trying to reuse it.
//
// The alternative — draining the body so the connection stays reusable — is
// precisely what the cap exists to avoid: an attacker would set a 2 GiB
// Content-Length and be read in full by the code refusing it.
func refuseWithoutReading(c fiber.Ctx) {
	c.RequestCtx().SetConnectionClose()
}
