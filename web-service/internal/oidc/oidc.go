// Package oidc is the small part of OpenID Connect a web app needs in order to
// let a person sign in with an identity provider: the authorization code flow
// with PKCE, and verification of the ID token that comes back.
//
// It is written against the specification using only the standard library, and
// does exactly four things: discover the provider's endpoints, build the
// address to send the browser to, swap the code for an ID token, and check
// that token (RS256 signature, issuer, audience, expiry, nonce).
//
// What it deliberately does not do: refresh tokens, userinfo, logout at the
// provider, other signing algorithms. A sign-in here ends in a session cookie
// the gateway issues itself.
package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config describes the provider and this application's registration with it.
type Config struct {
	// Issuer is the provider's address exactly as it appears in the tokens
	// it signs, and as the browser reaches it.
	Issuer string
	// BackchannelURL, if set, is where this server reaches the provider
	// instead (for example a container name that the browser cannot
	// resolve). It may differ from Issuer only in scheme, host and port.
	BackchannelURL string
	ClientID       string
	ClientSecret   string
	RedirectURL    string
}

// Claims are the facts about the person that the gateway uses.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Errors the gateway tells apart.
var (
	ErrUnavailable = errors.New("oidc: provider not reachable")
	ErrToken       = errors.New("oidc: ID token rejected")
)

type metadata struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
}

// Provider talks to one identity provider. It makes no network call until it
// is first used, so the gateway can start before the provider is up.
type Provider struct {
	cfg Config
	hc  *http.Client

	mu          sync.Mutex
	meta        *metadata
	keys        map[string]*rsa.PublicKey
	keysFetched time.Time
}

// New checks cfg and returns a Provider.
func New(cfg Config, hc *http.Client) (*Provider, error) {
	for name, v := range map[string]string{"issuer": cfg.Issuer, "client id": cfg.ClientID, "client secret": cfg.ClientSecret, "redirect URL": cfg.RedirectURL} {
		if v == "" {
			return nil, fmt.Errorf("oidc: %s is required", name)
		}
	}
	for _, v := range []string{cfg.Issuer, cfg.BackchannelURL, cfg.RedirectURL} {
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("oidc: %q is not an http(s) URL", v)
		}
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	cfg.BackchannelURL = strings.TrimRight(cfg.BackchannelURL, "/")
	return &Provider{cfg: cfg, hc: hc}, nil
}

// Issuer is the provider's identifier, which also scopes subjects: the same
// subject at a different issuer is a different person.
func (p *Provider) Issuer() string { return p.cfg.Issuer }

// back turns an address the browser would use into the one this server uses.
func (p *Provider) back(raw string) string {
	if p.cfg.BackchannelURL == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	iss, _ := url.Parse(p.cfg.Issuer)
	bc, _ := url.Parse(p.cfg.BackchannelURL)
	if u.Host == iss.Host {
		u.Scheme, u.Host = bc.Scheme, bc.Host
	}
	return u.String()
}

func (p *Provider) getJSON(ctx context.Context, rawURL string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s answered %d", ErrUnavailable, rawURL, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func (p *Provider) discover(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meta != nil {
		return p.meta, nil
	}
	base := p.cfg.Issuer
	if p.cfg.BackchannelURL != "" {
		base = p.cfg.BackchannelURL
	}
	var m metadata
	if err := p.getJSON(ctx, base+"/.well-known/openid-configuration", &m); err != nil {
		return nil, err
	}
	if strings.TrimRight(m.Issuer, "/") != p.cfg.Issuer {
		return nil, fmt.Errorf("%w: provider says it is %q, configured as %q", ErrUnavailable, m.Issuer, p.cfg.Issuer)
	}
	if m.AuthEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return nil, fmt.Errorf("%w: discovery document is incomplete", ErrUnavailable)
	}
	p.meta = &m
	return p.meta, nil
}

// AuthURL is where to send the browser to sign in.
func (p *Provider) AuthURL(ctx context.Context, state, nonce, challenge string) (string, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(m.AuthEndpoint)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", p.cfg.RedirectURL)
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange swaps the one-time code for the provider's ID token.
func (p *Provider) Exchange(ctx context.Context, code, verifier string) (string, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.back(m.TokenEndpoint), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: token endpoint answered %d", ErrUnavailable, resp.StatusCode)
	}
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return "", fmt.Errorf("%w: token endpoint answered %d %s", ErrToken, resp.StatusCode, out.Error)
	}
	return out.IDToken, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// key finds the signing key for kid, fetching the key set when it has not been
// seen (providers rotate keys). A refetch is allowed at most every 10 seconds,
// so a stream of tokens with made-up key ids cannot turn this into a way to
// hammer the provider.
func (p *Provider) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[kid]; ok {
		return k, nil
	}
	if p.keys != nil && time.Since(p.keysFetched) < 10*time.Second {
		return nil, fmt.Errorf("%w: unknown signing key", ErrToken)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := p.getJSON(ctx, p.back(m.JWKSURI), &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := int(new(big.Int).SetBytes(e).Int64())
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	p.keys, p.keysFetched = keys, time.Now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown signing key", ErrToken)
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Verify checks an ID token and returns who it says the person is.
func (p *Provider) Verify(ctx context.Context, raw, nonce string, now time.Time) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: not a JWT", ErrToken)
	}
	dec := func(s string, v any) error {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	var head struct{ Alg, Kid string }
	if err := dec(parts[0], &head); err != nil {
		return Claims{}, fmt.Errorf("%w: bad header", ErrToken)
	}
	// Only RS256 is accepted. The header's alg is never allowed to choose how
	// the token is checked: that is how "alg": "none" and HMAC-with-the-public-
	// key attacks work.
	if head.Alg != "RS256" {
		return Claims{}, fmt.Errorf("%w: algorithm %q is not accepted", ErrToken, head.Alg)
	}
	key, err := p.key(ctx, head.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: bad signature encoding", ErrToken)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, fmt.Errorf("%w: bad signature", ErrToken)
	}

	var c struct {
		Iss           string   `json:"iss"`
		Sub           string   `json:"sub"`
		Aud           audience `json:"aud"`
		Exp           float64  `json:"exp"`
		Nonce         string   `json:"nonce"`
		Email         string   `json:"email"`
		EmailVerified *bool    `json:"email_verified"`
		Name          string   `json:"name"`
	}
	if err := dec(parts[1], &c); err != nil {
		return Claims{}, fmt.Errorf("%w: bad payload", ErrToken)
	}
	switch {
	case strings.TrimRight(c.Iss, "/") != p.cfg.Issuer:
		return Claims{}, fmt.Errorf("%w: issued by %q", ErrToken, c.Iss)
	case !contains(c.Aud, p.cfg.ClientID):
		return Claims{}, fmt.Errorf("%w: not issued for this application", ErrToken)
	case c.Exp == 0 || now.Unix() >= int64(c.Exp):
		return Claims{}, fmt.Errorf("%w: expired", ErrToken)
	case nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1:
		return Claims{}, fmt.Errorf("%w: nonce does not match", ErrToken)
	case c.Sub == "":
		return Claims{}, fmt.Errorf("%w: no subject", ErrToken)
	}
	return Claims{
		Subject: c.Sub, Email: c.Email, Name: c.Name,
		// Absent means the provider makes no claim; only an explicit false is a refusal.
		EmailVerified: c.EmailVerified == nil || *c.EmailVerified,
	}, nil
}

func contains(a audience, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// Random returns n random bytes, base64url encoded.
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source is gone; nothing safe can continue
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// PKCE makes a verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	verifier = Random(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}
