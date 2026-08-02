package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Setting keys. These are the whole of the gateway's local configuration:
// everything else comes from Central Management.
const (
	// KeyCentralURL is the Central Management base URL, without a trailing
	// slash. Empty means unprovisioned, which is what makes the wizard appear.
	KeyCentralURL = "central_url"
	// KeyCentralInsecureTLS disables certificate verification against the
	// console. The shipped console serves a self-signed pair, so this is the
	// realistic default for a dev stack — and it is rendered as a warning on
	// the status page precisely because it should not be one in production.
	KeyCentralInsecureTLS = "central_insecure_tls"
	// KeyCentralCAFile is a PEM bundle to verify the console against, for the
	// private-CA case where disabling verification would be the wrong answer.
	KeyCentralCAFile = "central_ca_file"
	// KeyDesiredVersionID is the configuration version the console last said
	// this gateway should be running.
	//
	// Persisted rather than inferred because neither "most recently fetched"
	// nor "most recently applied" is the same question: a rollback repoints at
	// an older, already-cached version, and a deploy needing a restart is
	// applied but not yet fully in effect. Boot needs the console's intent, and
	// this is the only place it survives a restart.
	KeyDesiredVersionID = "desired_version_id"
)

// DesiredVersionID is the version the console last asked for, or 0 if it has
// never said.
func (s *Store) DesiredVersionID() (int64, error) {
	raw, err := s.Setting(KeyDesiredVersionID)
	if err != nil || raw == "" {
		return 0, err
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("setting %q is not a version id: %q", KeyDesiredVersionID, raw)
	}
	return v, nil
}

func (s *Store) SetDesiredVersionID(id int64) error {
	return s.SetSetting(KeyDesiredVersionID, strconv.FormatInt(id, 10))
}

// Setting returns a stored value, or "" if it has never been set.
//
// A missing key is deliberately not an error: CentralURL() == "" is the
// unprovisioned signal, and forcing every caller through an error path to
// discover the normal first-boot state would be noise.
func (s *Store) Setting(key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read setting %q: %w", key, err)
	}
	return value, nil
}

// SetSetting stores a value, replacing any previous one.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write setting %q: %w", key, err)
	}
	return nil
}

// BoolSetting reads a setting written by SetBoolSetting.
func (s *Store) BoolSetting(key string) (bool, error) {
	v, err := s.Setting(key)
	if err != nil || v == "" {
		return false, err
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		// A corrupt flag must not silently read as its permissive value.
		return false, fmt.Errorf("setting %q is not a boolean: %q", key, v)
	}
	return b, nil
}

// SetBoolSetting stores a boolean setting.
func (s *Store) SetBoolSetting(key string, v bool) error {
	return s.SetSetting(key, strconv.FormatBool(v))
}

// CentralURL is the Central Management base URL, or "" when unprovisioned.
func (s *Store) CentralURL() (string, error) { return s.Setting(KeyCentralURL) }

// SetCentralURL stores the Central Management base URL.
func (s *Store) SetCentralURL(u string) error { return s.SetSetting(KeyCentralURL, u) }
