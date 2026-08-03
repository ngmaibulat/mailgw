package store

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gw")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// The DSN is assembled with net/url, and a mistake there is silent: the
// database still opens, just without the pragmas. Assert they actually took.
func TestStore_PragmasApplied(t *testing.T) {
	s, _ := openTemp(t)

	var journal string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var busy int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

// A data directory whose name would break naive string concatenation.
func TestStore_OpensDirectoryWithAwkwardName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gw data?x=1")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	defer s.Close()

	var journal string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal — the DSN lost its pragmas", journal)
	}
}

func TestStore_Permissions(t *testing.T) {
	s, dir := openTemp(t)

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode = %04o, want 0700", got)
	}

	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	// The database holds an Ed25519 private key.
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("database mode = %04o, want 0600", got)
	}
}

func TestStore_MigrateIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gw")

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var v1 int
	if err := s1.db.QueryRow(`PRAGMA user_version`).Scan(&v1); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	var v2 int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&v2); err != nil {
		t.Fatalf("read user_version: %v", err)
	}

	if v1 != v2 {
		t.Errorf("user_version changed on reopen: %d -> %d", v1, v2)
	}
	if v1 != len(migrations)-1 {
		t.Errorf("user_version = %d, want %d", v1, len(migrations)-1)
	}
}

func TestStore_IdentityStableAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gw")

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := s1.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	second, err := s2.Identity()
	if err != nil {
		t.Fatalf("Identity after reopen: %v", err)
	}

	if first.Fingerprint != second.Fingerprint {
		t.Errorf("fingerprint changed across reopen: %s -> %s", first.Fingerprint, second.Fingerprint)
	}
	if !first.PrivateKey.Equal(second.PrivateKey) {
		t.Error("private key changed across reopen")
	}
	if !first.PublicKey.Equal(second.PublicKey) {
		t.Error("public key changed across reopen")
	}
}

// Two identities would be unrecoverable: the console would hold a public key
// whose private half the gateway had already discarded.
func TestStore_IdentityIdempotentUnderConcurrency(t *testing.T) {
	s, _ := openTemp(t)

	const n = 16
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Identity()
			if err != nil {
				t.Errorf("Identity: %v", err)
				return
			}
			mu.Lock()
			seen[id.Fingerprint]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != 1 {
		t.Fatalf("generated %d distinct identities, want 1: %v", len(seen), seen)
	}
}

// Must match webui-fastify/src/agent/verify.ts#fingerprintFromBase64, which
// hashes the raw key bytes rather than their base64 text.
func TestStore_FingerprintIsSHA256HexOfPublicKey(t *testing.T) {
	s, _ := openTemp(t)

	id, err := s.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if len(id.PublicKey) != ed25519.PublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(id.PublicKey), ed25519.PublicKeySize)
	}

	sum := sha256.Sum256(id.PublicKey)
	want := hex.EncodeToString(sum[:])
	if id.Fingerprint != want {
		t.Errorf("fingerprint = %s, want %s", id.Fingerprint, want)
	}
}

func TestStore_SetGatewayUIDPersists(t *testing.T) {
	s, _ := openTemp(t)

	if _, err := s.Identity(); err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if err := s.SetGatewayUID("6f1c0f6e-0000-4000-8000-000000000001"); err != nil {
		t.Fatalf("SetGatewayUID: %v", err)
	}

	id, err := s.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.GatewayUID != "6f1c0f6e-0000-4000-8000-000000000001" {
		t.Errorf("gateway uid = %q", id.GatewayUID)
	}
}

func TestStore_SetGatewayUIDBeforeIdentityIsAnError(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SetGatewayUID("x"); err == nil {
		t.Error("expected an error when no identity has been generated")
	}
}

func TestStore_SettingsRoundTrip(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SetCentralURL("https://console.example:4000"); err != nil {
		t.Fatalf("SetCentralURL: %v", err)
	}
	got, err := s.CentralURL()
	if err != nil {
		t.Fatalf("CentralURL: %v", err)
	}
	if got != "https://console.example:4000" {
		t.Errorf("central url = %q", got)
	}

	// Overwriting must replace, not accumulate.
	if err := s.SetCentralURL("https://other:4000"); err != nil {
		t.Fatalf("SetCentralURL: %v", err)
	}
	if got, _ := s.CentralURL(); got != "https://other:4000" {
		t.Errorf("central url after update = %q", got)
	}
}

// "" is the unprovisioned signal and must not arrive as an error.
func TestStore_AbsentSettingIsEmptyNotError(t *testing.T) {
	s, _ := openTemp(t)

	got, err := s.Setting("never-set")
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}

	url, err := s.CentralURL()
	if err != nil {
		t.Fatalf("CentralURL: %v", err)
	}
	if url != "" {
		t.Errorf("central url = %q, want empty on a fresh store", url)
	}
}

func TestStore_BoolSettingRoundTrip(t *testing.T) {
	s, _ := openTemp(t)

	if v, err := s.BoolSetting(KeyCentralInsecureTLS); err != nil || v {
		t.Fatalf("unset bool = %v, %v; want false, nil", v, err)
	}
	if err := s.SetBoolSetting(KeyCentralInsecureTLS, true); err != nil {
		t.Fatalf("SetBoolSetting: %v", err)
	}
	if v, err := s.BoolSetting(KeyCentralInsecureTLS); err != nil || !v {
		t.Errorf("bool = %v, %v; want true, nil", v, err)
	}
}

// A corrupt flag must not read as its permissive value.
func TestStore_MalformedBoolSettingIsAnError(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SetSetting(KeyCentralInsecureTLS, "yes-please"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := s.BoolSetting(KeyCentralInsecureTLS); err == nil {
		t.Error("expected an error for a non-boolean value")
	}
}

func TestStore_ConfigCacheSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gw")

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = s1.SaveConfig(&CachedConfig{
		VersionID: 42, Version: 7, SHA256: "abc",
		Bundle: []byte(`{"format":1}`), FetchedAt: time.Unix(1000, 0),
	})
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.LatestConfig()
	if err != nil {
		t.Fatalf("LatestConfig: %v", err)
	}
	if got == nil {
		t.Fatal("LatestConfig = nil after reopen")
	}
	if got.VersionID != 42 || got.Version != 7 || string(got.Bundle) != `{"format":1}` {
		t.Errorf("round-tripped config = %+v", got)
	}
}

func TestStore_LatestConfigIsNilWhenEmpty(t *testing.T) {
	s, _ := openTemp(t)

	got, err := s.LatestConfig()
	if err != nil {
		t.Fatalf("LatestConfig: %v", err)
	}
	if got != nil {
		t.Errorf("LatestConfig = %+v, want nil on an empty cache", got)
	}
}

// After a rollback the console repoints at an OLDER version_id, so ordering by
// version_id would return the superseded bundle.
func TestStore_LatestConfigPrefersMostRecentlyFetched(t *testing.T) {
	s, _ := openTemp(t)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("SaveConfig: %v", err)
		}
	}
	must(s.SaveConfig(&CachedConfig{VersionID: 1, Version: 1, SHA256: "v1",
		Bundle: []byte("one"), FetchedAt: time.Unix(1000, 0)}))
	must(s.SaveConfig(&CachedConfig{VersionID: 2, Version: 2, SHA256: "v2",
		Bundle: []byte("two"), FetchedAt: time.Unix(2000, 0)}))
	// Rolled back to v1: re-fetched, so it is current again despite the lower id.
	must(s.SaveConfig(&CachedConfig{VersionID: 1, Version: 1, SHA256: "v1",
		Bundle: []byte("one"), FetchedAt: time.Unix(3000, 0)}))

	got, err := s.LatestConfig()
	if err != nil {
		t.Fatalf("LatestConfig: %v", err)
	}
	if got.VersionID != 1 {
		t.Errorf("LatestConfig = version_id %d, want 1 (the rolled-back-to version)", got.VersionID)
	}
}

func TestStore_MarkAppliedAndAppliedConfig(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SaveConfig(&CachedConfig{VersionID: 5, Version: 3, SHA256: "s",
		Bundle: []byte("b"), FetchedAt: time.Unix(1000, 0)}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if got, _ := s.AppliedConfig(); got != nil {
		t.Fatalf("AppliedConfig = %+v before anything was applied", got)
	}

	at := time.Unix(1234, 0)
	if err := s.MarkApplied(5, at); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	got, err := s.AppliedConfig()
	if err != nil {
		t.Fatalf("AppliedConfig: %v", err)
	}
	if got == nil || got.VersionID != 5 {
		t.Fatalf("AppliedConfig = %+v, want version_id 5", got)
	}
	if got.AppliedAt == nil || !got.AppliedAt.Equal(at) {
		t.Errorf("applied_at = %v, want %v", got.AppliedAt, at)
	}
}

func TestStore_MarkApplyErrorIsReadableBack(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SaveConfig(&CachedConfig{VersionID: 9, Version: 4, SHA256: "s",
		Bundle: []byte("b"), FetchedAt: time.Unix(1000, 0)}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := s.MarkApplyError(9, `rule "x": unknown field`); err != nil {
		t.Fatalf("MarkApplyError: %v", err)
	}

	got, err := s.LatestConfig()
	if err != nil {
		t.Fatalf("LatestConfig: %v", err)
	}
	if got.ApplyError != `rule "x": unknown field` {
		t.Errorf("apply_error = %q", got.ApplyError)
	}
}

// The applied bundle is what boot falls back to, so it must survive pruning no
// matter how many newer versions have been fetched since.
func TestStore_PruneKeepsAppliedAndRecent(t *testing.T) {
	s, _ := openTemp(t)

	if err := s.SaveConfig(&CachedConfig{VersionID: 1, Version: 1, SHA256: "s1",
		Bundle: []byte("b"), FetchedAt: time.Unix(1000, 0)}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := s.MarkApplied(1, time.Unix(1000, 0)); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}

	for i := 2; i < 2+keepConfigs+5; i++ {
		err := s.SaveConfig(&CachedConfig{
			VersionID: int64(i), Version: i, SHA256: "s",
			Bundle: []byte("b"), FetchedAt: time.Unix(int64(1000+i), 0),
		})
		if err != nil {
			t.Fatalf("SaveConfig %d: %v", i, err)
		}
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM config_cache`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n > keepConfigs+1 {
		t.Errorf("cache holds %d rows, want at most %d", n, keepConfigs+1)
	}

	applied, err := s.AppliedConfig()
	if err != nil {
		t.Fatalf("AppliedConfig: %v", err)
	}
	if applied == nil || applied.VersionID != 1 {
		t.Errorf("the applied version was pruned: %+v", applied)
	}
}

// TestOpen_AcceptsARelativeDataDirectory.
//
// The DSN is a file: URL, and url.URL turns a relative path into a URI
// AUTHORITY — so `-data ./x` used to fail with "invalid uri authority: x",
// which names neither the flag nor the directory. The shipped node always gets
// an absolute /var/lib/mailgw-go, so this only ever bit somebody running the
// binary by hand or from a test harness, who has the least context for it.
func TestOpen_AcceptsARelativeDataDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	st, err := Open("./data")
	if err != nil {
		t.Fatalf("Open with a relative data dir: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// It must be a working store, not merely one that opened.
	if _, err := st.Identity(); err != nil {
		t.Errorf("Identity on a store opened relatively: %v", err)
	}
}
