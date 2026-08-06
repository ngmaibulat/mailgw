package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// What is in this file is what is NOT contract.
//
// The wire behaviour — every status, header and body byte — lives in
// logservice-go/apitest and runs against this implementation and net/http's
// alike; see contract_test.go. A test belongs here when it is about HOW Fiber is
// being held to that contract rather than what the contract is: the second
// defence against the 405, the exact boundary readBody draws, the listener
// publishing its address. If you find yourself asserting a status code or a
// response body here, it belongs in apitest instead.

func testServer() *Server {
	return &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func send(t *testing.T, app *fiber.App, r *http.Request) *http.Response {
	t.Helper()
	res, err := app.Test(r, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return res
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = res.Body.Close()
	return string(b)
}

// A panic becomes a 500, not a dropped connection: mailgw-go reads a dropped
// connection as a transport error and eventually spills the event to disk.
//
// Implementation-local because it builds the middleware directly around a
// handler that panics. The contract asserts the 500 an operator sees; this
// asserts the mechanism that produces it, which is Fiber's and not net/http's.
func TestRecoverPanics_TurnsAPanicIntoA500(t *testing.T) {
	s := testServer()
	app := fiber.New(fiber.Config{ErrorHandler: s.errorHandler})
	app.Use(s.recoverPanics)
	app.Get("/boom", func(fiber.Ctx) error { panic("boom") })

	r, _ := http.NewRequest("GET", "http://logservice.test/boom", nil)
	res := send(t, app, r)

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if got := body(t, res); got != `{"message":"Internal server error","status":"Error"}`+"\n" {
		t.Errorf("body = %q, want the Error envelope", got)
	}
}

// A half-written response is discarded before the 500 is written.
//
// Only possible on Fiber: fasthttp buffers the response, so a handler that
// wrote and then panicked cannot leave its bytes glued to the front of the
// error envelope. net/http has already flushed by then, which is why this test
// has no counterpart next door.
func TestRecoverPanics_DiscardsAHalfWrittenResponse(t *testing.T) {
	s := testServer()
	app := fiber.New(fiber.Config{ErrorHandler: s.errorHandler})
	app.Use(s.recoverPanics)
	app.Get("/boom", func(c fiber.Ctx) error {
		_ = c.SendString("half a response")
		panic("boom")
	})

	r, _ := http.NewRequest("GET", "http://logservice.test/boom", nil)
	res := send(t, app, r)

	got := body(t, res)
	if strings.Contains(got, "half a response") {
		t.Errorf("body = %q, want the half-written bytes discarded", got)
	}
	if got != `{"message":"Internal server error","status":"Error"}`+"\n" {
		t.Errorf("body = %q, want the Error envelope", got)
	}
}

// errorHandler is the SECOND defence against Fiber's 405, and it has to work on
// its own.
//
// The primary defence is the catch-all app.Use registered last in App(), and the
// contract suite covers that path — including the absence of an Allow header,
// which only the catch-all gives. This asserts the fallback independently, so a
// Fiber upgrade that changed the router's matching order would still produce the
// right bytes rather than Fiber's own 405 page.
func TestErrorHandler_MapsFrameworkMissesToThePlainText404(t *testing.T) {
	s := testServer()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"404", fiber.ErrNotFound},
		{"405", fiber.ErrMethodNotAllowed},
		{"501", fiber.ErrNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New(fiber.Config{ErrorHandler: s.errorHandler})
			app.Get("/x", func(fiber.Ctx) error { return tc.err })

			r, _ := http.NewRequest("GET", "http://logservice.test/x", nil)
			res := send(t, app, r)

			if res.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", res.StatusCode)
			}
			if got := body(t, res); got != notFoundBody {
				t.Errorf("body = %q, want %q", got, notFoundBody)
			}
		})
	}
}

// Anything else a handler returns is this service's 500, not Fiber's.
func TestErrorHandler_TurnsAnUnexpectedErrorIntoTheErrorEnvelope(t *testing.T) {
	s := testServer()
	app := fiber.New(fiber.Config{ErrorHandler: s.errorHandler})
	app.Get("/x", func(fiber.Ctx) error { return errors.New("something no handler here does") })

	r, _ := http.NewRequest("GET", "http://logservice.test/x", nil)
	res := send(t, app, r)

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}
	if got := body(t, res); got != `{"message":"Internal server error","status":"Error"}`+"\n" {
		t.Errorf("body = %q, want the Error envelope", got)
	}
}

// readBody draws its boundary at exactly maxBodyBytes, the same place
// http.MaxBytesReader does: a body AT the cap is accepted, one byte over is not.
//
// Off-by-one here is invisible in production — nothing sends a body within one
// byte of a megabyte — and would be a real difference between the two
// implementations, which is precisely the kind of thing this module exists to
// find.
func TestReadBody_BoundaryIsExactlyMaxBodyBytes(t *testing.T) {
	s := testServer()
	app := s.App()

	// {"uuid":"…"} with the padding sized so the whole body is exactly the cap.
	envelope := len(`{"uuid":""}`)
	atCap := `{"uuid":"` + strings.Repeat("x", maxBodyBytes-envelope) + `"}`
	if len(atCap) != maxBodyBytes {
		t.Fatalf("test built a %d-byte body, want exactly %d", len(atCap), maxBodyBytes)
	}

	for _, tc := range []struct {
		name     string
		body     string
		wantOver bool
	}{
		{"exactly at the cap", atCap, false},
		{"one byte over", atCap + " ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("POST", "http://logservice.test/api/connection",
				strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.ContentLength = int64(len(tc.body))
			res := send(t, app, r)

			// At the cap the body is read and the request reaches the store,
			// which has no pool here and fails — a 500. Over the cap it is
			// refused before that, as a 400. Distinguishing those two is the
			// whole assertion; neither is the contract's business.
			if tc.wantOver && res.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 — one byte over the cap must be refused", res.StatusCode)
			}
			if !tc.wantOver && res.StatusCode == http.StatusBadRequest {
				t.Error("a body exactly at the cap was refused; the boundary is off by one")
			}
		})
	}
}

// ListenAndServe opens its listener synchronously and publishes the address, so
// a caller can ask for :0 and find the port — which is what the e2e harness
// needs and what app.Listen would not give.
func TestListenAndServe_PublishesTheBoundAddressAndStopsOnCancel(t *testing.T) {
	s := testServer()
	s.MarkReady()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx, "127.0.0.1:0") }()

	// Addr is set before Serve is reached, but the goroutine above still has to
	// get there.
	var addr string
	for i := 0; i < 200; i++ {
		if addr = s.Addr(); addr != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		cancel()
		t.Fatal("Addr() stayed empty; a test asking for :0 could not find the port")
	}

	res, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		cancel()
		t.Fatalf("GET /healthz on the bound address: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", res.StatusCode)
	}
	_ = res.Body.Close()

	cancel()
	select {
	case err := <-done:
		// ctx.Err() is returned deliberately, so main's context.Canceled check
		// still turns `docker compose down` into exit 0.
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ListenAndServe returned %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ListenAndServe did not return after its context was cancelled")
	}
}

// A port already in use is a startup error the operator sees, not a goroutine
// failing after main has reported success.
func TestListenAndServe_ReportsAPortConflictSynchronously(t *testing.T) {
	first := testServer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = first.ListenAndServe(ctx, "127.0.0.1:0") }()

	var addr string
	for i := 0; i < 200; i++ {
		if addr = first.Addr(); addr != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("the first server never bound")
	}

	second := testServer()
	err := second.ListenAndServe(context.Background(), addr)
	if err == nil {
		t.Fatal("binding an address already in use returned nil")
	}
	if second.Addr() != "" {
		t.Errorf("Addr() = %q after a failed bind, want empty", second.Addr())
	}
}
