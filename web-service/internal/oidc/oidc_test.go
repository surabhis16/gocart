package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var b64 = base64.RawURLEncoding

var (
	keyOnce sync.Once
	keyVal  *rsa.PrivateKey
)

// testKey is generated once for the whole test run: 2048-bit generation is slow under -race.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if keyVal, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return keyVal
}

// idp is a stand-in identity provider: discovery, a key set, and a token endpoint.
type idp struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	kid       string
	jwksCalls atomic.Int32
	tokenForm url.Values // what the last token request carried
	issue     func() string
	status    int
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	p := &idp{key: testKey(t), kid: "k1", status: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("/dex/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 p.issuer(),
			"authorization_endpoint": p.issuer() + "/auth",
			"token_endpoint":         p.issuer() + "/token",
			"jwks_uri":               p.issuer() + "/keys",
		})
	})
	mux.HandleFunc("/dex/keys", func(w http.ResponseWriter, r *http.Request) {
		p.jwksCalls.Add(1)
		pub := p.key.PublicKey
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "kid": p.kid, "alg": "RS256",
			"n": b64.EncodeToString(pub.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/dex/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.tokenForm = r.PostForm
		if p.status != 200 {
			w.WriteHeader(p.status)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": p.issue(), "access_token": "x", "token_type": "bearer"})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) issuer() string { return p.srv.URL + "/dex" }

// token signs claims with the idp's key; alg and kid can be overridden to build bad tokens.
func (p *idp) token(claims map[string]any, alg, kid string) string {
	return p.tokenWith(p.key, claims, alg, kid)
}

func (p *idp) tokenWith(key *rsa.PrivateKey, claims map[string]any, alg, kid string) string {
	if alg == "" {
		alg = "RS256"
	}
	if kid == "" {
		kid = p.kid
	}
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signed := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sum := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	return signed + "." + b64.EncodeToString(sig)
}

func (p *idp) claims(over map[string]any) map[string]any {
	c := map[string]any{
		"iss": p.issuer(), "sub": "user-1", "aud": "gocart-web", "exp": time.Now().Add(time.Hour).Unix(),
		"nonce": "n1", "email": "user1@gocart.test", "email_verified": true, "name": "Asha",
	}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func (p *idp) provider(t *testing.T, mutate ...func(*Config)) *Provider {
	t.Helper()
	cfg := Config{Issuer: p.issuer(), ClientID: "gocart-web", ClientSecret: "s3cret", RedirectURL: "http://localhost:3000/auth/callback"}
	for _, m := range mutate {
		m(&cfg)
	}
	pr, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func TestAValidTokenIsAccepted(t *testing.T) {
	p := newIDP(t)
	c, err := p.provider(t).Verify(context.Background(), p.token(p.claims(nil), "", ""), "n1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "user-1" || c.Email != "user1@gocart.test" || c.Name != "Asha" || !c.EmailVerified {
		t.Fatalf("%+v", c)
	}
}

func TestBadTokensAreRejected(t *testing.T) {
	p := newIDP(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := map[string]string{
		"wrong issuer":         p.token(p.claims(map[string]any{"iss": "https://evil.example"}), "", ""),
		"wrong audience":       p.token(p.claims(map[string]any{"aud": "someone-else"}), "", ""),
		"expired":              p.token(p.claims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), "", ""),
		"no expiry":            p.token(p.claims(map[string]any{"exp": nil}), "", ""),
		"wrong nonce":          p.token(p.claims(map[string]any{"nonce": "replayed"}), "", ""),
		"no nonce":             p.token(p.claims(map[string]any{"nonce": nil}), "", ""),
		"no subject":           p.token(p.claims(map[string]any{"sub": nil}), "", ""),
		"alg none":             p.token(p.claims(nil), "none", ""),
		"alg HS256":            p.token(p.claims(nil), "HS256", ""),
		"signed by a stranger": p.tokenWith(other, p.claims(nil), "", ""),
		"unknown key id":       p.token(p.claims(nil), "", "ghost"),
		"not a jwt":            "hello",
		"truncated":            strings.Join(strings.Split(p.token(p.claims(nil), "", ""), ".")[:2], "."),
	}
	for name, tok := range cases {
		if _, err := p.provider(t).Verify(context.Background(), tok, "n1", time.Now()); !errors.Is(err, ErrToken) {
			t.Errorf("%s: got %v, want ErrToken", name, err)
		}
	}
}

func TestAudienceMayBeAList(t *testing.T) {
	p := newIDP(t)
	tok := p.token(p.claims(map[string]any{"aud": []string{"other", "gocart-web"}}), "", "")
	if _, err := p.provider(t).Verify(context.Background(), tok, "n1", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedPayloadFailsTheSignature(t *testing.T) {
	p := newIDP(t)
	parts := strings.Split(p.token(p.claims(nil), "", ""), ".")
	forged, _ := json.Marshal(p.claims(map[string]any{"sub": "admin"}))
	tok := parts[0] + "." + b64.EncodeToString(forged) + "." + parts[2]
	if _, err := p.provider(t).Verify(context.Background(), tok, "n1", time.Now()); !errors.Is(err, ErrToken) {
		t.Fatalf("got %v", err)
	}
}

func TestAnExplicitlyUnverifiedEmailIsReported(t *testing.T) {
	p := newIDP(t)
	c, err := p.provider(t).Verify(context.Background(), p.token(p.claims(map[string]any{"email_verified": false}), "", ""), "n1", time.Now())
	if err != nil || c.EmailVerified {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestKeysAreFetchedOnceAndRotationIsFollowed(t *testing.T) {
	p := newIDP(t)
	pr := p.provider(t)
	for i := 0; i < 3; i++ {
		if _, err := pr.Verify(context.Background(), p.token(p.claims(nil), "", ""), "n1", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if n := p.jwksCalls.Load(); n != 1 {
		t.Fatalf("key set fetched %d times for 3 tokens", n)
	}
	// A token with an unknown key id inside the refetch window must not cause another fetch.
	pr.Verify(context.Background(), p.token(p.claims(nil), "", "ghost"), "n1", time.Now())
	if n := p.jwksCalls.Load(); n != 1 {
		t.Fatalf("an unknown kid triggered a refetch inside the window (%d)", n)
	}
	// After the window, a rotated key is picked up.
	p.kid = "k2"
	pr.mu.Lock()
	pr.keysFetched = time.Now().Add(-time.Minute)
	pr.mu.Unlock()
	if _, err := pr.Verify(context.Background(), p.token(p.claims(nil), "", ""), "n1", time.Now()); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
}

func TestAuthURLCarriesPKCEAndState(t *testing.T) {
	p := newIDP(t)
	u, err := p.provider(t).AuthURL(context.Background(), "st", "no", "chal")
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	q := pu.Query()
	if !strings.HasSuffix(pu.Path, "/dex/auth") || q.Get("response_type") != "code" || q.Get("client_id") != "gocart-web" ||
		q.Get("state") != "st" || q.Get("nonce") != "no" || q.Get("code_challenge") != "chal" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "http://localhost:3000/auth/callback" || !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("bad authorization URL: %s", u)
	}
}

func TestExchangeSendsTheVerifierAndReturnsTheToken(t *testing.T) {
	p := newIDP(t)
	p.issue = func() string { return "the-id-token" }
	got, err := p.provider(t).Exchange(context.Background(), "code-1", "verifier-1")
	if err != nil || got != "the-id-token" {
		t.Fatalf("%q %v", got, err)
	}
	f := p.tokenForm
	if f.Get("grant_type") != "authorization_code" || f.Get("code") != "code-1" || f.Get("code_verifier") != "verifier-1" ||
		f.Get("client_id") != "gocart-web" || f.Get("client_secret") != "s3cret" || f.Get("redirect_uri") != "http://localhost:3000/auth/callback" {
		t.Fatalf("token request = %v", f)
	}
}

func TestExchangeFailures(t *testing.T) {
	p := newIDP(t)
	p.issue = func() string { return "x" }
	pr := p.provider(t)
	p.status = 400
	if _, err := pr.Exchange(context.Background(), "c", "v"); !errors.Is(err, ErrToken) {
		t.Errorf("400: %v", err)
	}
	p.status = 503
	if _, err := pr.Exchange(context.Background(), "c", "v"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("503: %v", err)
	}
}

func TestProviderDownIsUnavailableAndRecovers(t *testing.T) {
	p := newIDP(t)
	pr := p.provider(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	bad, _ := New(Config{Issuer: dead.URL + "/dex", ClientID: "c", ClientSecret: "s", RedirectURL: "http://x/cb"}, nil)
	if _, err := bad.AuthURL(context.Background(), "a", "b", "c"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := pr.AuthURL(context.Background(), "a", "b", "c"); err != nil {
		t.Fatalf("a working provider: %v", err)
	}
}

func TestDiscoveryMustNameTheConfiguredIssuer(t *testing.T) {
	p := newIDP(t)
	pr := p.provider(t, func(c *Config) { c.Issuer = p.srv.URL + "/dex/" }) // trailing slash is tolerated
	if _, err := pr.AuthURL(context.Background(), "a", "b", "c"); err != nil {
		t.Fatalf("trailing slash: %v", err)
	}
	// the provider says its issuer is .../dex, but we are told something else
	mismatch := p.provider(t, func(c *Config) { c.BackchannelURL = p.issuer(); c.Issuer = "http://elsewhere.example/dex" })
	if _, err := mismatch.AuthURL(context.Background(), "a", "b", "c"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("issuer mismatch: %v", err)
	}
}

func TestBackchannelRewritesOnlyTheHost(t *testing.T) {
	p := newIDP(t)
	pr := p.provider(t, func(c *Config) {
		c.Issuer = "http://localhost:5556/dex"
		c.BackchannelURL = p.issuer()
	})
	// the provider (reached over the back channel) names itself localhost:5556, the browser address
	p.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dex/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{
				"issuer": "http://localhost:5556/dex", "authorization_endpoint": "http://localhost:5556/dex/auth",
				"token_endpoint": "http://localhost:5556/dex/token", "jwks_uri": "http://localhost:5556/dex/keys",
			})
		case "/dex/token":
			r.ParseForm()
			p.tokenForm = r.PostForm
			json.NewEncoder(w).Encode(map[string]string{"id_token": "tok"})
		default:
			t.Errorf("unexpected back-channel request %s", r.URL.Path)
		}
	})
	u, err := pr.AuthURL(context.Background(), "s", "n", "c")
	if err != nil || !strings.HasPrefix(u, "http://localhost:5556/dex/auth?") {
		t.Fatalf("the browser must be sent to the public address: %q %v", u, err)
	}
	if tok, err := pr.Exchange(context.Background(), "c", "v"); err != nil || tok != "tok" {
		t.Fatalf("exchange over the back channel: %q %v", tok, err)
	}
}

func TestConfigIsChecked(t *testing.T) {
	for name, c := range map[string]Config{
		"no issuer":    {ClientID: "a", ClientSecret: "b", RedirectURL: "http://x/cb"},
		"no secret":    {Issuer: "http://x", ClientID: "a", RedirectURL: "http://x/cb"},
		"bad scheme":   {Issuer: "ftp://x", ClientID: "a", ClientSecret: "b", RedirectURL: "http://x/cb"},
		"bad redirect": {Issuer: "http://x", ClientID: "a", ClientSecret: "b", RedirectURL: "/cb"},
	} {
		if _, err := New(c, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPKCEChallengeIsTheHashOfTheVerifier(t *testing.T) {
	v, c := PKCE()
	sum := sha256.Sum256([]byte(v))
	if c != b64.EncodeToString(sum[:]) || len(v) < 43 {
		t.Fatalf("verifier %q challenge %q", v, c)
	}
	if v2, _ := PKCE(); v2 == v {
		t.Fatal("verifier repeated")
	}
}
