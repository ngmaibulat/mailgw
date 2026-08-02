package main

import (
	"fmt"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// loadBundle produces the same *loaded that a file-mode load produces, from a
// configuration bundle's bytes.
//
// It is the twin of load(dir), and the two must stay interchangeable — that is
// the actual claim this milestone makes, and bundle_test.go asserts it against
// testdata/config.
//
// The assembly lives here rather than in internal/config because it needs
// ruleset.Compile, and internal/config must not import internal/ruleset:
// internal/smtpsrv imports config on the session hot path, and pulling the rule
// compiler into everything that reads a configuration is a layering regression.
func loadBundle(raw []byte, opts config.BundleOptions, source string) (*loaded, error) {
	b, err := config.ParseBundle(raw)
	if err != nil {
		return nil, err
	}
	cfg, err := b.Config(opts)
	if err != nil {
		return nil, err
	}
	l := &loaded{cfg: cfg, source: source}

	text, ok := b.RoutingText()
	if !ok {
		// No ruleset profile assigned. Legal: default_action answers 451 "No
		// route found" for every recipient, exactly as an empty Haraka table
		// did. Warned about at apply time rather than refused here.
		l.file = &ruleset.File{Version: 1}
		l.source = source + " (no ruleset profile)"
	} else if l.file, err = ruleset.ParseFile([]byte(text), config.FileRouting); err != nil {
		return nil, err
	}

	// Identical to the file path: the compiled artifact never knows where its
	// rules came from.
	if l.rules, err = ruleset.Compile(l.file, cfg.Relays, ruleset.DefaultSchema()); err != nil {
		return nil, err
	}
	return l, nil
}

// bundleSource names a cached version for log lines and `check` output, where a
// file mode gateway would print a path.
func bundleSource(c *store.CachedConfig) string {
	if c == nil {
		return "bundle (none)"
	}
	return fmt.Sprintf("bundle v%d (version_id %d)", c.Version, c.VersionID)
}
