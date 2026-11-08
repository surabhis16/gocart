package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"gocart/web-service/internal/identity"
	"gocart/web-service/internal/oidc"
	"gocart/web-service/internal/session"
)

// Sign-in works like this:
//
//  1. /auth/login makes a one-time state, nonce and PKCE verifier, keeps them
//     in a short-lived signed cookie, and sends the browser to the identity
//     provider.
//  2. The person signs in there. The provider sends the browser back to
//     /auth/callback with a one-time code.
//  3. The gateway swaps the code for an ID token (proving it holds the PKCE
//     verifier and the client secret), checks the token, and sets a signed
//     session cookie. No password or token ever reaches the page's JavaScript.
//  4. On each API call the gateway reads the session and forwards the request
//     with a signed statement of who is calling (internal/identity).
//
// The session cookie is signed, not stored: any gateway replica can serve any
// request. The price is that a session cannot be revoked before it expires
// (eight hours), which is acceptable here and is said so in the architecture
// notes.

const (
	sessionCookie  = "gocart_session"
	loginCookie    = "gocart_login"
	sessionPurpose = "session/v1"
	loginPurpose   = "login/v1"
	loginWindow    = 10 * time.Minute
	assertionTTL   = 30 * time.Second
	csrfHeader     = "X-Requested-With"
	csrfValue      = "gocart"
)

// Auth is everything sign-in needs.
type Auth struct {
	OIDC           *oidc.Provider
	SessionSecret  []byte // signs the session cookie
	IdentitySecret []byte // signs what is sent to the services; they hold the same secret
	AdminKey       string // sent to the services for administrators only
	AdminEmails    []string
	SignInHint     string // shown on the landing page, for demo accounts; empty in a real deployment
	PublicURL      string // where people reach the gateway; https turns on Secure cookies
	SessionTTL     time.Duration
}

// WithAuth sets up sign-in. The gateway refuses to start without it.
func WithAuth(a Auth) Option { return func(s *Server) { s.auth = a } }

var errNoAuth = errors.New("httpapi: sign-in is not configured")

func (a *Auth) check() error {
	switch {
	case a.OIDC == nil:
		return errNoAuth
	case len(a.SessionSecret) < identity.MinSecretLen:
		return errors.New("httpapi: SESSION_SECRET must be at least 32 characters")
	case len(a.IdentitySecret) < identity.MinSecretLen:
		return errors.New("httpapi: IDENTITY_SECRET must be at least 32 characters")
	}
	if a.SessionTTL == 0 {
		a.SessionTTL = 8 * time.Hour
	}
	return nil
}

func (a *Auth) secure() bool { return strings.HasPrefix(a.PublicURL, "https://") }

func (a *Auth) isAdmin(email string) bool {
	for _, e := range a.AdminEmails {
		if email != "" && strings.EqualFold(strings.TrimSpace(e), email) {
			return true
		}
	}
	return false
}

// customerID is the id the services know a person by: stable for the same
// person at the same provider, and not guessable from anything public.
func customerID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "|" + subject))
	return "cust_" + hex.EncodeToString(sum[:8])
}

type access int

const (
	public access = iota
	user
	admin
)

type sess struct {
	Cust  string `json:"c"`
	Email string `json:"e"`
	Name  string `json:"n"`
	Admin bool   `json:"a,omitempty"`
	Exp   int64  `json:"x"`
}

type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"u"`
	Exp      int64  `json:"x"`
}

type sessKey struct{}

func (s *Server) readSession(r *http.Request) (sess, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return sess{}, false
	}
	var v sess
	if session.Open(s.auth.SessionSecret, sessionPurpose, c.Value, &v) != nil || time.Now().Unix() >= v.Exp || v.Cust == "" {
		return sess{}, false
	}
	return v, true
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.auth.secure(), SameSite: http.SameSiteLaxMode,
	})
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// guard enforces who may call a route, and that a state-changing call really
// came from this app: browsers cannot add a custom header to a cross-site
// request without the target's permission, and the gateway grants none.
func (s *Server) guard(level access, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if unsafeMethod(r.Method) && r.Header.Get(csrfHeader) != csrfValue {
			writeProblem(w, r, Problem{Type: "csrf", Title: "Request not accepted", Status: http.StatusForbidden,
				Detail: "State-changing requests must carry " + csrfHeader + ": " + csrfValue + "."})
			return
		}
		ss, ok := s.readSession(r)
		if level >= user && !ok {
			writeProblem(w, r, Problem{Type: "unauthenticated", Title: "Sign in required", Status: http.StatusUnauthorized,
				Detail: "Sign in to use this."})
			return
		}
		if level == admin && !ss.Admin {
			writeProblem(w, r, Problem{Type: "forbidden", Title: "Administrators only", Status: http.StatusForbidden,
				Detail: "This is only available to administrators."})
			return
		}
		if ok {
			r = r.WithContext(context.WithValue(r.Context(), sessKey{}, ss))
		}
		h(w, r)
	}
}

// vouch decides what identity the upstream request carries. Whatever the
// browser sent under these names is thrown away first: nobody can claim to be
// someone else, or to hold the admin key, by setting a header.
func (s *Server) vouch(pr *httputil.ProxyRequest) {
	h := pr.Out.Header
	h.Del("Cookie")
	h.Del("Authorization")
	h.Del(identity.Header)
	ss, ok := pr.In.Context().Value(sessKey{}).(sess)
	if !ok {
		return
	}
	role := identity.RoleCustomer
	if ss.Admin {
		role = identity.RoleAdmin
		if s.auth.AdminKey != "" {
			h.Set("Authorization", "Bearer "+s.auth.AdminKey)
		}
	}
	h.Set(identity.Header, identity.Sign(s.auth.IdentitySecret, identity.Claims{
		Sub: ss.Cust, Role: role, Exp: time.Now().Add(assertionTTL).Unix(),
	}))
}

// safeNext only lets people be sent back to a path on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") ||
		strings.HasPrefix(next, "/auth/") || strings.HasPrefix(next, "/api/") {
		return "/shop"
	}
	return next
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	verifier, challenge := oidc.PKCE()
	f := flow{State: oidc.Random(16), Nonce: oidc.Random(16), Verifier: verifier, Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(loginWindow).Unix()}
	to, err := s.auth.OIDC.AuthURL(r.Context(), f.State, f.Nonce, challenge)
	if err != nil {
		s.log.Warn("sign-in unavailable", "err", err)
		s.metrics.logins("unavailable")
		http.Redirect(w, r, "/?signin=unavailable", http.StatusSeeOther)
		return
	}
	sealed, _ := session.Seal(s.auth.SessionSecret, loginPurpose, f)
	s.setCookie(w, loginCookie, sealed, int(loginWindow.Seconds()))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	fail := func(reason string, err error) {
		s.log.Warn("sign-in failed", "reason", reason, "err", err)
		s.metrics.logins(reason)
		s.setCookie(w, loginCookie, "", -1)
		http.Redirect(w, r, "/?signin="+reason, http.StatusSeeOther)
	}
	var f flow
	c, err := r.Cookie(loginCookie)
	if err != nil || session.Open(s.auth.SessionSecret, loginPurpose, c.Value, &f) != nil || time.Now().Unix() >= f.Exp {
		fail("expired", err)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		fail("failed", errors.New("state does not match"))
		return
	}
	if q.Get("error") != "" {
		fail("denied", errors.New(q.Get("error")))
		return
	}
	code := q.Get("code")
	if code == "" {
		fail("failed", errors.New("no code"))
		return
	}
	raw, err := s.auth.OIDC.Exchange(r.Context(), code, f.Verifier)
	if err == nil {
		var claims oidc.Claims
		if claims, err = s.auth.OIDC.Verify(r.Context(), raw, f.Nonce, time.Now()); err == nil {
			if !claims.EmailVerified {
				fail("unverified", errors.New("email not verified"))
				return
			}
			name := claims.Name
			if name == "" {
				name = claims.Email
			}
			sealed, _ := session.Seal(s.auth.SessionSecret, sessionPurpose, sess{
				Cust: customerID(s.auth.OIDC.Issuer(), claims.Subject), Email: claims.Email, Name: name,
				Admin: s.auth.isAdmin(claims.Email), Exp: time.Now().Add(s.auth.SessionTTL).Unix(),
			})
			s.setCookie(w, loginCookie, "", -1)
			s.setCookie(w, sessionCookie, sealed, int(s.auth.SessionTTL.Seconds()))
			s.metrics.logins("ok")
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, f.Next, http.StatusSeeOther)
			return
		}
	}
	if errors.Is(err, oidc.ErrUnavailable) {
		fail("unavailable", err)
		return
	}
	fail("failed", err)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != csrfValue {
		writeProblem(w, r, Problem{Type: "csrf", Title: "Request not accepted", Status: http.StatusForbidden})
		return
	}
	s.setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// me tells the app who is signed in. Anonymous is a normal answer (200), so
// the app can ask on every load without an error showing up in the console.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ss, ok := s.readSession(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"signed_in": false, "hint": s.auth.SignInHint})
		return
	}
	role := identity.RoleCustomer
	if ss.Admin {
		role = identity.RoleAdmin
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"signed_in": true, "customer_id": ss.Cust, "name": ss.Name, "email": ss.Email, "role": role,
	})
}
