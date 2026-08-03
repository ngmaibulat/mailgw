// Command mailgw-go-test is the ENGINEERING build of the gateway: mailgw-go
// plus an unauthenticated HTTP control API for configuring it from a test.
//
// # It must never be deployed
//
// The control API has no authentication and what it does is re-point a gateway.
// Anyone who can reach it can hand this node relay credentials of their
// choosing, or take the ones it already has. This binary exists so that the
// end-to-end suites can configure a gateway without a browser; it is not built
// by `pnpm docker:push`, and its image is published under a different repository
// name and never as :latest.
//
// The shipped binary is unaffected and cannot be made to do this: cmd/mailgw-go
// does not import internal/testctl, and CI asserts that with `go list -deps`.
//
// # What it does NOT change
//
// Everything below the control API is the shipped code. It brings the gateway up
// through the same internal/node this repository ships, applies configuration
// through the same applyCached that boot, the poll loop, the WebSocket and
// SIGHUP use, and validates a bundle with the same compiler. If that were not
// true the suite would be testing something no console can produce, which is the
// mistake M18 recorded when it rejected seeding MariaDB directly.
//
// Central Management still works exactly as it does in the shipped binary: this
// node can be enrolled, approved and deployed to in the ordinary way, and the
// control API is an addition rather than a replacement.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/node"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/testctl"
)

// Set via -ldflags at build time. The version deliberately carries a suffix so
// that a node reporting in to a console is identifiable as a test build in the
// fleet view, rather than looking like a shipped one.
var (
	version = "dev-testctl"
	commit  = "none"
)

const usage = `usage: mailgw-go-test -testctl <addr> [-data <dir>] [-admin <addr>]

The ENGINEERING build: mailgw-go plus an unauthenticated control API.

  -testctl <addr>   bind the control API here (REQUIRED, no default)
  -data <dir>       data directory: SQLite store and gateway identity
  -admin <addr>     admin UI bind address (required; use :0 for any free port)

The control API is unauthenticated. This binary must never be deployed; use
mailgw-go for anything real.

  POST /testctl/config            apply a configuration bundle, synchronously
  POST /testctl/config/profiles   apply server/routing/allowlist/relays texts
  GET  /testctl/config            the applied bundle, unredacted
  GET  /testctl/status            serving?, applied version, BOUND listen addrs
  POST /testctl/enroll            register with a Central Management console
  POST /testctl/reset             forget the cached configuration and console
  GET  /testctl/queue             the outbound spool
  POST /testctl/queue/flush       make envelopes due now and nudge the scheduler
  POST /testctl/queue/release     quarantine -> the ready queue
  POST /testctl/queue/hold        the ready queue -> quarantine
  POST /testctl/queue/remove      delete envelopes and collect their bodies

The bound control address is written to stdout as "testctl <addr>" once the
listener is up, so -testctl 127.0.0.1:0 is usable from a harness.
`

func main() {
	fs := flag.NewFlagSet("mailgw-go-test", flag.ExitOnError)
	o := node.Options{Version: version, Commit: commit}
	var ctlAddr string
	// No default, deliberately. A default would make this binary a drop-in for
	// the shipped one that silently opens an unauthenticated configuration API,
	// which is precisely the accident the separate binary exists to prevent.
	fs.StringVar(&ctlAddr, "testctl", "", "control API bind address (required)")
	fs.StringVar(&o.DataDir, "data", "/var/lib/mailgw-go", "data directory: SQLite store and gateway identity")
	fs.StringVar(&o.AdminAddr, "admin", "0.0.0.0:8080", "admin UI bind address (required; :0 takes any free port)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("mailgw-go-test %s (%s)\n", version, commit)
		return
	}
	if ctlAddr == "" {
		fmt.Fprint(os.Stderr, "mailgw-go-test: -testctl is required\n\n"+usage)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := node.New(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailgw-go-test: %v\n", err)
		// errors.Is rather than ==, matching node.Serve. Equivalent today
		// because New returns the sentinel unwrapped, and the two copies of this
		// exit-code decision should not be able to drift apart.
		if errors.Is(err, node.ErrNoAdminAddr) {
			os.Exit(2)
		}
		os.Exit(1)
	}

	n.Log().Warn("THIS IS THE TEST BUILD — the control API is unauthenticated and this binary must not be deployed",
		"testctl", ctlAddr)

	ctl := &testctl.Server{Control: n, Log: n.Log()}
	ctlErr := make(chan error, 1)
	go func() { ctlErr <- ctl.ListenAndServe(ctx, ctlAddr) }()

	// Run blocks and owns the teardown. A control-API bind failure has to
	// interrupt it, or a suite would sit against a gateway it can never
	// configure until its own timeout fired.
	done := make(chan int, 1)
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() { done <- n.Run(runCtx) }()

	select {
	case code := <-done:
		os.Exit(code)
	case err := <-ctlErr:
		if err != nil {
			n.Log().Error("test control API failed", "addr", ctlAddr, "err", err)
			cancelRun()
			<-done
			os.Exit(1)
		}
		os.Exit(<-done)
	}
}
