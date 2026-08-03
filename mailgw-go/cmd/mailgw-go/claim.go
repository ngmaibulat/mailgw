package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

const claimUsage = `usage: mailgw-go claim [status|reset] [-data <dir>]

The admin UI is locked by a claim code, generated the first time a managed
gateway starts and written to its log. Presenting it in the wizard signs an
operator in.

  status   show the code and whether anyone has used it yet
  reset    mint a new code and sign every session out

This is the recovery path for a lost code. It needs filesystem access to the
data directory, which already holds this gateway's private key — so it is not a
weaker gate than the one it reopens.

  docker exec mailgw-go /usr/local/bin/mailgw-go claim status

Note that ` + "`status`" + ` PRINTS A LIVE CREDENTIAL: it is the answer to "I lost the
code", which should not require rotating it and signing everybody else out.
Treat its output like a password, not like a log line to paste into a ticket.

Note also that -data is opened read-write: the directory, the database and any
pending schema migration are created if missing. Run it as the same user the
gateway runs as.
`

// runClaim implements the `claim` subcommand group.
func runClaim(args []string) int {
	sub := "status"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	if sub != "status" && sub != "reset" {
		fmt.Fprintf(os.Stderr, "unknown claim subcommand %q\n\n%s", sub, claimUsage)
		return 2
	}

	o := mustParse("claim "+sub, args, nil)

	st, err := store.Open(o.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claim: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	if sub == "reset" {
		code, err := st.ResetClaimCode()
		if err != nil {
			fmt.Fprintf(os.Stderr, "claim reset: %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "# every admin session has been signed out")
		fmt.Println(code)
		return 0
	}

	state, err := st.ClaimState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claim status: %v\n", err)
		return 1
	}
	if state.Code == "" {
		fmt.Fprintf(os.Stderr,
			"claim status: %s: no claim code yet — this gateway has not started in\n"+
				"  managed mode. It generates one on its first boot.\n", o.DataDir)
		return 1
	}

	if state.Claimed {
		fmt.Fprintf(os.Stderr, "# claimed:  %s\n", state.ClaimedAt.Format(time.RFC3339))
	} else {
		fmt.Fprintln(os.Stderr, "# claimed:  never — nobody has signed in to this gateway yet")
	}
	fmt.Fprintf(os.Stderr, "# sessions: %d signed in now\n", state.Sessions)
	fmt.Println(state.Code)
	return 0
}
