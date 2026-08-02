package adminui

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed templates static
var assets embed.FS

// Each page is its own template set: they all define "content", which would
// collide in a single ParseFS set.
var pages = map[string]*template.Template{
	"wizard":   mustPage("wizard.html"),
	"status":   mustPage("status.html"),
	"filemode": mustPage("filemode.html"),
	"claim":    mustPage("claim.html"),
}

func mustPage(name string) *template.Template {
	return template.Must(template.ParseFS(assets, "templates/layout.html", "templates/"+name))
}

// pageData is what every template renders against.
type pageData struct {
	Version string
	Commit  string

	// File mode.
	FileMode  bool
	ConfigDir string

	State State

	// Authenticated drives the parts of the layout that only make sense to a
	// signed-in operator, such as the sign-out button.
	Authenticated bool
	// CSRF is this session's token. Every form that posts must carry it in a
	// hidden field; it is on pageData rather than passed per page so a new form
	// cannot be written without it being in scope.
	CSRF string
	// ClaimError is why the last claim attempt failed. Never echoes what was
	// submitted.
	ClaimError string

	// Wizard form state, preserved across a failed submission.
	FormURL      string
	FormInsecure bool
	FormCAFile   string
	FormError    string

	QueueReady      int
	QueueInflight   int
	QueueQuarantine int
	QueueDead       int
	QueueError      string

	// Refresh drives a meta-refresh while the gateway is waiting for an
	// operator. It is deliberately off once approved: a page that reloads
	// forever is a nuisance, and there is nothing left to watch for.
	Refresh bool
}

// render writes a page. The template is executed into a buffer first so a
// mid-render error cannot emit half a page under a 200.
func (s *Server) render(w http.ResponseWriter, name string, code int, data pageData) {
	tmpl, ok := pages[name]
	if !ok {
		s.log().Error("no such admin UI page", "page", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		s.log().Error("render failed", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Nothing here should ever be cached: it is all live state.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}

func (s *Server) baseData() pageData {
	d := pageData{Version: s.Version, Commit: s.Commit, ConfigDir: s.ConfigDir}
	if s.SpoolFn != nil {
		if spool := s.SpoolFn(); spool != nil {
			c, err := spool.LenAll()
			if err != nil {
				d.QueueError = err.Error()
			} else {
				d.QueueReady, d.QueueInflight = c.Ready, c.Inflight
				d.QueueQuarantine, d.QueueDead = c.Quarantine, c.Dead
			}
		}
	}
	return d
}

// renderClaim writes the one page an unauthenticated visitor may see.
//
// It builds its own data rather than calling baseData, for two reasons. The
// obvious one is that baseData's fields — the console URL, the approval state,
// the console's own error text, the applied and desired versions — are exactly
// what this milestone exists to stop leaking. The other is that baseData does
// filesystem work: Spool.LenAll stats several directories, and an
// unauthenticated caller must not be able to ask for that in a loop.
//
// The fingerprint is the deliberate exception. It exists before registration
// precisely so an operator can pre-approve this gateway in the console, and
// hiding it would break that quietly. It is sha256 of a public key, the console
// publishes it for approval anyway, and registration proves possession of the
// private half — so knowing it grants nothing.
func (s *Server) renderClaim(w http.ResponseWriter, code int, msg string) {
	data := pageData{Version: s.Version, Commit: s.Commit, ClaimError: msg}
	if s.State != nil {
		data.State = State{Fingerprint: s.State().Fingerprint}
	}
	s.render(w, "claim", code, data)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// A nil store means -config was given explicitly: this gateway runs from
	// files and is not centrally managed, so there is nothing to provision and
	// nothing to sign in to.
	if s.Store == nil {
		data := s.baseData()
		data.FileMode = true
		s.render(w, "filemode", http.StatusOK, data)
		return
	}

	sess := s.currentSession(r)
	if sess == nil {
		s.renderClaim(w, http.StatusOK, "")
		return
	}

	data := s.baseData()
	data.Authenticated, data.CSRF = true, sess.CSRF
	if s.State != nil {
		data.State = s.State()
	}

	if data.State.CentralURL == "" || data.State.GatewayUID == "" {
		// Unprovisioned, or a registration that did not complete. Pre-fill
		// whatever we already know so a retry is one click.
		data.FormURL = data.State.CentralURL
		data.FormInsecure = data.State.InsecureTLS
		data.FormCAFile = data.State.CAFile
		s.render(w, "wizard", http.StatusOK, data)
		return
	}

	data.Refresh = data.State.Approval != "approved"
	s.render(w, "status", http.StatusOK, data)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil || s.Register == nil {
		http.Error(w, "this gateway is not centrally managed", http.StatusNotFound)
		return
	}
	// A no-op: s.session has already parsed the form to check the CSRF token,
	// and ParseForm caches into r.PostForm. Kept so this handler still reads
	// correctly on its own.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	settings := Settings{
		CentralURL:  r.PostFormValue("central_url"),
		InsecureTLS: r.PostFormValue("insecure_tls") != "",
		CAFile:      strings.TrimSpace(r.PostFormValue("ca_file")),
	}

	fail := func(msg string) {
		data := s.baseData()
		if s.State != nil {
			data.State = s.State()
		}
		// The re-rendered form has to carry a usable token, or a corrected
		// retry would be refused as a forgery.
		if sess := s.currentSession(r); sess != nil {
			data.Authenticated, data.CSRF = true, sess.CSRF
		}
		data.FormURL = settings.CentralURL
		data.FormInsecure = settings.InsecureTLS
		data.FormCAFile = settings.CAFile
		data.FormError = msg
		s.render(w, "wizard", http.StatusBadRequest, data)
	}

	normalized, err := NormalizeCentralURL(settings.CentralURL)
	if err != nil {
		fail(err.Error())
		return
	}
	settings.CentralURL = normalized

	if settings.CAFile != "" {
		if _, err := os.ReadFile(settings.CAFile); err != nil {
			// Catch it here rather than as an opaque TLS failure later.
			fail(fmt.Sprintf("cannot read the CA bundle: %v", err))
			return
		}
	}

	// Deliberately not r.Context(): an operator navigating away mid-request
	// must not abort a registration that has already reached the console.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.Register(ctx, settings); err != nil {
		fail(err.Error())
		return
	}

	// POST/redirect/GET, so a browser refresh does not re-register.
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// NormalizeCentralURL validates and cleans the console URL from the wizard.
//
// Rejecting here rather than at first use means the operator finds out while
// looking at the form, and a value that could never work is never stored.
func NormalizeCentralURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("the Central Management URL is required")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("that is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("the URL must start with http:// or https://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("the URL must include a host")
	}

	// Credentials in a base URL would be echoed into every log line, and a
	// query or fragment on a base URL is always a mistake.
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")

	return u.String(), nil
}
