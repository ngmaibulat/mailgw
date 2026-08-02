package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
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

	// A gateway running from files has no bundle. Compose the equivalent view
	// from its directory so the two modes are diffable rather than answering
	// "wrong mode".
	if o.configSet {
		return showConfigDir(o.configDir)
	}

	st, err := store.Open(o.dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config show: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	cached := bootConfig(st, newLogger(config.LogConfig{}))
	if cached == nil {
		fmt.Fprintf(os.Stderr, "config show: %s: no configuration has been cached yet\n", o.dataDir)
		return 1
	}

	fmt.Fprintf(os.Stderr, "# %s\n", bundleSource(cached))
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

// showConfigDir renders a configuration directory in the same shape the console
// would deploy, so `config show` answers the same question in both modes.
func showConfigDir(dir string) int {
	b := config.Bundle{Format: config.BundleFormat}

	if raw, err := os.ReadFile(filepath.Join(dir, config.FileServer)); err == nil {
		s := string(raw)
		b.Server = &s
	}
	if raw, err := os.ReadFile(filepath.Join(dir, config.FileRouting)); err == nil {
		s := string(raw)
		b.Routing = &s
	}
	if raw, err := os.ReadFile(filepath.Join(dir, config.FileFilter)); err == nil {
		b.Allowlist = raw
	}
	if raw, err := os.ReadFile(filepath.Join(dir, config.FileRelays)); err == nil {
		if err := json.Unmarshal(raw, &b.Relays); err != nil {
			fmt.Fprintf(os.Stderr, "config show: %s: %v\n", config.FileRelays, err)
			return 1
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, config.FileLogging)); err == nil {
		if err := json.Unmarshal(raw, &b.Logging); err != nil {
			fmt.Fprintf(os.Stderr, "config show: %s: %v\n", config.FileLogging, err)
			return 1
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, config.FileAdmin)); err == nil {
		var admin config.Admin
		if err := json.Unmarshal(raw, &admin); err != nil {
			fmt.Fprintf(os.Stderr, "config show: %s: %v\n", config.FileAdmin, err)
			return 1
		}
		b.Admin = &admin
	}
	if raw, err := os.ReadFile(filepath.Join(dir, config.FileAuth)); err == nil {
		var auth config.Auth
		if err := json.Unmarshal(raw, &auth); err != nil {
			fmt.Fprintf(os.Stderr, "config show: %s: %v\n", config.FileAuth, err)
			return 1
		}
		b.Auth = &auth
	}

	raw, err := json.Marshal(b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config show: %v\n", err)
		return 1
	}
	out, err := config.RedactBundle(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config show: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "# %s (file mode; no deployed version)\n", dir)
	fmt.Println(string(out))
	return 0
}
