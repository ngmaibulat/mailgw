package api

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ngmaibulat/mailgw/logservice-go/apitest"
)

// fiberTarget runs a contract case against the Fiber route table.
//
// Through App(), never a bare handler, so the route table and the per-route
// middleware are themselves under test — which matters more here than next door,
// because several of the cases exist to catch a Fiber default leaking through
// the router rather than through a handler.
type fiberTarget struct{ app *fiber.App }

func (a fiberTarget) Do(t *testing.T, r *http.Request) *apitest.Response {
	t.Helper()
	// The default app.Test timeout is one second, which the 4 MiB oversized-body
	// cases exceed under -race. FailOnTimeout so a timeout is a failure rather
	// than a silently truncated response that some assertion might accept.
	res, err := a.app.Test(r, fiber.TestConfig{
		Timeout:       30 * time.Second,
		FailOnTimeout: true,
	})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return apitest.ReadResponse(t, res)
}

// TestContract is the shared suite — the same cases logservice-go runs against
// net/http. The two passing is what "byte-identical" means in this repo, and a
// case that passes there and fails here is the whole reason this module exists.
func TestContract(t *testing.T) {
	apitest.Run(t, apitest.ImplFiber, func(t *testing.T, f apitest.Fixture) apitest.Target {
		s := &Server{
			DB:      f.DB,
			APIKey:  f.APIKey,
			Version: f.Version,
			// Discarded: several cases drive error paths on purpose, and their
			// log lines are noise rather than signal.
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if f.Ready {
			s.MarkReady()
		}
		return fiberTarget{s.App()}
	})
}

// The contract package owns these strings; this asserts this implementation's
// own copies still match. Without it the two could drift and the suite would
// keep passing, because the suite compares against its own constant.
func TestContractConstants_MatchThisImplementation(t *testing.T) {
	if notFoundBody != apitest.NotFoundBody {
		t.Errorf("notFoundBody = %q, contract says %q", notFoundBody, apitest.NotFoundBody)
	}
	if reasonMigrating != apitest.ReasonMigrating {
		t.Errorf("reasonMigrating = %q, contract says %q", reasonMigrating, apitest.ReasonMigrating)
	}
	if reasonNoDB != apitest.ReasonNoDB {
		t.Errorf("reasonNoDB = %q, contract says %q", reasonNoDB, apitest.ReasonNoDB)
	}
}
