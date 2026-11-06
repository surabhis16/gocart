// Package httpapi is the HTTP surface of the Cart service. The contract it
// implements lives in docs/openapi/cart.yaml, and a test fails if the two
// drift apart.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"gocart/cart-service/internal/cart"
	"gocart/cart-service/internal/orderclient"
	"gocart/cart-service/internal/productclient"
	"gocart/cart-service/internal/reqid"
)

const maxBodyBytes = 1 << 20

// Catalog is what the Cart service needs from the Product service.
type Catalog interface {
	Name(ctx context.Context, productID string) (string, error)
}

// Orders is what the Cart service needs from the Order service.
type Orders interface {
	Place(ctx context.Context, key, customer, token string, lines []cart.Line) (orderclient.Response, error)
}

type route struct {
	pattern string
	handler http.HandlerFunc
}

// Server wires the cart store to HTTP.
type Server struct {
	carts   *cart.Store
	catalog Catalog
	orders  Orders
	log     *slog.Logger
	routes  []route
	metrics *metrics
}

// New builds a Server.
func New(carts *cart.Store, catalog Catalog, orders Orders, log *slog.Logger, opts ...Option) *Server {
	s := &Server{carts: carts, catalog: catalog, orders: orders, log: log, metrics: newMetrics("cart-service")}
	for _, o := range opts {
		o(s)
	}
	s.routes = []route{
		{"GET /healthz", s.health},
		{"GET /metrics", s.serveMetrics},
		{"GET /carts/{customer_id}", s.getCart},
		{"DELETE /carts/{customer_id}", s.clearCart},
		{"PUT /carts/{customer_id}/items/{product_id}", s.setItem},
		{"DELETE /carts/{customer_id}/items/{product_id}", s.removeItem},
		{"POST /carts/{customer_id}/checkout", s.checkout},
	}
	return s
}

func (s *Server) gauges() []Gauge {
	open, ordered := s.carts.Counts()
	help := "Carts, by state: open (has lines) or checked_out (became an order)."
	return []Gauge{
		{Name: "gocart_carts", Help: help, Labels: map[string]string{"state": "open"}, Value: float64(open)},
		{Name: "gocart_carts", Help: help, Labels: map[string]string{"state": "checked_out"}, Value: float64(ordered)},
	}
}

// Routes lists the "METHOD /path" patterns this server answers.
func (s *Server) Routes() []string {
	out := make([]string, len(s.routes))
	for i, rt := range s.routes {
		out[i] = rt.pattern
	}
	return out
}

// Handler returns the server wrapped in its middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes {
		mux.HandleFunc(rt.pattern, s.metrics.wrap(rt.pattern, rt.handler))
	}
	return requestID(accessLog(s.log, s.metrics.middleware(recoverer(s.log, mux))))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// customer reads and validates the customer id in the path.
func (s *Server) customer(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("customer_id")
	if !cart.ValidCustomerID(id) {
		writeProblem(w, r, Problem{
			Type: "validation-failed", Title: "Validation failed", Status: http.StatusUnprocessableEntity,
			Detail: "customer id must be 1-64 characters from A-Z a-z 0-9 . _ -",
		})
		return "", false
	}
	return id, true
}

func (s *Server) getCart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.customer(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.carts.Get(id))
}

func (s *Server) clearCart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.customer(w, r)
	if !ok {
		return
	}
	if _, err := s.carts.Clear(id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setItem is PUT: it states the quantity wanted rather than adding to it, so
// sending it twice leaves the cart the same as sending it once.
func (s *Server) setItem(w http.ResponseWriter, r *http.Request) {
	id, ok := s.customer(w, r)
	if !ok {
		return
	}
	product := r.PathValue("product_id")
	var in struct {
		Quantity int `json:"quantity"`
	}
	if !decode(w, r, &in) {
		return
	}
	// A product already in the cart is not looked up again, so quantities can
	// still be changed while the Product service is down. The first time a
	// product is added, it must exist.
	name := ""
	if !s.carts.Has(id, product) {
		var err error
		if name, err = s.catalog.Name(r.Context(), product); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	c, err := s.carts.SetItem(id, cart.Line{ProductID: product, Name: name, Quantity: in.Quantity})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) removeItem(w http.ResponseWriter, r *http.Request) {
	id, ok := s.customer(w, r)
	if !ok {
		return
	}
	c, err := s.carts.RemoveItem(id, r.PathValue("product_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// checkout turns the cart into an order. The answer is the Order service's
// own, passed through, so every status it can give (201 created, 200 replayed,
// 202 still settling, 402 declined, 409 rejected) reaches the caller intact.
//
// Retrying is safe at every point. The Idempotency-Key sent to the Order
// service is derived from the cart's revision, so:
//
//   - no answer (timeout, 5xx): the cart is untouched and the same key is sent
//     next time; if the order was made after all, the Order service returns it
//     instead of making another.
//   - answer received but lost on the way back: the cart is marked as ordered,
//     and a repeated checkout replays the same order.
//   - a definite refusal (declined, out of stock): the cart moves to a new
//     revision, because the old key would replay the refusal forever.
func (s *Server) checkout(w http.ResponseWriter, r *http.Request) {
	id, ok := s.customer(w, r)
	if !ok {
		return
	}
	var in struct {
		PaymentToken string `json:"payment_token"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.PaymentToken == "" {
		writeProblem(w, r, Problem{
			Type: "validation-failed", Title: "Validation failed", Status: http.StatusUnprocessableEntity,
			Detail: "payment_token is required, for example tok_visa",
		})
		return
	}
	c := s.carts.Get(id)
	if len(c.Items) == 0 {
		writeProblem(w, r, Problem{
			Type: "cart-empty", Title: "Cart is empty", Status: http.StatusConflict,
			Detail: "There is nothing to check out. Add an item first.",
		})
		return
	}

	resp, err := s.orders.Place(r.Context(), cart.CheckoutKey(id, c.Revision), id, in.PaymentToken, c.Items)
	switch {
	case errors.Is(err, orderclient.ErrUnavailable):
		s.log.Warn("order service gave no answer to checkout", "err", err, "customer", id, "request_id", reqid.From(r.Context()))
		w.Header().Set("Retry-After", "2")
		writeProblem(w, r, Problem{
			Type: "checkout-unavailable", Title: "Could not complete checkout", Status: http.StatusServiceUnavailable,
			Detail: "The Order service did not answer, so it is not known whether the order was placed. Your cart is unchanged. Retry the same request: it cannot create a second order.",
		})
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}

	switch {
	case resp.Accepted():
		if err := s.carts.MarkOrdered(id, c.Revision, resp.OrderID()); err != nil {
			// The order exists. Failing to note that is not worth failing the
			// customer's checkout over: a retry replays the same order.
			s.log.Error("could not record the order on the cart", "err", err, "order_id", resp.OrderID(), "request_id", reqid.From(r.Context()))
		}
	case resp.Definite():
		if err := s.carts.Renew(id, c.Revision); err != nil {
			s.log.Error("could not renew the cart after a refused checkout", "err", err, "request_id", reqid.From(r.Context()))
		}
	}
	w.Header().Set("Content-Type", contentType(resp))
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// contentType keeps the Order service's own media type, so a refusal stays
// application/problem+json.
func contentType(resp orderclient.Response) string {
	var probe struct {
		Type string `json:"type"`
	}
	if resp.Status >= 400 && json.Unmarshal(resp.Body, &probe) == nil && probe.Type != "" {
		return "application/problem+json"
	}
	return "application/json"
}

// decode reads a strict JSON body: unknown fields and trailing data are errors.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if _, extra := dec.Token(); !errors.Is(extra, io.EOF) {
			err = errors.New("request body must contain a single JSON object")
		}
	}
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeProblem(w, r, Problem{Type: "payload-too-large", Title: "Request body too large", Status: http.StatusRequestEntityTooLarge,
			Detail: "The body may be at most " + strconv.Itoa(maxBodyBytes) + " bytes."})
		return false
	}
	writeProblem(w, r, Problem{Type: "invalid-request", Title: "Malformed request body", Status: http.StatusBadRequest, Detail: err.Error()})
	return false
}

// fail maps errors onto problem responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, cart.ErrInvalid):
		writeProblem(w, r, Problem{Type: "validation-failed", Title: "Validation failed", Status: http.StatusUnprocessableEntity, Detail: err.Error()})
	case errors.Is(err, productclient.ErrUnknown):
		writeProblem(w, r, Problem{
			Type: "unknown-product", Title: "Unknown product", Status: http.StatusUnprocessableEntity,
			Detail: "The Product service has no such product, so it was not added.",
		})
	case errors.Is(err, productclient.ErrUnavailable):
		w.Header().Set("Retry-After", "2")
		writeProblem(w, r, Problem{
			Type: "product-service-unavailable", Title: "Could not check the product", Status: http.StatusServiceUnavailable,
			Detail: "The Product service did not answer, so the product could not be added. Nothing changed; retrying is safe.",
		})
	default:
		s.log.Error("unhandled error", "err", err, "request_id", reqid.From(r.Context()))
		writeProblem(w, r, Problem{Type: "internal", Title: "Internal error", Status: http.StatusInternalServerError})
	}
}
