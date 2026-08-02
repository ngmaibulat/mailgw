package smtpsrv

import (
	"strings"

	smtp "github.com/emersion/go-smtp"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ratelimit"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/ruleset"
)

// Rate-limit refusals inside a transaction are 450, never 5xx.
//
// The whole milestone is designed against one failure mode: a limit an operator
// set too low turning into permanently rejected mail. A 4xx costs delay and a
// retry; a 5xx costs the message. It is the same reasoning M7 applies to
// bouncing only on 5xx, seen from the other end.
func rateRefusal(what string) *smtp.SMTPError {
	return &smtp.SMTPError{
		Code:         450,
		EnhancedCode: smtp.EnhancedCode{4, 7, 1},
		Message:      "Rate limit exceeded for " + what + ", try again later",
	}
}

// checkMailRate applies the two message-rate limits at MAIL FROM.
//
// Both are checked, and the sender is checked FIRST, so an authenticated client
// whose sender is over its limit is told about the sender rather than about its
// login — the two are usually different things and the operator raising a limit
// needs to know which.
//
// Returns nil when the message may proceed.
func (s *session) checkMailRate(from string) error {
	l := s.b.limiter()
	if l == nil {
		return nil
	}

	// Lower-cased so one sender is one bucket. An address that differs only in
	// case is the same mailbox, and two buckets would double its allowance.
	if sender := strings.ToLower(strings.TrimSpace(from)); sender != "" {
		if !l.Allow(ratelimit.MsgPerSender, sender) {
			s.b.obs().RateSender.Add(1)
			s.log.Info("message refused: ratelimit.messages_per_sender",
				"from", from, "txn_uuid", s.txnUUID.String())
			return rateRefusal("this sender")
		}
	}
	// A null sender is deliberately not limited here: it has no id, every bounce
	// in the world shares it, and one bucket for all of them would refuse the
	// notifications this gateway most needs to deliver. Limiter.Allow already
	// treats an empty id as unlimited; the check above simply never asks.

	if user := strings.ToLower(s.authUser); user != "" {
		if !l.Allow(ratelimit.MsgPerUser, user) {
			s.b.obs().RateUser.Add(1)
			s.log.Info("message refused: ratelimit.messages_per_user",
				"user", s.authUser, "txn_uuid", s.txnUUID.String())
			return rateRefusal("this user")
		}
	}
	return nil
}

// checkRcptRate applies the per-destination-domain limit at RCPT TO.
//
// Per RECIPIENT and refused per recipient: the others in the same transaction
// are unaffected, which is the whole reason this is checked here rather than at
// DATA. It is an INBOUND control on where mail is going, and is not
// outbound.per_group_connections, which bounds connections to a relay.
func (s *session) checkRcptRate(to string) error {
	l := s.b.limiter()
	if l == nil {
		return nil
	}
	domain := strings.ToLower(ruleset.Domain(to))
	if domain == "" || l.Allow(ratelimit.RcptPerDomain, domain) {
		return nil
	}

	s.b.obs().RateRcptDomain.Add(1)
	s.log.Info("recipient refused: ratelimit.rcpts_per_domain",
		"rcpt", to, "domain", domain, "txn_uuid", s.txnUUID.String())
	return rateRefusal("this recipient domain")
}

// authRateKey is the bucket this peer's failed AUTH attempts are counted in, or
// "" when there is nothing to key on.
func (s *session) authRateKey() string {
	if s.b.limiter() == nil || !s.remoteIP.IsValid() {
		return ""
	}
	return s.remoteIP.Unmap().String()
}

// allowAuthAttempt reports whether another AUTH attempt from this peer may be
// judged at all.
//
// Checked BEFORE the password comparison, which is the point: a refusal here
// costs no bcrypt, so a credential-stuffing run cannot spend the CPU the
// delivery runner needs. M13 bounded concurrent comparisons to GOMAXPROCS,
// which put a floor under the damage; this is the limit that floor was standing
// in for.
//
// It only QUERIES the bucket. The allowance is spent by recordAuthFailure, so a
// client that authenticates correctly a hundred times is never affected however
// tight the limit is set — the key is called auth_failures_per_ip and it means
// it. Spending on every attempt instead would make it "AUTH commands per IP",
// which would throttle exactly the clients that are behaving.
//
// The refusal is 454 4.7.0, not 421, and that is a compromise worth recording.
// 421 means "service closing transmission channel" and is the honest code for
// what should happen here — but go-smtp's handleAuth hands an *SMTPError
// straight to writeError (conn.go:851-861) and never closes the connection, so
// answering 421 would announce a disconnection that does not occur. 454 is RFC
// 4954's "temporary authentication failure", it is true, and it is a 4xx. What
// actually bounds the connection is inactivity_timeout and max.connections.
func (s *session) allowAuthAttempt() error {
	key := s.authRateKey()
	if key == "" || !s.b.limiter().Blocked(ratelimit.AuthFailPerIP, key) {
		return nil
	}

	s.b.obs().RateAuth.Add(1)
	s.log.Warn("AUTH refused without checking the password: ratelimit.auth_failures_per_ip",
		"remote", s.remoteIP.String())
	return &smtp.SMTPError{
		Code:         454,
		EnhancedCode: smtp.EnhancedCode{4, 7, 0},
		Message:      "Too many authentication failures, try again later",
	}
}

// recordAuthFailure charges one failed attempt to this peer's budget.
func (s *session) recordAuthFailure() {
	if key := s.authRateKey(); key != "" {
		s.b.limiter().Spend(ratelimit.AuthFailPerIP, key)
	}
}
