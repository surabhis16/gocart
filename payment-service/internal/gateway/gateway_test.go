package gateway

import (
	"testing"
	"time"
)

func TestTokenBehaviour(t *testing.T) {
	g := New(0)
	cases := []struct {
		token string
		want  Outcome
	}{
		{"tok_visa", Outcome{Approved: true}},
		{"tok_anything_else", Outcome{Approved: true}},
		{"tok_declined", Outcome{DeclineCode: DeclineCard}},
		{"tok_insufficient_funds", Outcome{DeclineCode: DeclineInsufficientFunds}},
		{"tok_expired_card", Outcome{DeclineCode: DeclineExpiredCard}},
		{"tok_down", Outcome{Unavailable: true}},
	}
	for _, tc := range cases {
		if got := g.Authorize("o", tc.token); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.token, got, tc.want)
		}
	}
}

func TestFlakyFailsOnlyTheFirstAttemptPerOrder(t *testing.T) {
	g := New(0)
	if !g.Authorize("a", "tok_flaky").Unavailable {
		t.Error("first attempt for a should be unavailable")
	}
	if !g.Authorize("a", "tok_flaky").Approved {
		t.Error("second attempt for a should be approved")
	}
	if !g.Authorize("b", "tok_flaky").Unavailable {
		t.Error("the first attempt for another order should fail again")
	}
}

func TestSlowTakesTimeThenApproves(t *testing.T) {
	g := New(30 * time.Millisecond)
	start := time.Now()
	out := g.Authorize("o", "tok_slow")
	if !out.Approved || time.Since(start) < 30*time.Millisecond {
		t.Errorf("out=%+v after %v", out, time.Since(start))
	}
}

func TestValidToken(t *testing.T) {
	for s, want := range map[string]bool{
		"tok_visa": true, "tok_a1_b2": true,
		"": false, "tok_": false, "visa": false, "tok_Visa": false,
		"4242424242424242": false, "tok_has space": false,
		"tok_" + string(make([]byte, 41)): false,
	} {
		if got := ValidToken(s); got != want {
			t.Errorf("ValidToken(%q) = %v, want %v", s, got, want)
		}
	}
}
