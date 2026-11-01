// Package reqid carries a correlation ID through a request and across service
// calls, so one customer action can be followed through both services' logs.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// Header is the HTTP header that carries the ID between services.
const Header = "X-Request-ID"

// Only IDs that are safe to put in logs and headers are accepted.
var valid = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type key struct{}

// Valid reports whether id is safe to adopt.
func Valid(id string) bool { return valid.MatchString(id) }

// New returns a fresh random ID.
func New() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return hex.EncodeToString(b[:])
}

// With returns ctx carrying id.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// From returns the ID in ctx, or "".
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
