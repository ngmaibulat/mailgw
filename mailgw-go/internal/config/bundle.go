package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/relays"
)

// BundleFormat is the only bundle shape this build understands.
//
// The console bumps it if the document's shape ever changes. A gateway that
// guesses at an unknown format is worse than one that refuses and says so:
// the refusal is visible in the console as an apply error, and the previous
// configuration keeps running.
const BundleFormat = 1

// Bundle is one immutable configuration version, as Central Management deploys
// it. It mirrors webui-fastify/src/central/bundle.ts.
//
// Its keys are the configuration directory's files one for one, which is the
// whole point of the design: `check`, `explain` and every existing test keep
// working, and a bundle is diffable against a directory.
//
//	server    -> server.yaml   (text, verbatim)
//	routing   -> routing.yaml  (text, verbatim — the rule DSL)
//	allowlist -> ngmfilter.json
//	relays    -> relays.json
//	logging   -> logging.json
//	admin     -> admin.json
//	auth      -> auth.json
//
// The rule DSL is compiled and type-checked here, not on the console: the
// gateway is authoritative for validation, keeps its last-good configuration on
// failure, and reports the compiler's message back through POST /agent/report.
type Bundle struct {
	Format int `json:"format"`

	// Server and Routing are the raw file texts, or nil when no profile of
	// that kind is assigned. Never objects.
	Server  *string `json:"server"`
	Routing *string `json:"routing"`

	// Allowlist stays raw so it can be fed straight to ParseAllowlist, reusing
	// the exact fail-closed validator the file path uses — including the
	// "empty list requires allow_all" rule.
	Allowlist json.RawMessage `json:"allowlist"`

	Relays  map[string][]relays.Relay `json:"relays"`
	Logging Logging                   `json:"logging"`

	// Admin is omitted by the console when it has nothing to say, which is what
	// keeps an unchanged configuration hashing identically. A pointer so that
	// "absent" and "present but empty" are the same thing here as they are
	// there.
	Admin *Admin `json:"admin,omitempty"`

	// Auth carries the inbound submission credentials, as bcrypt hashes. A
	// pointer and omitempty on the same reasoning as Admin: a gateway with no
	// credential set assigned must produce the byte-identical bundle it produced
	// before this key existed, or every gateway in the fleet re-pulls and
	// restarts for a configuration that did not change.
	Auth *Auth `json:"auth,omitempty"`
}

// BundleOptions carries what a bundle deliberately cannot: properties of this
// process rather than of the configuration.
type BundleOptions struct {
	// SpoolDir replaces the compiled-in default when the bundle's server
	// profile does not choose one. A managed gateway is given a data directory,
	// not /opt/mailgw-go, so the default is usually unwritable for it — and the
	// first apply would fail on queue.NewSpool before SMTP ever started.
	SpoolDir string
}

// ParseBundle decodes one bundle and checks its envelope. Turning it into a
// usable configuration is Config.
func ParseBundle(raw []byte) (*Bundle, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("bundle: empty")
	}

	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	if b.Format != BundleFormat {
		return nil, fmt.Errorf(
			"bundle: unsupported format %d (this build understands %d); upgrade the gateway",
			b.Format, BundleFormat)
	}
	return &b, nil
}

// Config turns a bundle into a validated configuration, through exactly the
// validators the file path uses.
//
// The returned Config has no Dir: it did not come from a directory, and
// inventing a plausible-looking path would make a log line lie.
func (b *Bundle) Config(opts BundleOptions) (*Config, error) {
	cfg := &Config{Server: defaults(), Logging: b.Logging}
	if b.Admin != nil {
		cfg.Admin = *b.Admin
	}
	if b.Auth != nil {
		cfg.Auth = *b.Auth
	}

	// A nil server profile is a working configuration for everything except the
	// relay table — the same thing an absent server.yaml means to Load.
	if b.Server != nil && strings.TrimSpace(*b.Server) != "" {
		srv, err := ParseServer([]byte(*b.Server), FileServer, true)
		if err != nil {
			return nil, fmt.Errorf("server profile: %w", err)
		}
		cfg.Server = srv
	}
	// Only when the operator did not choose one: an explicit outbound.spool_dir
	// always wins over the process default.
	if opts.SpoolDir != "" && cfg.Server.Outbound.SpoolDir == DefaultSpoolDir {
		cfg.Server.Outbound.SpoolDir = opts.SpoolDir
	}

	// A gateway with no relay group cannot deliver anything, so this fails the
	// whole bundle rather than applying the rules and quietly dropping mail.
	tbl, err := relays.NewTable(b.Relays)
	if err != nil {
		return nil, fmt.Errorf("relay groups: %w (assign a relay group to this gateway)", err)
	}
	cfg.Relays = tbl

	// The allowlist is the only gate protecting the relay, so the bundle path
	// uses the same fail-closed validator the file path does.
	list, err := ParseAllowlist(b.Allowlist, "allowlist profile")
	if err != nil {
		return nil, err
	}
	cfg.Allowlist = list

	if err := cfg.Server.validate(); err != nil {
		return nil, err
	}
	if err := cfg.validateRelayRefs(); err != nil {
		return nil, err
	}
	if err := cfg.validateAuth(); err != nil {
		return nil, err
	}
	if err := cfg.validateDKIM(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// RoutingText is the rule-file source and whether the bundle carried one.
//
// A bundle with no ruleset profile is a gateway with no routes. That is legal,
// not an error: default_action already answers 451 "No route found" for every
// recipient, which is the same thing Haraka's hook_get_mx did.
func (b *Bundle) RoutingText() (string, bool) {
	if b.Routing == nil || strings.TrimSpace(*b.Routing) == "" {
		return "", false
	}
	return *b.Routing, true
}

// RedactBundle re-renders a bundle with every relay password replaced, for
// `mailgw-go config show`.
//
// It must never be used to recompute a digest: keys come back in Go's sorted
// map order, not the console's stable stringify, so the bytes differ even when
// the configuration does not.
func RedactBundle(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}

	if groups, ok := doc["relays"]; ok {
		var byGroup map[string][]map[string]json.RawMessage
		// A relays key that is not the expected shape is not worth failing an
		// operator's debugging session over; leave it as it was.
		if err := json.Unmarshal(groups, &byGroup); err == nil {
			for _, members := range byGroup {
				for _, m := range members {
					if _, has := m["auth_pass"]; has {
						m["auth_pass"] = json.RawMessage(`"[redacted]"`)
					}
				}
			}
			if out, err := json.Marshal(byGroup); err == nil {
				doc["relays"] = out
			}
		}
	}
	// A bcrypt hash is not a password, but it is an offline-crackable artefact
	// and this output is pasted into issue trackers. Redacting it also keeps
	// `config show` from being the easiest way to harvest the fleet's
	// submission hashes from any host that can run the binary.
	if raw, ok := doc["auth"]; ok {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err == nil {
			var users []map[string]json.RawMessage
			if err := json.Unmarshal(obj["users"], &users); err == nil {
				for _, u := range users {
					if _, has := u["hash"]; has {
						u["hash"] = json.RawMessage(`"[redacted]"`)
					}
				}
				if out, err := json.Marshal(users); err == nil {
					obj["users"] = out
					if out, err := json.Marshal(obj); err == nil {
						doc["auth"] = out
					}
				}
			}
		}
	}

	// The logservice credential and the metrics bearer token are secrets on the
	// same footing: `config show` is a command the docs tell operators to run
	// and paste.
	for key, field := range map[string]string{"logging": "api_key", "admin": "metrics_token"} {
		raw, ok := doc[key]
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		if _, has := obj[field]; !has {
			continue
		}
		obj[field] = json.RawMessage(`"[redacted]"`)
		if out, err := json.Marshal(obj); err == nil {
			doc[key] = out
		}
	}

	return json.MarshalIndent(doc, "", "  ")
}
