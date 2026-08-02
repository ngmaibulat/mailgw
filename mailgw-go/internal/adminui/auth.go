package adminui

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

// cookieName is the admin session cookie.
const cookieName = "mailgw_admin"

// Claim-attempt rate limit: a bucket of burst tokens refilling at refillPerSec.
//
// Deliberately in memory rather than in the store. A counter in `settings`
// would make POST /claim an UNAUTHENTICATED WRITE to the same SQLite file the
// poll loop and every page view use — a denial-of-service primitive this
// process does not currently have — bought against a brute-force attack that
// 100 bits of claim code already makes impossible. What this bucket is actually
// for is keeping a scanner from filling the log and burning CPU.
const (
	claimBurst  = 5
	claimRefill = 1.0 // tokens per second
)

// allowClaimAttempt reports whether another claim attempt may be processed.
//
// It never sleeps. Sleeping on failure would let an attacker park goroutines
// inside the 30s ReadTimeout until a real operator's claim queued behind them,
// which turns the throttle into the outage it exists to prevent. Refuse fast.
func (s *Server) allowClaimAttempt() bool {
	s.claimMu.Lock()
	defer s.claimMu.Unlock()

	now := time.Now()
	if s.claimLast.IsZero() {
		s.claimTokens, s.claimLast = claimBurst, now
	} else {
		s.claimTokens += now.Sub(s.claimLast).Seconds() * claimRefill
		s.claimTokens = min(s.claimTokens, claimBurst)
		s.claimLast = now
	}
	if s.claimTokens < 1 {
		return false
	}
	s.claimTokens--
	return true
}

// currentSession resolves the request's session, or nil when there is none.
//
// It is a store read on every page view and deliberately not cached on the
// Server: an in-memory copy would keep honouring a session that
// `mailgw-go claim reset`, run from a second process, had already revoked. The
// store is one connection to a local file and the busiest page here is a
// five-second meta-refresh.
func (s *Server) currentSession(r *http.Request) *store.AdminSession {
	if s.Store == nil {
		return nil
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	sess, err := s.Store.Session(c.Value)
	if err != nil {
		s.log().Error("cannot read the admin session", "err", err)
		return nil
	}
	return sess
}

// session gates a handler behind a valid session and, for anything that is not
// a GET, a matching CSRF token.
//
// CSRF lives here rather than in each handler so that every route added later
// inherits it by being wrapped, and a route that forgets the wrapper is a
// visible omission in Handler's ten-line table rather than an invisible hole.
//
// In file mode (Store == nil) it passes straight through: that gateway owns its
// configuration on disk, has nothing to provision, and POST /register already
// answers 404.
func (s *Server) session(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Store == nil {
			next(w, r)
			return
		}

		sess := s.currentSession(r)
		if sess == nil {
			http.Error(w, "not signed in: open / and present this gateway's claim code",
				http.StatusForbidden)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// ParseForm caches into r.PostForm, so the handler's own ParseForm
			// is a no-op afterwards rather than a second read of the body.
			if err := r.ParseForm(); err != nil {
				http.Error(w, "malformed form", http.StatusBadRequest)
				return
			}
			if subtle.ConstantTimeCompare(
				[]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
				s.log().Warn("admin UI request with a bad CSRF token",
					"path", r.URL.Path, "remote", r.RemoteAddr)
				http.Error(w, "this form has expired — reload the page and try again",
					http.StatusForbidden)
				return
			}
		}

		next(w, r)
	}
}

// bearer gates a handler behind the configured token, and lets everything
// through when there is none.
//
// Open-when-unset preserves today's behaviour for every operator who has
// firewalled this port and does not want to re-plumb their scraper, and it is
// also the only thing that can be true before the first configuration applies —
// the token arrives in a bundle the gateway has not read yet.
func (s *Server) bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if s.MetricsToken != nil {
			token = s.MetricsToken()
		}
		if token == "" {
			next(w, r)
			return
		}

		// Header only. A token in a query parameter would be written to every
		// access log and proxy trace between the scraper and here.
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			// 401 rather than 403: a scraper arriving without a credential
			// should be told to present one, which is what WWW-Authenticate says.
			w.Header().Set("WWW-Authenticate", `Bearer realm="mailgw-go"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleClaim takes the claim code and mints a session.
//
// There is deliberately no CSRF token on this route: it IS the sign-in, so
// there is no session to bind a token to, and an attacker who could forge the
// request would have to know the code — at which point they would simply sign
// in themselves.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		http.Error(w, "this gateway is not centrally managed", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	if !s.allowClaimAttempt() {
		http.Error(w, "too many attempts — wait a few seconds", http.StatusTooManyRequests)
		return
	}

	ok, err := s.Store.CheckClaimCode(r.PostFormValue("code"))
	if err != nil {
		s.log().Error("cannot check the claim code", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Never the submitted value: it lands in a log an operator pastes into
		// a ticket, and a near-miss is still most of a secret.
		s.log().Warn("admin UI claim refused", "remote", r.RemoteAddr)
		s.renderClaim(w, http.StatusUnauthorized, "That code is not right.")
		return
	}

	sess, err := s.Store.NewSession()
	if err != nil {
		s.log().Error("cannot create an admin session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, r, sess)
	s.log().Info("admin UI claimed", "remote", r.RemoteAddr)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout ends this browser's session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		http.Error(w, "this gateway is not centrally managed", http.StatusNotFound)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if err := s.Store.DeleteSession(c.Value); err != nil {
			s.log().Error("cannot delete the admin session", "err", err)
		}
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// setSessionCookie writes the session cookie.
//
// No Max-Age, so this is a browser-session cookie and the server-side
// expires_at is the only thing that decides how long a session really lives.
//
// Secure is set only when the request arrived over TLS. Setting it
// unconditionally would make the browser refuse to store the cookie over
// http://, which is what this listener serves today, and the UI would silently
// never sign anyone in. This way the flag is correct now and stays correct if
// the listener ever gains TLS.
//
// SameSite=Lax rather than Strict: every mutating route is a POST carrying a
// per-session CSRF token, so Strict would add only protection against top-level
// GET navigation, and would cost one baffling report — an operator following a
// link from another origin gets no cookie on that first navigation and lands on
// the claim page as though they had never signed in.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, sess *store.AdminSession) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}
