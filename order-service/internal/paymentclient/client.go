// Package paymentclient is the Order service's only window onto the Payment
// service, the way productclient is for the Product service.
//
// The thing this client exists to get right is the difference between three
// answers that all look like "the charge did not go through":
//
//   - approved: carry on.
//   - declined (HTTP 402): a definite no. Retrying will not change it, so the
//     order must stop and give its stock back.
//   - unavailable: the provider or the network did not answer, so nothing is
//     known. The charge may even have succeeded. Retrying is safe, because the
//     Payment service identifies a charge by its order and never charges twice,
//     and giving up is not: the order stays as it is until a retry gets an answer.
//
// Treating the third as the second would cancel orders whose money was taken.
// Treating it as the first would ship goods nobody paid for.
package paymentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gocart/order-service/internal/reqid"
)

var (
	// ErrUnavailable means no definite answer was obtained. Try again later.
	ErrUnavailable = errors.New("payment service unavailable")
	// ErrVoided means the order's charge was cancelled and can no longer be taken.
	ErrVoided = errors.New("charge for this order was cancelled")
)

// DeclinedError is a definite refusal.
type DeclinedError struct{ Code string }

func (e *DeclinedError) Error() string { return "payment declined: " + e.Code }

// Charge is an approved charge.
type Charge struct {
	OrderID   string
	Reference string
}

// Client talks to the Payment service.
type Client struct {
	base     string
	hc       *http.Client
	attempts int
	backoff  time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client (3 second timeout per try).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithAttempts sets the total number of tries per call (default 2).
func WithAttempts(n int) Option {
	return func(c *Client) {
		if n >= 1 {
			c.attempts = n
		}
	}
}

// WithBackoff sets the pause before the first retry; it doubles each time.
func WithBackoff(d time.Duration) Option { return func(c *Client) { c.backoff = d } }

// New returns a client for the Payment service at baseURL.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		base:     strings.TrimRight(baseURL, "/"),
		hc:       &http.Client{Timeout: 3 * time.Second},
		attempts: 2,
		backoff:  100 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Charge asks the Payment service to take payment for an order. Asking again
// for the same order returns the same answer.
func (c *Client) Charge(ctx context.Context, orderID string, amountMinor int64, currency, token string) (Charge, error) {
	body, err := json.Marshal(map[string]any{
		"order_id": orderID, "amount_minor": amountMinor, "currency": currency, "payment_token": token,
	})
	if err != nil {
		return Charge{}, err
	}
	status, resp, err := c.roundTrip(ctx, http.MethodPost, "/charges", body)
	if err != nil {
		return Charge{}, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		var r struct {
			OrderID   string `json:"order_id"`
			Status    string `json:"status"`
			Reference string `json:"reference"`
		}
		if err := json.Unmarshal(resp, &r); err != nil || r.Status != "approved" || r.Reference == "" {
			// A success we cannot read is not one we can mark an order paid on.
			return Charge{}, fmt.Errorf("%w: unreadable charge response", ErrUnavailable)
		}
		return Charge{OrderID: r.OrderID, Reference: r.Reference}, nil
	case status == http.StatusPaymentRequired:
		var p struct {
			DeclineCode string `json:"decline_code"`
		}
		_ = json.Unmarshal(resp, &p)
		if p.DeclineCode == "" {
			p.DeclineCode = "card_declined"
		}
		return Charge{}, &DeclinedError{Code: p.DeclineCode}
	case status == http.StatusConflict && problemType(resp) == "charge-voided":
		return Charge{}, ErrVoided
	default:
		// Anything else (a 422 for a request we built wrongly, a 409 mismatch)
		// is a definite refusal of this request but not a decline of the card.
		return Charge{}, fmt.Errorf("payment service refused the charge: status %d: %s", status, problemType(resp))
	}
}

// Cancel asks the Payment service to void an order's charge, refunding it if it
// was taken and blocking it if it has not arrived yet. Cancelling twice, or a
// charge that never existed, is a success.
func (c *Client) Cancel(ctx context.Context, orderID string) error {
	status, resp, err := c.roundTrip(ctx, http.MethodDelete, "/charges/"+url.PathEscape(orderID), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent || status == http.StatusOK {
		return nil
	}
	return fmt.Errorf("payment service refused to cancel %s: status %d: %s", orderID, status, problemType(resp))
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// roundTrip sends a request, retrying only when the outcome is unknown. It
// returns a definite status and body, or ErrUnavailable.
func (c *Client) roundTrip(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var last error
	delay := c.backoff
	for attempt := 1; attempt <= c.attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
			}
			delay *= 2
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if id := reqid.From(ctx); id != "" {
			req.Header.Set(reqid.Header, id)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			last = readErr
			continue
		}
		if retryable(resp.StatusCode) {
			last = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		return resp.StatusCode, b, nil
	}
	return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, last)
}

// problemType extracts the short problem type from a problem document.
func problemType(body []byte) string {
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &p)
	return strings.TrimPrefix(p.Type, "urn:gocart:problem:")
}
