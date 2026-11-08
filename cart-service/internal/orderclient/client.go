// Package orderclient is how the Cart service places an order.
//
// There is no retry loop here, on purpose. Placing an order is safe to repeat
// only because every attempt carries the same Idempotency-Key, and the caller
// of checkout is the one who knows whether to try again. A second hidden retry
// inside this client would only make the customer wait longer for the same
// answer.
package orderclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gocart/cart-service/internal/cart"
	"gocart/cart-service/internal/identity"
	"gocart/cart-service/internal/reqid"
)

// ErrUnavailable means the Order service gave no definite answer: the order
// may or may not exist. Repeating the same checkout is safe.
var ErrUnavailable = errors.New("order service unavailable")

// Response is the Order service's answer, passed through untouched.
type Response struct {
	Status int
	Body   []byte
}

// Accepted reports whether an order now exists for the request: created,
// replayed, or accepted and still being settled.
func (r Response) Accepted() bool {
	return r.Status == http.StatusCreated || r.Status == http.StatusOK || r.Status == http.StatusAccepted
}

// Definite reports a refusal that retrying cannot change.
func (r Response) Definite() bool {
	return r.Status >= 400 && r.Status < 500 && r.Status != http.StatusRequestTimeout && r.Status != http.StatusTooManyRequests
}

// OrderID reads the order's id from an accepted response.
func (r Response) OrderID() string {
	var o struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Body, &o)
	return o.ID
}

// Client talks to the Order service.
type Client struct {
	base string
	hc   *http.Client
}

// New returns a client for the Order service at baseURL. Placing an order
// waits on the Product and Payment services, so the default timeout is long.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// Place sends the cart's lines as an order, with the given Idempotency-Key.
func (c *Client) Place(ctx context.Context, key, customer, token string, lines []cart.Line) (Response, error) {
	items := make([]map[string]any, len(lines))
	for i, l := range lines {
		items[i] = map[string]any{"product_id": l.ProductID, "quantity": l.Quantity}
	}
	body, err := json.Marshal(map[string]any{"customer_id": customer, "payment_token": token, "items": items})
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/orders", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Idempotency-Key", key)
	// Pass the shopper's identity on, so the Order service applies the same
	// ownership rules to a checkout as to any other order request.
	if tok := identity.Token(ctx); tok != "" {
		req.Header.Set(identity.Header, tok)
	}
	if id := reqid.From(ctx); id != "" {
		req.Header.Set(reqid.Header, id)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	out := Response{Status: resp.StatusCode, Body: b}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return Response{}, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	if out.Accepted() && out.OrderID() == "" {
		return Response{}, fmt.Errorf("%w: success without an order id", ErrUnavailable)
	}
	return out, nil
}
