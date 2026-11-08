package session

import (
	"strings"
	"testing"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

type val struct {
	Sub string `json:"sub"`
	N   int    `json:"n"`
}

func TestRoundTrip(t *testing.T) {
	s, err := Seal(secret, "session", val{"c1", 7})
	if err != nil {
		t.Fatal(err)
	}
	var got val
	if err := Open(secret, "session", s, &got); err != nil || got != (val{"c1", 7}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestRefuses(t *testing.T) {
	s, _ := Seal(secret, "session", val{"c1", 7})
	other, _ := Seal(secret, "session", val{"c2", 7})
	payload, sig, _ := strings.Cut(s, ".")
	otherPayload, _, _ := strings.Cut(other, ".")
	cases := map[string]struct {
		purpose, cookie string
		key             []byte
	}{
		"another purpose": {"login", s, secret},
		"another secret":  {"session", s, []byte("a-different-secret-a-different-one")},
		"swapped payload": {"session", otherPayload + "." + sig, secret},
		"no signature":    {"session", payload, secret},
		"empty":           {"session", "", secret},
		"not base64":      {"session", payload + ".!!", secret},
	}
	for name, c := range cases {
		var v val
		if err := Open(c.key, c.purpose, c.cookie, &v); err != ErrInvalid {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
