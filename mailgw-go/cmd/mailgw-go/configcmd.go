package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/node"
)

const configUsage = `usage: mailgw-go config show [-data <dir>]

Print the configuration bundle this gateway last received from Central
Management, with relay passwords, the logservice key and the admin metrics
token redacted. It is the
managed-mode equivalent of reading routing.yaml, and the first thing to look at
when a route misbehaves.

The header goes to stderr and the bundle to stdout, so ` + "`config show | jq`" + ` works.

Note that -data is opened read-write: the directory, the database and any
pending schema migration are created if missing. Run it as the same user the
gateway runs as.
`

// runConfig implements the `config` subcommand group.
func runConfig(args []string) int {
	sub := "show"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	if sub != "show" {
		fmt.Fprintf(os.Stderr, "unknown config subcommand %q\n\n%s", sub, configUsage)
		return 2
	}

	o := mustParse("config show", args, nil)

	cached, release, err := node.CachedBundle(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config show: %v\n", err)
		return 1
	}
	defer release()

	fmt.Fprintf(os.Stderr, "# %s\n", node.BundleSource(cached))
	fmt.Fprintf(os.Stderr, "# digest:  %s\n", cached.SHA256)
	fmt.Fprintf(os.Stderr, "# fetched: %s\n", cached.FetchedAt.Format(time.RFC3339))
	if cached.AppliedAt != nil {
		fmt.Fprintf(os.Stderr, "# applied: %s\n", cached.AppliedAt.Format(time.RFC3339))
	} else {
		fmt.Fprintf(os.Stderr, "# applied: never\n")
	}
	if cached.ApplyError != "" {
		fmt.Fprintf(os.Stderr, "# error:   %s\n", cached.ApplyError)
	}

	out, err := config.RedactBundle(cached.Bundle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config show: %v\n", err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}
