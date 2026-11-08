// Package identity is how the web gateway tells the other services who is
// calling. After a person signs in, the gateway attaches a short-lived signed
// assertion to every request it forwards; a service that verifies it can trust
// the customer id without trusting the network.
//
// The format is deliberately smaller than a JWT: "v1.<payload>.<signature>",
// with the payload base64url JSON and the signature HMAC-SHA256 over
// "v1.<payload>". One algorithm, no header to downgrade, and 60 lines to read.
// This file is copied into each service that uses it (the services share no
// code), and the copies have the same tests.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Header carries the assertion between the gateway and the services.
const Header = "X-Gocart-Identity"

// MinSecretLen is the shortest signing secret the services accept.
const MinSecretLen = 32

// Roles. Anything else is treated as no role.
const (
	RoleCustomer = "customer"
	RoleAdmin    = "admin"
)

// Claims say who is calling.
type Claims struct {
	Sub  string `json:"sub"`            // the customer id
	Role string `json:"role,omitempty"` // RoleCustomer or RoleAdmin
	Exp  int64  `json:"exp"`            // unix seconds
}

// Admin reports whether the caller signed in as an administrator.
func (c Claims) Admin() bool { return c.Role == RoleAdmin }

// Errors from Verify. Callers answer 401 for all of them and do not say which.
var (
	ErrMalformed = errors.New("identity: malformed assertion")
	ErrSignature = errors.New("identity: bad signature")
	ErrExpired   = errors.New("identity: assertion expired")
)

var enc = base64.RawURLEncoding

func mac(secret []byte, signed string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(signed))
	return h.Sum(nil)
}

// Sign makes an assertion for c.
func Sign(secret []byte, c Claims) string {
	body, _ := json.Marshal(c)
	signed := "v1." + enc.EncodeToString(body)
	return signed + "." + enc.EncodeToString(mac(secret, signed))
}

// Verify checks the signature and the expiry, in that order.
func Verify(secret []byte, token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return Claims{}, ErrMalformed
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if !hmac.Equal(sig, mac(secret, parts[0]+"."+parts[1])) {
		return Claims{}, ErrSignature
	}
	body, err := enc.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil || c.Sub == "" {
		return Claims{}, ErrMalformed
	}
	if now.Unix() >= c.Exp {
		return Claims{}, ErrExpired
	}
	return c, nil
}

type ctxKey struct{}

type carried struct {
	claims Claims
	token  string
}

// With stores a verified caller (and the assertion it came in as, so it can be
// passed on to the next service) in ctx.
func With(ctx context.Context, c Claims, token string) context.Context {
	return context.WithValue(ctx, ctxKey{}, carried{c, token})
}

// From returns the verified caller, if the request had one.
func From(ctx context.Context) (Claims, bool) {
	v, ok := ctx.Value(ctxKey{}).(carried)
	return v.claims, ok
}

// Token returns the assertion the request arrived with, for forwarding.
func Token(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(carried)
	return v.token
}
