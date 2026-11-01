// Package productclient is the Order service's only window onto the Product
// service. Everything the Order service assumes about that other service is
// written down here.
//
// Two decisions matter:
//
//   - Retries are safe because the Product service's reserve and release are
//     idempotent per order ID. The client retries only when it cannot know
//     whether a request landed (network error, 5xx, 429), never on a definite
//     answer such as "insufficient stock".
//   - Responses are read tolerantly: unknown fields are ignored. The Product
//     service may add fields without coordinating a release with us. The
//     reverse is not true: the Product service reads our requests strictly.
package productclient

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

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/reqid"
)

// ErrUnavailable means the Product service could not give a definite answer.
// The outcome of the request is unknown, and trying again later is safe.
var ErrUnavailable = errors.New("product service unavailable")

// Rejection reasons recorded on rejected orders.
const (
	ReasonInsufficientStock = "insufficient_stock"
	ReasonUnknownProduct    = "unknown_product"
	ReasonInvalidItems      = "invalid_items"
	ReasonRefused           = "refused"
)

// RejectedError is a definite "no" from the Product service. Retrying the same
// request will not change the answer.
type RejectedError struct {
	Reason  string
	Message string
}

func (e *RejectedError) Error() string {
	return "product service rejected the reservation: " + e.Reason
}

// Reservation is what the Product service holds for an order.
type Reservation struct {
	OrderID  string
	Currency string
	Items    []domain.PricedItem
}

// Client talks to the Product service.
type Client struct {
	base     string
	hc       *http.Client
	attempts int
	backoff  time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client (2 second timeout per try).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithAttempts sets the total number of tries per call (default 3).
func WithAttempts(n int) Option {
	return func(c *Client) {
		if n >= 1 {
			c.attempts = n
		}
	}
}

// WithBackoff sets the pause before the first retry; it doubles each time.
func WithBackoff(d time.Duration) Option { return func(c *Client) { c.backoff = d } }

// New returns a client for the Product service at baseURL.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		base:     strings.TrimRight(baseURL, "/"),
		hc:       &http.Client{Timeout: 2 * time.Second},
		attempts: 3,
		backoff:  100 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Reserve asks the Product service to hold stock for an order, all lines or none.
func (c *Client) Reserve(ctx context.Context, orderID string, items []domain.Item) (Reservation, error) {
	body, err := json.Marshal(map[string]any{"order_id": orderID, "items": items})
	if err != nil {
		return Reservation{}, err
	}
	status, resp, err := c.roundTrip(ctx, http.MethodPost, "/reservations", body)
	if err != nil {
		return Reservation{}, err
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		var r struct {
			OrderID  string `json:"order_id"`
			Currency string `json:"currency"`
			Items    []struct {
				ProductID      string `json:"product_id"`
				Name           string `json:"name"`
				Quantity       int    `json:"quantity"`
				UnitPriceMinor int64  `json:"unit_price_minor"`
			} `json:"items"`
		}
		if err := json.Unmarshal(resp, &r); err != nil || len(r.Items) == 0 || r.Currency == "" {
			// A success we cannot read is not a success we can build an order on.
			return Reservation{}, fmt.Errorf("%w: unreadable reservation response", ErrUnavailable)
		}
		out := Reservation{OrderID: r.OrderID, Currency: r.Currency, Items: make([]domain.PricedItem, len(r.Items))}
		for i, it := range r.Items {
			out.Items[i] = domain.PricedItem{
				ProductID: it.ProductID, Name: it.Name, Quantity: it.Quantity, UnitPriceMinor: it.UnitPriceMinor,
			}
		}
		return out, nil
	case status >= 400 && status < 500:
		return Reservation{}, rejection(resp)
	default:
		return Reservation{}, fmt.Errorf("%w: unexpected status %d", ErrUnavailable, status)
	}
}

// Release asks the Product service to put an order's stock back. Releasing
// stock that is not held is a success.
func (c *Client) Release(ctx context.Context, orderID string) error {
	status, _, err := c.roundTrip(ctx, http.MethodDelete, "/reservations/"+url.PathEscape(orderID), nil)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNoContent || status == http.StatusOK || status == http.StatusNotFound:
		return nil
	case status >= 400 && status < 500:
		return fmt.Errorf("product service refused to release %s: status %d", orderID, status)
	default:
		return fmt.Errorf("%w: unexpected status %d", ErrUnavailable, status)
	}
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

// rejection turns a Product service problem document into a RejectedError.
func rejection(body []byte) *RejectedError {
	var p struct {
		Type      string `json:"type"`
		Detail    string `json:"detail"`
		Shortages []struct {
			ProductID string `json:"product_id"`
			Requested int    `json:"requested"`
			Available int    `json:"available"`
		} `json:"shortages"`
		ProductIDs []string `json:"product_ids"`
	}
	_ = json.Unmarshal(body, &p) // a body we cannot read still leaves a usable rejection

	switch strings.TrimPrefix(p.Type, "urn:gocart:problem:") {
	case "insufficient-stock":
		parts := make([]string, len(p.Shortages))
		for i, s := range p.Shortages {
			parts[i] = fmt.Sprintf("%s: wanted %d, only %d available", s.ProductID, s.Requested, s.Available)
		}
		return &RejectedError{Reason: ReasonInsufficientStock, Message: strings.Join(parts, "; ")}
	case "unknown-product":
		return &RejectedError{Reason: ReasonUnknownProduct, Message: "unknown product(s): " + strings.Join(p.ProductIDs, ", ")}
	case "validation-failed":
		return &RejectedError{Reason: ReasonInvalidItems, Message: p.Detail}
	default:
		return &RejectedError{Reason: ReasonRefused, Message: p.Detail}
	}
}
