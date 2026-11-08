package httpapi

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gocart/web-service/internal/identity"
	"gocart/web-service/internal/oidc"
	"gocart/web-service/internal/session"
)

var (
	testSessionSecret  = []byte("session-secret-session-secret-0001")
	testIdentitySecret = []byte("identity-secret-identity-secret-02")
)

const testAdminKey = "the-real-admin-key"

func testAuth(t *testing.T) Auth {
	t.Helper()
	p, err := oidc.New(oidc.Config{Issuer: "http://idp.test/dex", ClientID: "gocart-web", ClientSecret: "s", RedirectURL: "http://localhost:3000/auth/callback"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Auth{OIDC: p, SessionSecret: testSessionSecret, IdentitySecret: testIdentitySecret, AdminKey: testAdminKey,
		AdminEmails: []string{"admin@gocart.test"}, SignInHint: "demo hint", PublicURL: "http://localhost:3000"}
}

// cookie is a valid session cookie header value for the test customer or admin.
func (f *fixture) cookie(admin bool) string {
	return sessionCookieFor(sess{Cust: "cust_abc", Email: "a@x", Name: "A", Admin: admin, Exp: time.Now().Add(time.Hour).Unix()})
}

func sessionCookieFor(s sess) string {
	v, _ := session.Seal(testSessionSecret, sessionPurpose, s)
	return sessionCookie + "=" + v
}

func TestWhoMayCallWhat(t *testing.T) {
	f := newFixture(t)
	type row struct {
		method, path string
		public, user bool // admin can always
	}
	rows := []row{
		{"GET", "/api/products", true, true}, {"GET", "/api/products/prd_x", true, true},
		{"POST", "/api/products", false, false}, {"PUT", "/api/products/prd_x/stock", false, false},
		{"GET", "/api/orders", false, true}, {"POST", "/api/orders", false, true}, {"GET", "/api/orders/ord_1", false, true},
		{"POST", "/api/orders/ord_1/cancel", false, true},
		{"GET", "/api/orders/ord_1/events", false, false}, {"POST", "/api/orders/ord_1/ship", false, false}, {"POST", "/api/orders/ord_1/deliver", false, false},
		{"GET", "/api/carts/cust_abc", false, true}, {"DELETE", "/api/carts/cust_abc", false, true},
		{"PUT", "/api/carts/cust_abc/items/prd_x", false, true}, {"DELETE", "/api/carts/cust_abc/items/prd_x", false, true},
		{"POST", "/api/carts/cust_abc/checkout", false, true},
		{"GET", "/api/charges/ord_1", false, false}, {"GET", "/api/status", false, false},
	}
	for _, r := range rows {
		name := r.method + " " + r.path
		anon := f.anon(r.method, r.path, "{}")
		cust := f.asCustomer(r.method, r.path, "{}")
		adm := f.do(r.method, r.path, "{}")
		if r.public {
			if anon.Code == 401 || anon.Code == 403 {
				t.Errorf("%s: anonymous refused (%d)", name, anon.Code)
			}
		} else {
			wantProblem(t, anon, 401, "unauthenticated", name+" anonymous")
		}
		if r.user {
			if cust.Code == 401 || cust.Code == 403 {
				t.Errorf("%s: a signed-in customer was refused (%d)", name, cust.Code)
			}
		} else {
			wantProblem(t, cust, 403, "forbidden", name+" as customer")
		}
		if adm.Code == 401 || adm.Code == 403 {
			t.Errorf("%s: an administrator was refused (%d)", name, adm.Code)
		}
	}
}

func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ, what string) {
	t.Helper()
	var p map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != status || p["type"] != "urn:gocart:problem:"+typ {
		t.Errorf("%s: got %d %v, want %d %s", what, rec.Code, p["type"], status, typ)
	}
}

func TestStateChangingCallsMustCarryTheAppsHeader(t *testing.T) {
	f := newFixture(t)
	for _, c := range [][2]string{{"PUT", "/api/carts/cust_abc/items/prd_x"}, {"DELETE", "/api/carts/cust_abc/items/prd_x"}, {"POST", "/api/carts/cust_abc/checkout"}} {
		rec := f.send(c[0], c[1], `{"quantity":1}`, "Cookie", f.cookie(false))
		wantProblem(t, rec, 403, "csrf", c[0]+" "+c[1])
	}
	if f.cart.path != "" {
		t.Fatal("a request without the header reached the cart service")
	}
	if rec := f.send("GET", "/api/carts/cust_abc", "", "Cookie", f.cookie(false)); rec.Code != 200 {
		t.Errorf("reads need no header: %d", rec.Code)
	}
}

func TestNoSessionAndBadSessionsAreTheSame(t *testing.T) {
	f := newFixture(t)
	good := sess{Cust: "cust_abc", Exp: time.Now().Add(time.Hour).Unix()}
	expired := sess{Cust: "cust_abc", Exp: time.Now().Add(-time.Second).Unix()}
	forged, _ := session.Seal([]byte("not-the-gateways-secret-not-the-gateway"), sessionPurpose, good)
	wrongPurpose, _ := session.Seal(testSessionSecret, loginPurpose, good)
	for name, c := range map[string]string{
		"expired":       sessionCookieFor(expired),
		"forged":        sessionCookie + "=" + forged,
		"wrong purpose": sessionCookie + "=" + wrongPurpose,
		"garbage":       sessionCookie + "=abc",
		"login cookie":  loginCookie + "=" + wrongPurpose,
	} {
		wantProblem(t, f.send("GET", "/api/carts/cust_abc", "", "Cookie", c), 401, "unauthenticated", name)
	}
	if rec := f.send("GET", "/api/carts/cust_abc", "", "Cookie", sessionCookieFor(good)); rec.Code != 200 {
		t.Errorf("control: %d", rec.Code)
	}
}

func TestServicesAreToldWhoIsCallingAndNothingTheBrowserSentSurvives(t *testing.T) {
	f := newFixture(t)
	spoof := []string{
		"Authorization", "Bearer the-browser-guessed-this",
		identity.Header, identity.Sign(testIdentitySecret, identity.Claims{Sub: "cust_victim", Role: identity.RoleAdmin, Exp: time.Now().Add(time.Hour).Unix()}),
	}

	f.asCustomer("GET", "/api/carts/cust_abc", "", spoof...)
	h := f.cart.headers
	if h.Get("Authorization") != "" {
		t.Errorf("a customer's request carried an Authorization header: %q", h.Get("Authorization"))
	}
	if h.Get("Cookie") != "" {
		t.Error("the session cookie was passed to a service")
	}
	c, err := identity.Verify(testIdentitySecret, h.Get(identity.Header), time.Now())
	if err != nil || c.Sub != "cust_abc" || c.Role != identity.RoleCustomer {
		t.Fatalf("what the service was told: %+v %v", c, err)
	}
	if time.Until(time.Unix(c.Exp, 0)) > time.Minute {
		t.Error("the assertion must be short-lived")
	}

	f.do("GET", "/api/orders", "", spoof...)
	h = f.order.headers
	if h.Get("Authorization") != "Bearer "+testAdminKey {
		t.Errorf("an administrator's request must carry the real admin key, got %q", h.Get("Authorization"))
	}
	if c, err := identity.Verify(testIdentitySecret, h.Get(identity.Header), time.Now()); err != nil || !c.Admin() || c.Sub != "cust_abc" {
		t.Fatalf("admin assertion: %+v %v", c, err)
	}

	// no session on a public route: no identity at all, and the browser's claims are still dropped
	f.send("GET", "/api/products", "", spoof...)
	if f.product.headers.Get("Authorization") != "" || f.product.headers.Get(identity.Header) != "" {
		t.Errorf("anonymous request vouched for: %v", f.product.headers)
	}
}

func TestMeReportsWhoIsSignedIn(t *testing.T) {
	f := newFixture(t)
	var m map[string]any
	rec := f.send("GET", "/api/me", "")
	json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != 200 || m["signed_in"] != false || m["hint"] != "demo hint" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous: %d %v", rec.Code, m)
	}
	rec = f.send("GET", "/api/me", "", "Cookie", f.cookie(true))
	m = nil
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["signed_in"] != true || m["customer_id"] != "cust_abc" || m["role"] != "admin" || m["name"] != "A" {
		t.Fatalf("%v", m)
	}
	rec = f.send("GET", "/api/me", "", "Cookie", f.cookie(false))
	m = nil
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["role"] != "customer" {
		t.Fatalf("%v", m)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/cart": "/cart", "/orders/ord_1": "/orders/ord_1", "": "/shop",
		"https://evil.example": "/shop", "//evil.example": "/shop", "/\\evil.example": "/shop",
		"cart": "/shop", "/auth/login": "/shop", "/api/orders": "/shop", "/x\r\nSet-Cookie: a=b": "/shop",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogoutNeedsTheHeaderAndClearsTheSession(t *testing.T) {
	f := newFixture(t)
	wantProblem(t, f.send("POST", "/auth/logout", "", "Cookie", f.cookie(false)), 403, "csrf", "no header")
	rec := f.asCustomer("POST", "/auth/logout", "")
	if rec.Code != 204 {
		t.Fatalf("%d", rec.Code)
	}
	if c := rec.Header().Get("Set-Cookie"); !strings.Contains(c, sessionCookie+"=;") || !strings.Contains(c, "Max-Age=0") {
		t.Errorf("session not cleared: %q", c)
	}
}

// ---- the whole sign-in, against a stand-in identity provider ----

type fakeIDP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	mu    sync.Mutex
	nonce string // what the next ID token will carry
	claim map[string]any
	fail  bool
	forms []url.Values
}

var idpKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	p := &fakeIDP{key: idpKey()}
	b64 := base64.RawURLEncoding
	mux := http.NewServeMux()
	mux.HandleFunc("/dex/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": p.issuer(), "authorization_endpoint": p.issuer() + "/auth",
			"token_endpoint": p.issuer() + "/token", "jwks_uri": p.issuer() + "/keys"})
	})
	mux.HandleFunc("/dex/keys", func(w http.ResponseWriter, r *http.Request) {
		pub := p.key.PublicKey
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "kid": "k", "alg": "RS256",
			"n": b64.EncodeToString(pub.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes())}}})
	})
	mux.HandleFunc("/dex/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.mu.Lock()
		defer p.mu.Unlock()
		p.forms = append(p.forms, r.PostForm)
		if p.fail {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		c := map[string]any{"iss": p.issuer(), "sub": "u-asha", "aud": "gocart-web", "exp": time.Now().Add(time.Hour).Unix(),
			"nonce": p.nonce, "email": "user1@gocart.test", "email_verified": true, "name": "Asha"}
		for k, v := range p.claim {
			c[k] = v
		}
		h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k"})
		body, _ := json.Marshal(c)
		signed := b64.EncodeToString(h) + "." + b64.EncodeToString(body)
		sum := sha256.Sum256([]byte(signed))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
		json.NewEncoder(w).Encode(map[string]string{"id_token": signed + "." + b64.EncodeToString(sig)})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIDP) issuer() string { return p.srv.URL + "/dex" }

func signInFixture(t *testing.T) (*fixture, *fakeIDP) {
	t.Helper()
	idp := newFakeIDP(t)
	f := newFixture(t)
	a := testAuth(t)
	prov, err := oidc.New(oidc.Config{Issuer: idp.issuer(), ClientID: "gocart-web", ClientSecret: "client-secret", RedirectURL: "http://localhost:3000/auth/callback"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.OIDC = prov
	srv, err := New(Upstreams{Product: f.product.srv.URL, Order: f.order.srv.URL, Cart: f.cart.srv.URL, Payment: f.y.srv.URL},
		app, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAuth(a))
	if err != nil {
		t.Fatal(err)
	}
	f.srv, f.h = srv, srv.Handler()
	return f, idp
}

// startLogin performs /auth/login and returns the provider redirect, the login cookie, and the state.
func startLogin(t *testing.T, f *fixture, idp *fakeIDP, next string) (loc *url.URL, loginCookieHeader, state string) {
	t.Helper()
	rec := f.send("GET", "/auth/login?next="+url.QueryEscape(next), "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	loc, _ = url.Parse(rec.Header().Get("Location"))
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != loginCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie: %+v", cookies)
	}
	idp.mu.Lock()
	idp.nonce = loc.Query().Get("nonce")
	idp.mu.Unlock()
	return loc, cookies[0].Name + "=" + cookies[0].Value, loc.Query().Get("state")
}

func TestSigningInEndToEnd(t *testing.T) {
	f, idp := signInFixture(t)
	loc, lc, state := startLogin(t, f, idp, "/cart")

	q := loc.Query()
	if !strings.HasPrefix(loc.String(), idp.issuer()+"/auth?") || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || state == "" || q.Get("nonce") == "" {
		t.Fatalf("bad provider redirect: %s", loc)
	}

	rec := f.send("GET", "/auth/callback?code=abc&state="+url.QueryEscape(state), "", "Cookie", lc)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/cart" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge > 0 {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Secure {
		t.Fatalf("session cookie: %+v", session)
	}
	// the code was exchanged with the PKCE verifier whose hash was sent in the challenge
	form := idp.forms[0]
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if form.Get("code") != "abc" || form.Get("client_secret") != "client-secret" || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") {
		t.Errorf("token request: %v", form)
	}

	me := f.send("GET", "/api/me", "", "Cookie", session.Name+"="+session.Value)
	var m map[string]any
	json.Unmarshal(me.Body.Bytes(), &m)
	if m["signed_in"] != true || m["name"] != "Asha" || m["email"] != "user1@gocart.test" || m["role"] != "customer" ||
		m["customer_id"] != customerID(idp.issuer(), "u-asha") || !strings.HasPrefix(m["customer_id"].(string), "cust_") {
		t.Fatalf("me: %v", m)
	}
	// and the id is the same next time, and different for someone else
	if customerID(idp.issuer(), "u-asha") != customerID(idp.issuer(), "u-asha") || customerID(idp.issuer(), "u-asha") == customerID(idp.issuer(), "u-ravi") {
		t.Error("customer ids must be stable per person and distinct between people")
	}
}

func TestAdminsAreChosenByVerifiedEmail(t *testing.T) {
	f, idp := signInFixture(t)
	idp.claim = map[string]any{"email": "Admin@GoCart.test"}
	_, lc, state := startLogin(t, f, idp, "/admin")
	rec := f.send("GET", "/auth/callback?code=c&state="+url.QueryEscape(state), "", "Cookie", lc)
	var ck string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge > 0 {
			ck = c.Name + "=" + c.Value
		}
	}
	var m map[string]any
	json.Unmarshal(f.send("GET", "/api/me", "", "Cookie", ck).Body.Bytes(), &m)
	if m["role"] != "admin" {
		t.Fatalf("%v", m)
	}

	// an unverified address, even the admin's, does not sign in at all
	idp.claim = map[string]any{"email": "admin@gocart.test", "email_verified": false}
	_, lc, state = startLogin(t, f, idp, "/admin")
	rec = f.send("GET", "/auth/callback?code=c&state="+url.QueryEscape(state), "", "Cookie", lc)
	if rec.Header().Get("Location") != "/?signin=unverified" || strings.Contains(rec.Header().Get("Set-Cookie"), sessionCookie+"=v") {
		t.Fatalf("unverified: %s", rec.Header().Get("Location"))
	}
}

func TestSignInFailuresSendThePersonBackWithAReason(t *testing.T) {
	f, idp := signInFixture(t)
	check := func(name, want string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?signin="+want {
			t.Errorf("%s: %d %q, want /?signin=%s", name, rec.Code, rec.Header().Get("Location"), want)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie && c.MaxAge > 0 {
				t.Errorf("%s: a session was started", name)
			}
		}
	}

	_, lc, state := startLogin(t, f, idp, "/")
	check("no login cookie", "expired", f.send("GET", "/auth/callback?code=c&state="+state, ""))
	check("wrong state", "failed", f.send("GET", "/auth/callback?code=c&state=forged", "", "Cookie", lc))
	check("provider refused", "denied", f.send("GET", "/auth/callback?error=access_denied&state="+url.QueryEscape(state), "", "Cookie", lc))
	check("no code", "failed", f.send("GET", "/auth/callback?state="+url.QueryEscape(state), "", "Cookie", lc))

	idp.fail = true
	check("provider rejects the code", "failed", f.send("GET", "/auth/callback?code=c&state="+url.QueryEscape(state), "", "Cookie", lc))
	idp.fail = false

	idp.claim = map[string]any{"aud": "someone-else"}
	check("token for another app", "failed", f.send("GET", "/auth/callback?code=c&state="+url.QueryEscape(state), "", "Cookie", lc))
	idp.claim = nil

	// a login cookie is not a session, and an old one cannot be replayed after its window
	old, _ := session.Seal(testSessionSecret, loginPurpose, flow{State: "s", Nonce: "n", Verifier: "v", Next: "/", Exp: time.Now().Add(-time.Minute).Unix()})
	check("stale login", "expired", f.send("GET", "/auth/callback?code=c&state=s", "", "Cookie", loginCookie+"="+old))

}

func TestAProviderThatIsDownMeansSignInIsUnavailableNotBroken(t *testing.T) {
	f, idp := signInFixture(t)
	idp.srv.Close() // down before it was ever asked
	rec := f.send("GET", "/auth/login", "")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?signin=unavailable" {
		t.Errorf("provider down at login: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.anon("GET", "/api/products", ""); rec.Code != 200 {
		t.Errorf("browsing must not depend on the identity provider: %d", rec.Code)
	}
}

func TestACodeCannotBeUsedWithSomeoneElsesLoginCookie(t *testing.T) {
	f, idp := signInFixture(t)
	_, lcA, stateA := startLogin(t, f, idp, "/")
	_, _, stateB := startLogin(t, f, idp, "/")
	rec := f.send("GET", "/auth/callback?code=c&state="+url.QueryEscape(stateB), "", "Cookie", lcA)
	if rec.Header().Get("Location") != "/?signin=failed" {
		t.Fatalf("state from one login with the cookie of another was accepted: %s (stateA %s)", rec.Header().Get("Location"), stateA)
	}
}

func TestSecureCookiesOverHTTPS(t *testing.T) {
	f := newFixture(t)
	a := testAuth(t)
	a.PublicURL = "https://shop.example"
	srv, _ := New(Upstreams{Product: f.product.srv.URL, Order: f.order.srv.URL, Cart: f.cart.srv.URL, Payment: f.y.srv.URL}, app,
		slog.New(slog.NewTextHandler(io.Discard, nil)), WithAuth(a))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/auth/logout", nil))
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/auth/logout", nil)
	req.Header.Set(csrfHeader, csrfValue)
	srv.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Secure") {
		t.Errorf("cookies must be Secure when the site is https: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestTheGatewayWillNotStartWithoutSignIn(t *testing.T) {
	up := Upstreams{"http://a:1", "http://b:1", "http://c:1", "http://d:1"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(up, app, log); err == nil {
		t.Error("started with no sign-in configured")
	}
	a := testAuth(t)
	a.SessionSecret = []byte("short")
	if _, err := New(up, app, log, WithAuth(a)); err == nil {
		t.Error("accepted a short session secret")
	}
	a = testAuth(t)
	a.IdentitySecret = nil
	if _, err := New(up, app, log, WithAuth(a)); err == nil {
		t.Error("accepted no identity secret")
	}
}
