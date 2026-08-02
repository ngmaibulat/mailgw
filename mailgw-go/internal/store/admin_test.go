package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The code is the node's admin secret for its whole service life, so it must be
// the same string on every call — not a fresh one per boot, which would make
// "the code in yesterday's log" wrong.
func TestAdmin_ClaimCodeIsGeneratedOnceAndIsStable(t *testing.T) {
	s, _ := openTemp(t)

	first, err := s.EnsureClaimCode()
	if err != nil {
		t.Fatalf("EnsureClaimCode: %v", err)
	}
	if first.Code == "" {
		t.Fatal("no claim code was generated")
	}
	if first.Claimed {
		t.Error("a fresh node reports itself claimed")
	}

	second, err := s.EnsureClaimCode()
	if err != nil {
		t.Fatalf("EnsureClaimCode: %v", err)
	}
	if second.Code != first.Code {
		t.Errorf("claim code changed on the second call: %q -> %q", first.Code, second.Code)
	}
}

func TestAdmin_ClaimCodeSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gw")

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := s1.EnsureClaimCode()
	if err != nil {
		t.Fatalf("EnsureClaimCode: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	again, err := s2.EnsureClaimCode()
	if err != nil {
		t.Fatalf("EnsureClaimCode: %v", err)
	}
	if again.Code != first.Code {
		t.Errorf("claim code changed across a restart: %q -> %q", first.Code, again.Code)
	}
}

// The code is NOT consumed. A single-use code plus a cookie leaves the node
// reachable by exactly one browser for ever.
func TestAdmin_ClaimCodeWorksMoreThanOnce(t *testing.T) {
	s, _ := openTemp(t)
	st, err := s.EnsureClaimCode()
	if err != nil {
		t.Fatalf("EnsureClaimCode: %v", err)
	}

	for i := range 3 {
		ok, err := s.CheckClaimCode(st.Code)
		if err != nil {
			t.Fatalf("CheckClaimCode: %v", err)
		}
		if !ok {
			t.Fatalf("presentation %d was refused", i+1)
		}
	}

	after, err := s.ClaimState()
	if err != nil {
		t.Fatalf("ClaimState: %v", err)
	}
	if !after.Claimed {
		t.Error("the node is not marked claimed after a successful presentation")
	}
	if after.Code != st.Code {
		t.Error("the code was rotated by being used")
	}
}

// The claimed timestamp records the FIRST use, so a later presentation must not
// move it.
func TestAdmin_ClaimedAtRecordsTheFirstUse(t *testing.T) {
	s, _ := openTemp(t)
	st, _ := s.EnsureClaimCode()

	if _, err := s.CheckClaimCode(st.Code); err != nil {
		t.Fatalf("CheckClaimCode: %v", err)
	}
	first, err := s.ClaimState()
	if err != nil {
		t.Fatalf("ClaimState: %v", err)
	}

	if err := s.SetSetting(KeyAdminClaimedAt, "1000000000"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := s.CheckClaimCode(st.Code); err != nil {
		t.Fatalf("CheckClaimCode: %v", err)
	}
	second, err := s.ClaimState()
	if err != nil {
		t.Fatalf("ClaimState: %v", err)
	}
	if second.ClaimedAt.Unix() != 1000000000 {
		t.Errorf("claimed_at was overwritten by a later presentation: %v (first use was %v)",
			second.ClaimedAt, first.ClaimedAt)
	}
}

func TestAdmin_WrongCodeIsRefusedAndDoesNotRotate(t *testing.T) {
	s, _ := openTemp(t)
	st, _ := s.EnsureClaimCode()

	for _, bad := range []string{"", "nonsense", strings.Repeat("A", claimCodeLen)} {
		ok, err := s.CheckClaimCode(bad)
		if err != nil {
			t.Fatalf("CheckClaimCode(%q): %v", bad, err)
		}
		if ok {
			t.Errorf("CheckClaimCode(%q) accepted", bad)
		}
	}

	after, _ := s.ClaimState()
	if after.Code != st.Code {
		t.Error("a failed attempt changed the code")
	}
	if after.Claimed {
		t.Error("a failed attempt marked the node claimed")
	}
}

// A file-mode store never generates a code, and an empty presentation against an
// empty setting must not read as a match.
func TestAdmin_NoCodeMeansNoAccess(t *testing.T) {
	s, _ := openTemp(t)

	ok, err := s.CheckClaimCode("")
	if err != nil {
		t.Fatalf("CheckClaimCode: %v", err)
	}
	if ok {
		t.Error("an empty code was accepted against a store with no code")
	}
}

func TestAdmin_ResetMintsANewCodeAndRevokesEverySession(t *testing.T) {
	s, _ := openTemp(t)
	before, _ := s.EnsureClaimCode()
	if _, err := s.CheckClaimCode(before.Code); err != nil {
		t.Fatalf("CheckClaimCode: %v", err)
	}

	sess, err := s.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	next, err := s.ResetClaimCode()
	if err != nil {
		t.Fatalf("ResetClaimCode: %v", err)
	}
	if next == before.Code {
		t.Error("reset returned the same code")
	}

	live, err := s.Session(sess.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if live != nil {
		t.Error("a session survived a claim reset")
	}

	after, _ := s.ClaimState()
	if after.Claimed {
		t.Error("the node is still marked claimed after a reset")
	}
	if ok, _ := s.CheckClaimCode(before.Code); ok {
		t.Error("the old code still works after a reset")
	}
	if ok, _ := s.CheckClaimCode(next); !ok {
		t.Error("the new code does not work")
	}
}

func TestAdmin_SessionRoundTripAndLogout(t *testing.T) {
	s, _ := openTemp(t)

	sess, err := s.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sess.ID == "" || sess.CSRF == "" || sess.ID == sess.CSRF {
		t.Fatalf("session id/csrf look wrong: %+v", sess)
	}

	got, err := s.Session(sess.ID)
	if err != nil || got == nil {
		t.Fatalf("Session: %v, %v", got, err)
	}
	if got.CSRF != sess.CSRF {
		t.Errorf("csrf = %q, want %q", got.CSRF, sess.CSRF)
	}

	if err := s.DeleteSession(sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if got, _ := s.Session(sess.ID); got != nil {
		t.Error("the session resolved after being deleted")
	}
	// Logging out twice is the outcome the caller wanted either way.
	if err := s.DeleteSession(sess.ID); err != nil {
		t.Errorf("second DeleteSession: %v", err)
	}
}

// Sessions are in the database precisely so a restart mid-provisioning does not
// dump the operator back to the claim page.
func TestAdmin_SessionSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gw")

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sess, err := s1.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	got, err := s2.Session(sess.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got == nil {
		t.Fatal("the session did not survive a restart")
	}
	if got.CSRF != sess.CSRF {
		t.Error("the csrf token did not survive with its session")
	}
}

func TestAdmin_ExpiredSessionDoesNotResolve(t *testing.T) {
	s, _ := openTemp(t)

	sess, err := s.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE admin_sessions SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Second).Unix(), sess.ID); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	got, err := s.Session(sess.ID)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got != nil {
		t.Error("an expired session resolved")
	}

	// Minting a session is where expired rows are swept — the read path stays
	// read-only.
	if _, err := s.NewSession(); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_sessions WHERE id = ?`, sess.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Error("the expired session was not swept when a new one was created")
	}
}

// An empty id is "no cookie", not a lookup.
func TestAdmin_EmptySessionIDResolvesToNothing(t *testing.T) {
	s, _ := openTemp(t)

	got, err := s.Session("")
	if err != nil || got != nil {
		t.Errorf("Session(\"\") = %v, %v; want nil, nil", got, err)
	}
}

// The alphabet exists so a code can be dictated. If the normaliser does not
// undo the mistakes it invites, the alphabet is pointless.
func TestNormalizeClaimCode(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"canonical", "ABCDE-FGHJK-MNPQR-STVWX", "ABCDE-FGHJK-MNPQR-STVWX"},
		{"no dashes", "ABCDEFGHJKMNPQRSTVWX", "ABCDE-FGHJK-MNPQR-STVWX"},
		{"lower case", "abcde-fghjk-mnpqr-stvwx", "ABCDE-FGHJK-MNPQR-STVWX"},
		{"spaces instead of dashes", "ABCDE FGHJK MNPQR STVWX", "ABCDE-FGHJK-MNPQR-STVWX"},
		{"surrounding space", "  ABCDEFGHJKMNPQRSTVWX\n", "ABCDE-FGHJK-MNPQR-STVWX"},
		// Crockford: the characters a person reading aloud gets wrong.
		{"I and L fold onto 1", "IBCDE-LGHJK-MNPQR-STVWX", "1BCDE-1GHJK-MNPQR-STVWX"},
		{"O folds onto 0", "OBCDE-FGHJK-MNPQR-STVWX", "0BCDE-FGHJK-MNPQR-STVWX"},
		{"empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeClaimCode(tc.in); got != tc.want {
				t.Errorf("NormalizeClaimCode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A generated code must round-trip through the normaliser unchanged, or a
// correct paste would be refused.
func TestAdmin_GeneratedCodeIsCanonical(t *testing.T) {
	for range 20 {
		code, err := newClaimCode()
		if err != nil {
			t.Fatalf("newClaimCode: %v", err)
		}
		if got := NormalizeClaimCode(code); got != code {
			t.Fatalf("generated %q normalises to %q", code, got)
		}
		if strings.ContainsAny(code, "ILOU") {
			t.Fatalf("generated code %q uses an ambiguous character", code)
		}
		if want := claimCodeLen + claimCodeLen/5 - 1; len(code) != want {
			t.Fatalf("len(%q) = %d, want %d", code, len(code), want)
		}
	}
}
