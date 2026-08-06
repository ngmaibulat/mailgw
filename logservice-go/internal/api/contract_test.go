package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ngmaibulat/mailgw/logservice-go/apitest"
)

// httpTarget runs a contract case against the net/http route table.
//
// Through Handler(), never a bare handler func, so the route table and the
// middleware wrapping are themselves under test — a target that called
// s.handleRoot directly would pass with the auth wrapper deleted.
type httpTarget struct{ h http.Handler }

func (a httpTarget) Do(t *testing.T, r *http.Request) *apitest.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	return apitest.ReadResponse(t, rec.Result())
}

// TestContract is the shared suite. Its counterpart in logservice-fiber runs
// the same cases against Fiber v3, and the two passing is what "byte-identical"
// means in this repo.
func TestContract(t *testing.T) {
	apitest.Run(t, apitest.ImplNetHTTP, func(t *testing.T, f apitest.Fixture) apitest.Target {
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
		return httpTarget{s.Handler()}
	})
}

// The contract package owns these strings; this asserts the implementation's
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
