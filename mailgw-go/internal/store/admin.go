package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Admin UI credentials (M12).
//
// Before this, the admin UI was unauthenticated and reachability was the access
// control. That was defensible while the UI was opt-in; M5 made the wizard the
// only provisioning path, so it is always listening, on a root process, on an
// internet-facing relay.
const (
	// KeyAdminClaimCode is the shared secret that lets an operator take control
	// of the admin UI. Presenting it mints a session.
	//
	// It is stored in the clear, deliberately. The obvious alternative — store
	// a hash — protects against a threat that does not exist here: the same
	// 0600 database in the same 0700 directory holds this gateway's Ed25519
	// seed, and whoever can read that re-registers as this gateway and is handed
	// the relay credentials by the console directly. What plaintext buys is
	// real: `mailgw-go claim status` can answer "I lost the code" WITHOUT
	// rotating it, so an operator recovering their own access does not log every
	// other operator out.
	KeyAdminClaimCode = "admin_claim_code"
	// KeyAdminClaimedAt records the first time the code was accepted.
	//
	// It does not consume the code. A code that worked exactly once, with a
	// cookie as the only other credential, would leave the node reachable by
	// exactly one browser for ever — a second operator, a cleared cookie or a
	// new laptop would each need `claim reset`. What must not reopen is the
	// UNAUTHENTICATED window, and that is closed the moment a code exists. This
	// flag exists so a live credential stops being echoed into the log on every
	// boot after the first use.
	KeyAdminClaimedAt = "admin_claimed_at"
)

// SessionTTL is how long an admin session lives. Absolute, not sliding: the
// question "how long can a stolen cookie be used?" should have one answer.
const SessionTTL = 12 * time.Hour

// claimAlphabet is Crockford base32: upper case, without I, L, O and U.
//
// This string gets read off a terminal, copied out of `docker logs` and
// sometimes dictated, so the characters that are indistinguishable in a
// proportional font are simply not in it.
const claimAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// claimCodeLen is 20 characters of the alphabet above — 100 bits.
//
// Generous on purpose. Unlike a one-time code this one is live for the node's
// whole service life, on a port that faces the internet if an operator skipped
// deploy/gateway/05-firewall.sh, and it costs nothing because it is copied
// rather than typed.
const claimCodeLen = 20

// AdminSession is one logged-in browser.
type AdminSession struct {
	ID        string
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ClaimState is what the admin UI and `mailgw-go claim status` need to know.
type ClaimState struct {
	Code      string
	Claimed   bool
	ClaimedAt time.Time
	Sessions  int
}

// EnsureClaimCode returns this gateway's claim code, generating one on the
// first call.
//
// Read-or-create, idempotent under concurrency for the same reasons Identity is
// (store.go's single connection, the IMMEDIATE transaction the DSN forces, and
// here an ON CONFLICT DO NOTHING that cannot produce a second code).
func (s *Store) EnsureClaimCode() (ClaimState, error) {
	ctx := context.Background()

	st, err := s.claimState(ctx)
	if err != nil {
		return ClaimState{}, err
	}
	if st.Code != "" {
		return st, nil
	}

	code, err := newClaimCode()
	if err != nil {
		return ClaimState{}, err
	}

	// DO NOTHING rather than DO UPDATE: whoever loses the race must return the
	// winner's code, not overwrite a secret an operator may already be holding.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO NOTHING`, KeyAdminClaimCode, code); err != nil {
		return ClaimState{}, fmt.Errorf("generate admin claim code: %w", err)
	}
	return s.claimState(ctx)
}

// ClaimState reports the current code and whether it has ever been used.
func (s *Store) ClaimState() (ClaimState, error) {
	return s.claimState(context.Background())
}

func (s *Store) claimState(ctx context.Context) (ClaimState, error) {
	var st ClaimState
	var err error

	if st.Code, err = s.Setting(KeyAdminClaimCode); err != nil {
		return ClaimState{}, err
	}
	raw, err := s.Setting(KeyAdminClaimedAt)
	if err != nil {
		return ClaimState{}, err
	}
	if raw != "" {
		sec, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil {
			return ClaimState{}, fmt.Errorf("setting %q is not a timestamp: %q", KeyAdminClaimedAt, raw)
		}
		st.Claimed, st.ClaimedAt = true, time.Unix(sec, 0)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_sessions WHERE expires_at > ?`,
		time.Now().Unix()).Scan(&st.Sessions); err != nil {
		return ClaimState{}, fmt.Errorf("count admin sessions: %w", err)
	}
	return st, nil
}

// CheckClaimCode compares a presented code against the stored one and, on the
// first success, records that this node has been claimed.
//
// The comparison is constant-time in the value but not in the length:
// subtle.ConstantTimeCompare returns 0 immediately for differing lengths. That
// leaks only how long a claim code is, which is a compiled-in constant.
func (s *Store) CheckClaimCode(presented string) (bool, error) {
	stored, err := s.Setting(KeyAdminClaimCode)
	if err != nil {
		return false, err
	}
	// No code means the node has not generated one, which happens only in file
	// mode. Refuse rather than accept an empty presentation.
	if stored == "" {
		return false, nil
	}
	if subtle.ConstantTimeCompare([]byte(NormalizeClaimCode(presented)), []byte(stored)) != 1 {
		return false, nil
	}

	claimed, err := s.Setting(KeyAdminClaimedAt)
	if err != nil {
		return false, err
	}
	if claimed == "" {
		if err := s.SetSetting(KeyAdminClaimedAt,
			strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ResetClaimCode mints a new code, forgets that the node was ever claimed and
// revokes every session, in one transaction.
//
// This is the recovery path: it needs filesystem access to the data directory,
// which is already game over for this gateway, so it is not a weaker gate than
// the one it reopens. Sessions go with it because "reset the code" is what an
// operator reaches for when they have lost control of the UI, not only when
// they have lost a piece of paper.
func (s *Store) ResetClaimCode() (string, error) {
	ctx := context.Background()

	code, err := newClaimCode()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil) // IMMEDIATE, per the DSN
	if err != nil {
		return "", fmt.Errorf("reset admin claim code: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO settings (key, value) VALUES (?, ?)
		  ON CONFLICT(key) DO UPDATE SET value = excluded.value`, []any{KeyAdminClaimCode, code}},
		{`DELETE FROM settings WHERE key = ?`, []any{KeyAdminClaimedAt}},
		{`DELETE FROM admin_sessions`, nil},
	} {
		if _, err := tx.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			return "", fmt.Errorf("reset admin claim code: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("reset admin claim code: commit: %w", err)
	}
	return code, nil
}

// NewSession mints a session and its CSRF token.
//
// Expired rows are swept here rather than on the read path: a session lookup
// runs on every page view, and making a GET write to the database that the poll
// loop is also using would be a poor trade for a table that can only grow when
// somebody presents the claim code.
func (s *Store) NewSession() (*AdminSession, error) {
	ctx := context.Background()
	now := time.Now()

	id, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("new admin session: %w", err)
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("new admin session: %w", err)
	}
	sess := &AdminSession{
		ID:        id,
		CSRF:      csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionTTL),
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM admin_sessions WHERE expires_at <= ?`, now.Unix()); err != nil {
		return nil, fmt.Errorf("new admin session: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_sessions (id, csrf, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		sess.ID, sess.CSRF, now.Unix(), sess.ExpiresAt.Unix()); err != nil {
		return nil, fmt.Errorf("new admin session: %w", err)
	}
	return sess, nil
}

// Session resolves a session id, or returns nil when there is no live session
// with that id.
//
// A missing or expired session is deliberately not an error: "not logged in" is
// the normal state of a request, not a failure.
func (s *Store) Session(id string) (*AdminSession, error) {
	if id == "" {
		return nil, nil
	}
	var (
		sess             AdminSession
		created, expires int64
	)
	err := s.db.QueryRowContext(context.Background(),
		`SELECT id, csrf, created_at, expires_at FROM admin_sessions
		 WHERE id = ? AND expires_at > ?`, id, time.Now().Unix()).
		Scan(&sess.ID, &sess.CSRF, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read admin session: %w", err)
	}
	sess.CreatedAt, sess.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	return &sess, nil
}

// DeleteSession ends one session. Logging out an id that is already gone is not
// an error — the outcome the caller wanted is the outcome they get.
func (s *Store) DeleteSession(id string) error {
	if id == "" {
		return nil
	}
	if _, err := s.db.ExecContext(context.Background(),
		`DELETE FROM admin_sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete admin session: %w", err)
	}
	return nil
}

// newClaimCode returns a fresh code in canonical form.
func newClaimCode() (string, error) {
	raw := make([]byte, claimCodeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate admin claim code: %w", err)
	}
	// One alphabet character per random byte. The alphabet is 32 long and 256
	// is a whole multiple of it, so the modulo is unbiased.
	out := make([]byte, 0, claimCodeLen+claimCodeLen/5-1)
	for i, b := range raw {
		if i > 0 && i%5 == 0 {
			out = append(out, '-')
		}
		out = append(out, claimAlphabet[int(b)%len(claimAlphabet)])
	}
	return string(out), nil
}

// NormalizeClaimCode puts a presented code into the canonical form the stored
// one is written in.
//
// Groups, spaces and case are presentation, so a code pasted back with or
// without its dashes works either way. I/L and O are folded onto 1 and 0 the
// way Crockford base32 specifies, which is the whole reason for using that
// alphabet: those are the characters an operator reading a code aloud gets
// wrong.
func NormalizeClaimCode(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	n := 0
	for _, r := range strings.ToUpper(strings.TrimSpace(in)) {
		switch {
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'O':
			r = '0'
		case (r < '0' || r > '9') && (r < 'A' || r > 'Z'):
			// Separators and anything else the operator's terminal added.
			continue
		}
		if n > 0 && n%5 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// randomToken is 256 bits of session identifier, hex so it is safe in a cookie
// and in a hidden form field without any escaping question.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
