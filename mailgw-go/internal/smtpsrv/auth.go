package smtpsrv

import (
	"bytes"
	"errors"
	"runtime"
	"strings"

	sasl "github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
)

// Inbound SMTP AUTH (RFC 4954).
//
// go-smtp advertises AUTH only for a session that also implements
// smtp.AuthSession (backend.go:95-102), and only when authAllowed() is true —
// the session is encrypted, or the server sets AllowInsecureAuth. So there are
// three independent switches between a peer and a password prompt, and all three
// are off in a configuration that says nothing: no credentials, no TLS, no
// allow_insecure_auth.
var _ smtp.AuthSession = (*session)(nil)

// verifySem bounds how many bcrypt comparisons run at once.
//
// bcrypt at the console's cost 10 is tens of milliseconds of CPU, deliberately,
// and until the rate limiter lands anything the IP allowlist admits can ask for
// one by sending AUTH. Without a bound, a handful of peers can saturate every
// core and starve the delivery runner and the event senders — mail stops moving
// because somebody guessed passwords.
//
// GOMAXPROCS is the right size because the work is pure CPU: more concurrency
// buys nothing and only lengthens every queued attempt. Waiters cost a parked
// goroutine each and are already bounded by max.connections, so this cannot
// become the M11 trap where a cap meant to protect the gateway is the thing that
// takes it down. It is a floor under the damage, not a limiter — that is M15.
var verifySem = make(chan struct{}, max(runtime.GOMAXPROCS(0), 1))

// dummyHash is compared against when the username is unknown.
//
// Without it, a bad username returns in microseconds and a bad password in tens
// of milliseconds, which turns the AUTH command into a username oracle. The
// value is a bcrypt hash of a string nobody knows; only its cost matters.
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

// errBadCredentials is what every failure path returns.
//
// One error for "no such user" and "wrong password" alike, and 535 5.7.8 rather
// than go-smtp's default 454 4.7.0 for an authenticator error: a wrong password
// is permanent, and answering 4xx tells a client to try the same wrong password
// again. Conn.writeError (conn.go:1314-1320) passes an *SMTPError through
// verbatim, which is the same mechanism Session.Data's deliberate 250 relies on.
func errBadCredentials() error {
	return &smtp.SMTPError{
		Code:         535,
		EnhancedCode: smtp.EnhancedCode{5, 7, 8},
		Message:      "Authentication credentials invalid",
	}
}

// auth returns the credential set in force, or nil.
func (s *session) auth() *config.Auth {
	if s.b == nil || s.b.Auth == nil {
		return nil
	}
	return s.b.Auth()
}

// AuthMechanisms lists what this session will accept.
//
// Empty when no credential is configured: go-smtp then omits the capability
// entirely (conn.go:263-265), so a gateway with nothing to authenticate against
// looks exactly as it did before M13 rather than advertising a mechanism that
// can only ever fail.
//
// PLAIN and LOGIN, and deliberately nothing else. CRAM-MD5 and the other
// challenge-response mechanisms require the server to hold a recoverable
// password, which would undo the whole reason the bundle carries hashes.
func (s *session) AuthMechanisms() []string {
	if !s.auth().Enabled() {
		return nil
	}
	return []string{sasl.Plain, sasl.Login}
}

// Auth starts a SASL exchange.
func (s *session) Auth(mech string) (sasl.Server, error) {
	if !s.auth().Enabled() {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	switch strings.ToUpper(mech) {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			// RFC 4616 leaves the authorization identity blank to mean "the same
			// as the username". This gateway has no notion of acting as somebody
			// else, so anything else is refused rather than quietly ignored —
			// ignoring it would let a client believe it had assumed an identity
			// the audit trail then attributes to the login instead.
			if identity != "" && identity != username {
				return errBadCredentials()
			}
			return s.verify(username, password, sasl.Plain)
		}), nil
	case sasl.Login:
		return newLoginServer(func(username, password string) error {
			return s.verify(username, password, sasl.Login)
		}), nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

// verify checks one credential and records the outcome.
//
// It is the only place authUser is set, and the only place the two AUTH counters
// move — the same single-site discipline refuse() applies to msg_rejected.
func (s *session) verify(username, password, mech string) error {
	// First, and before anything reads the credential set: a peer that has
	// already burned through ratelimit.auth_failures_per_ip is answered without
	// a bcrypt comparison at all, which is the entire point of that limit.
	if err := s.allowAuthAttempt(); err != nil {
		return err
	}

	hash, known := s.auth().Lookup(username)
	if !known {
		// Compared anyway, against a hash nobody can match. See dummyHash.
		hash = string(dummyHash)
	}

	select {
	case verifySem <- struct{}{}:
		defer func() { <-verifySem }()
	case <-s.b.ctx().Done():
		// Shutdown started while this attempt was queued. Temporary by nature,
		// so 454 rather than 535: the credential was never judged.
		return &smtp.SMTPError{
			Code:         454,
			EnhancedCode: smtp.EnhancedCode{4, 7, 0},
			Message:      "Server shutting down, try again",
		}
	}

	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	// Folded in so the "unknown user" path costs a comparison too — Go would
	// otherwise be free to skip the branch entirely.
	if !known || err != nil {
		// Only a failure charges the rate budget, so a client that gets its
		// password right every time is never throttled by a limit named for
		// failures.
		s.recordAuthFailure()
		s.b.obs().AuthFailed.Add(1)
		s.log.Info("authentication failed", "user", username, "mechanism", mech)
		return errBadCredentials()
	}

	s.authUser = username
	s.authMech = mech
	// The env's helo scope is rebuilt from the session at MAIL, so this is
	// enough to make auth.user readable by a rule; updating it here as well
	// keeps a rule that somehow evaluates before the next MAIL consistent.
	if s.env != nil && s.env.Helo != nil {
		s.env.Helo.AuthUser = username
		s.env.Helo.AuthMechanism = mech
	}
	s.b.obs().AuthOK.Add(1)
	s.log.Info("authenticated", "user", username, "mechanism", mech)
	return nil
}

// loginServer implements the LOGIN mechanism's server half.
//
// go-sasl ships only the client (login.go), so this is the one piece of M13 that
// could not be delegated to it. LOGIN is obsolete and PLAIN is strictly better,
// but enough submission clients still reach for it first that omitting it means
// an unnecessary failed connection for a user who did nothing wrong.
//
// The exchange, per draft-murchison-sasl-login: the server prompts "Username:",
// the client answers, the server prompts "Password:", the client answers. A
// client may also send the username as the initial response, in which case the
// first challenge is skipped — which is what go-smtp's `AUTH LOGIN <base64>`
// path produces.
type loginServer struct {
	authenticate func(username, password string) error
	username     string
	haveUser     bool
	done         bool
}

func newLoginServer(authenticate func(username, password string) error) sasl.Server {
	return &loginServer{authenticate: authenticate}
}

var (
	loginUserChallenge = []byte("Username:")
	loginPassChallenge = []byte("Password:")
)

func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	if l.done {
		return nil, false, sasl.ErrUnexpectedClientResponse
	}

	// No initial response: prompt for the username. Note this is reached with a
	// nil response, not an empty one — an empty username is a client that
	// answered the prompt with nothing, and that is a failed login rather than a
	// reason to prompt again.
	if response == nil && !l.haveUser {
		return loginUserChallenge, false, nil
	}

	if !l.haveUser {
		// Some clients echo the challenge back verbatim when they meant to send
		// an empty initial response. Treating that as a username would produce a
		// baffling audit line, so it is refused as the malformed exchange it is.
		if bytes.EqualFold(response, loginUserChallenge) {
			return nil, false, errors.New("sasl: malformed LOGIN exchange")
		}
		l.username = string(response)
		l.haveUser = true
		return loginPassChallenge, false, nil
	}

	l.done = true
	return nil, true, l.authenticate(l.username, string(response))
}
