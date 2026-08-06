package apitest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// The fake database exists so /readyz's three states are all contract-asserted
// without a MariaDB anywhere near the test. `go test ./...` stays offline in
// both modules, which is what CI runs.
//
// It is a driver.Connector passed to sql.OpenDB rather than a registered
// driver: sql.Register writes to a package-level map, so two tests registering
// the same name would panic and a test that ran alone would pass. A connector
// is passed in by value and owns nothing global.
//
// Only Ping is implemented. Nothing here should ever run a query — a case that
// needs rows belongs in the e2e suite, against a real schema.

var errFakeDBQuery = errors.New("apitest: the fake database answers Ping and nothing else; " +
	"a contract case that needs rows belongs in the e2e suite")

// OKDB returns a pool whose Ping succeeds and whose every statement fails.
//
// Both halves are useful. The successful Ping is what /readyz needs to reach
// its 200; the failing statement is how a handler is driven onto its error path
// without a nil pool — which would fail by nil-pointer panic and make the case
// depend on each implementation's panic recoverer rather than on the status it
// answers.
func OKDB() *sql.DB { return sql.OpenDB(fakeConnector{}) }

// FailDB returns a pool whose Ping fails, which is how /readyz's
// "database unreachable" reason is reached without unplugging anything.
func FailDB() *sql.DB {
	return sql.OpenDB(fakeConnector{pingErr: errors.New("apitest: database is down")})
}

type fakeConnector struct{ pingErr error }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeConn{pingErr: c.pingErr}, nil
}

func (c fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

// Open is never called: sql.OpenDB uses the connector directly. It exists
// because driver.Connector requires a Driver().
func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errFakeDBQuery }

type fakeConn struct{ pingErr error }

// Ping is the only real method.
//
// The error is returned as-is rather than wrapped in driver.ErrBadConn, which
// database/sql treats as "retry on a fresh connection" — it would retry twice
// and then surface a confusing error instead of this one.
func (c fakeConn) Ping(context.Context) error { return c.pingErr }

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errFakeDBQuery }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errFakeDBQuery }
