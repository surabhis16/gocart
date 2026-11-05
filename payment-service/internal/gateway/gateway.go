// Package gateway is a stand-in for a real card network or payment provider.
//
// It does no real payments. What it does is behave in the ways real providers
// do, on demand, so the rest of the system can be shown coping with them. The
// behaviour is chosen by the payment token, which is how real providers make
// test cards work too:
//
//	tok_visa (or any other tok_...)  approved
//	tok_declined                     declined: card_declined
//	tok_insufficient_funds           declined: insufficient_funds
//	tok_expired_card                 declined: expired_card
//	tok_flaky                        first attempt for an order: provider unavailable; then approved
//	tok_down                         provider unavailable, every time
//	tok_slow                         approved, but only after a delay
//
// "Declined" is a definite answer: retrying will not change it. "Unavailable"
// means the provider could not answer, and nothing is known about the outcome.
// Telling those apart is the whole difficulty of taking payments over a network.
package gateway

import (
	"regexp"
	"sync"
	"time"
)

// Decline codes returned with a declined outcome.
const (
	DeclineCard              = "card_declined"
	DeclineInsufficientFunds = "insufficient_funds"
	DeclineExpiredCard       = "expired_card"
)

// Outcome is what the provider said.
type Outcome struct {
	Approved    bool
	DeclineCode string // set when declined
	Unavailable bool   // the provider could not answer; the outcome is unknown
}

// tokenPattern is deliberately narrow. A token is an opaque reference to a
// payment method held by the provider. Requiring the "tok_" shape means a card
// number pasted into the wrong field is refused instead of being stored.
var tokenPattern = regexp.MustCompile(`^tok_[a-z0-9_]{1,40}$`)

// ValidToken reports whether s looks like a payment token.
func ValidToken(s string) bool { return tokenPattern.MatchString(s) }

// Gateway is the fake provider.
type Gateway struct {
	slowDelay time.Duration

	mu       sync.Mutex
	attempts map[string]int // per order, for tok_flaky
}

// New returns a Gateway whose tok_slow approvals take slowDelay.
func New(slowDelay time.Duration) *Gateway {
	return &Gateway{slowDelay: slowDelay, attempts: map[string]int{}}
}

// Authorize asks the provider to approve a charge for an order.
func (g *Gateway) Authorize(orderID, token string) Outcome {
	switch token {
	case "tok_declined":
		return Outcome{DeclineCode: DeclineCard}
	case "tok_insufficient_funds":
		return Outcome{DeclineCode: DeclineInsufficientFunds}
	case "tok_expired_card":
		return Outcome{DeclineCode: DeclineExpiredCard}
	case "tok_down":
		return Outcome{Unavailable: true}
	case "tok_flaky":
		g.mu.Lock()
		g.attempts[orderID]++
		first := g.attempts[orderID] == 1
		g.mu.Unlock()
		if first {
			return Outcome{Unavailable: true}
		}
		return Outcome{Approved: true}
	case "tok_slow":
		// Deliberately ignores whether the caller is still waiting: a real
		// provider carries on processing after a client gives up, which is
		// exactly how a charge can succeed while the caller never hears about it.
		time.Sleep(g.slowDelay)
		return Outcome{Approved: true}
	default:
		return Outcome{Approved: true}
	}
}
