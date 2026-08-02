package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// keepConfigs is how many cached bundles to retain beyond the applied one.
// Bundles are whole configurations and the poll loop writes one on every
// change; without a cap this table grows for the life of the gateway.
const keepConfigs = 10

// CachedConfig is one configuration version as pulled from Central Management.
//
// The cache exists so that a console outage is a non-event: the gateway boots
// from the last configuration it successfully applied. More than one row is
// kept so "keep the last-good configuration" is a query rather than a re-fetch.
type CachedConfig struct {
	VersionID  int64
	Version    int
	SHA256     string
	Bundle     []byte
	FetchedAt  time.Time
	AppliedAt  *time.Time
	ApplyError string
}

// SaveConfig stores a pulled bundle, replacing any previous copy of the same
// version. Re-saving refreshes FetchedAt, which is what makes LatestConfig
// meaningful after a rollback.
func (s *Store) SaveConfig(c *CachedConfig) error {
	ctx := context.Background()
	var appliedAt any
	if c.AppliedAt != nil {
		appliedAt = c.AppliedAt.Unix()
	}
	fetched := c.FetchedAt
	if fetched.IsZero() {
		fetched = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO config_cache (version_id, version, sha256, bundle, fetched_at, applied_at, apply_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(version_id) DO UPDATE SET
			version     = excluded.version,
			sha256      = excluded.sha256,
			bundle      = excluded.bundle,
			fetched_at  = excluded.fetched_at,
			apply_error = excluded.apply_error`,
		c.VersionID, c.Version, c.SHA256, c.Bundle, fetched.Unix(), appliedAt, c.ApplyError)
	if err != nil {
		return fmt.Errorf("cache config version %d: %w", c.VersionID, err)
	}
	return s.pruneConfigs(ctx)
}

// pruneConfigs keeps the most recently fetched bundles plus anything that has
// ever been applied — the applied row is the one boot falls back to, so it must
// never be evicted no matter how old it is.
func (s *Store) pruneConfigs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM config_cache
		  WHERE applied_at IS NULL
		    AND version_id NOT IN (
			    SELECT version_id FROM config_cache
			     ORDER BY fetched_at DESC, version_id DESC
			     LIMIT ?
		    )`, keepConfigs)
	if err != nil {
		return fmt.Errorf("prune config cache: %w", err)
	}
	return nil
}

// LatestConfig is the most recently fetched bundle, or nil if none is cached.
//
// Ordered by fetched_at rather than version_id: a rollback repoints the gateway
// at an *older* version, so the highest version_id is then a superseded bundle
// that must not be treated as current.
func (s *Store) LatestConfig() (*CachedConfig, error) {
	return s.oneConfig(`SELECT version_id, version, sha256, bundle, fetched_at, applied_at, apply_error
	                      FROM config_cache
	                     ORDER BY fetched_at DESC, version_id DESC
	                     LIMIT 1`)
}

// AppliedConfig is the most recently applied bundle, or nil if none has ever
// been applied. This is what a managed gateway falls back to on boot.
//
// Ordered by applied_seq, not applied_at: the timestamp has one-second
// resolution, and a rollback applied in the same second as the deploy it undoes
// would otherwise tie-break on version_id — returning the very bundle the
// operator just rejected.
func (s *Store) AppliedConfig() (*CachedConfig, error) {
	return s.oneConfig(`SELECT version_id, version, sha256, bundle, fetched_at, applied_at, apply_error
	                      FROM config_cache
	                     WHERE applied_at IS NOT NULL
	                     ORDER BY applied_seq DESC, applied_at DESC, version_id DESC
	                     LIMIT 1`)
}

// ConfigByVersionID is one specific cached bundle, or nil if it is not cached.
//
// This is what makes rollback cheap: the console repoints at an older version
// whose bytes are still here, so the gateway re-applies them without asking for
// them again — and what it then runs is byte-identical to what ran before.
func (s *Store) ConfigByVersionID(versionID int64) (*CachedConfig, error) {
	return s.oneConfig(`SELECT version_id, version, sha256, bundle, fetched_at, applied_at, apply_error
	                      FROM config_cache
	                     WHERE version_id = ?`, versionID)
}

func (s *Store) oneConfig(query string, args ...any) (*CachedConfig, error) {
	var (
		c         CachedConfig
		fetched   int64
		applied   sql.NullInt64
		bundleRaw []byte
	)
	err := s.db.QueryRowContext(context.Background(), query, args...).
		Scan(&c.VersionID, &c.Version, &c.SHA256, &bundleRaw, &fetched, &applied, &c.ApplyError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cached config: %w", err)
	}
	c.Bundle = bundleRaw
	c.FetchedAt = time.Unix(fetched, 0)
	if applied.Valid {
		t := time.Unix(applied.Int64, 0)
		c.AppliedAt = &t
	}
	return &c, nil
}

// MarkApplied records that a version is now the running configuration and
// clears any previous error for it.
//
// applied_seq is bumped past every other row, which is what makes "most
// recently applied" answerable when two applies land in the same second — a
// rollback and the deploy it undoes usually do.
func (s *Store) MarkApplied(versionID int64, at time.Time) error {
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE config_cache
		    SET applied_at  = ?,
		        applied_seq = (SELECT COALESCE(MAX(applied_seq), 0) + 1 FROM config_cache),
		        apply_error = ''
		  WHERE version_id = ?`,
		at.Unix(), versionID)
	if err != nil {
		return fmt.Errorf("mark version %d applied: %w", versionID, err)
	}
	return nil
}

// MarkApplyError records why a version could not be applied. The gateway keeps
// running its previous configuration; this is how the reason reaches the
// console instead of being silent.
func (s *Store) MarkApplyError(versionID int64, msg string) error {
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE config_cache SET apply_error = ? WHERE version_id = ?`, msg, versionID)
	if err != nil {
		return fmt.Errorf("mark version %d failed: %w", versionID, err)
	}
	return nil
}
