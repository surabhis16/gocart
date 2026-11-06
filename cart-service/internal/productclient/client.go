// Package productclient is the Cart service's window onto the Product service.
// It asks one question: does this product exist, and what is it called?
// Prices and stock are deliberately not read; see the cart package.
package productclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gocart/cart-service/internal/reqid"
)

var (
	// ErrUnknown is a definite answer: there is no such product.
	ErrUnknown = errors.New("unknown product")
	// ErrUnavailable means the Product service did not give a usable answer.
	ErrUnavailable = errors.New("product service unavailable")
)

// Client talks to the Product service.
type Client struct {
	base string
	hc   *http.Client
}

// New returns a client for the Product service at baseURL. A lookup is
// harmless to repeat, but the customer is waiting, so there is one try only.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// Name returns the product's name, or ErrUnknown / ErrUnavailable.
func (c *Client) Name(ctx context.Context, productID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/products/"+url.PathEscape(productID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if id := reqid.From(ctx); id != "" {
		req.Header.Set(reqid.Header, id)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrUnknown
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	// Tolerant reader: unknown fields are ignored, so the Product service can
	// add fields without a coordinated release.
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Name == "" {
		return "", fmt.Errorf("%w: unreadable product", ErrUnavailable)
	}
	return p.Name, nil
}
