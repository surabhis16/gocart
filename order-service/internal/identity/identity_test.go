package identity

import (
	"context"
	"strings"
	"testing"
	"time"
)

var (
	secret = []byte("0123456789abcdef0123456789abcdef")
	now    = time.Unix(1_700_000_000, 0)
)

func TestRoundTrip(t *testing.T) {
	tok := Sign(secret, Claims{Sub: "cust_1", Role: RoleAdmin, Exp: now.Add(30 * time.Second).Unix()})
	c, err := Verify(secret, tok, now)
	if err != nil || c.Sub != "cust_1" || !c.Admin() {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestRejects(t *testing.T) {
	good := Sign(secret, Claims{Sub: "cust_1", Exp: now.Add(time.Minute).Unix()})
	parts := strings.Split(good, ".")
	forged := Sign(secret, Claims{Sub: "cust_2", Exp: now.Add(time.Minute).Unix()})
	fp := strings.Split(forged, ".")
	cases := map[string]struct {
		token string
		key   []byte
		at    time.Time
		want  error
	}{
		"expired":           {good, secret, now.Add(time.Minute), ErrExpired},
		"wrong secret":      {good, []byte("another-secret-another-secret-0000"), now, ErrSignature},
		"swapped payload":   {parts[0] + "." + fp[1] + "." + parts[2], secret, now, ErrSignature},
		"empty":             {"", secret, now, ErrMalformed},
		"two parts":         {parts[0] + "." + parts[1], secret, now, ErrMalformed},
		"unknown version":   {"v2." + parts[1] + "." + parts[2], secret, now, ErrMalformed},
		"bad base64":        {parts[0] + "." + parts[1] + ".!!", secret, now, ErrMalformed},
		"no subject":        {Sign(secret, Claims{Exp: now.Add(time.Minute).Unix()}), secret, now, ErrMalformed},
		"unsigned (no mac)": {parts[0] + "." + parts[1] + ".", secret, now, ErrSignature},
	}
	for name, tc := range cases {
		if _, err := Verify(tc.key, tc.token, tc.at); err != tc.want {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestContext(t *testing.T) {
	if _, ok := From(context.Background()); ok {
		t.Fatal("empty context has a caller")
	}
	ctx := With(context.Background(), Claims{Sub: "c"}, "tok")
	if c, ok := From(ctx); !ok || c.Sub != "c" || Token(ctx) != "tok" {
		t.Fatal("caller not carried")
	}
}
