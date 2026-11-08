// Package session seals small values into cookies the browser cannot read or
// change. A sealed value is "<payload>.<signature>"; the signature covers a
// purpose label as well as the payload, so a cookie made for one use (the
// sign-in handshake) is worthless for another (a session).
//
// The cookie is signed, not encrypted: it carries nothing the person does not
// already know, and the server keeps no session table, so any replica can
// serve any request.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// ErrInvalid is returned for any cookie that is not exactly what Seal made.
var ErrInvalid = errors.New("session: invalid cookie")

var enc = base64.RawURLEncoding

func mac(secret []byte, purpose, payload string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// Seal encodes v for the given purpose.
func Seal(secret []byte, purpose string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	p := enc.EncodeToString(b)
	return p + "." + enc.EncodeToString(mac(secret, purpose, p)), nil
}

// Open verifies s and decodes it into v. Expiry is the caller's business: the
// sealed value carries its own.
func Open(secret []byte, purpose, s string, v any) error {
	payload, sig, ok := strings.Cut(s, ".")
	if !ok {
		return ErrInvalid
	}
	got, err := enc.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(secret, purpose, payload)) {
		return ErrInvalid
	}
	b, err := enc.DecodeString(payload)
	if err != nil || json.Unmarshal(b, v) != nil {
		return ErrInvalid
	}
	return nil
}
