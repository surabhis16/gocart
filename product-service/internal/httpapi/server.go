// Package httpapi is the HTTP surface of the Product service. The contract it
// implements lives in docs/openapi/product.yaml, written before this code, and
// a test fails if the two drift apart.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"gocart/product-service/internal/catalog"
)

const maxBodyBytes = 1 << 20

type route struct {
	pattern string
	handler http.HandlerFunc
}

// Server wires the catalogue to HTTP.
type Server struct {
	store    *catalog.Store
	log      *slog.Logger
	routes   []route
	metrics  *metrics
	adminKey string
}

// New builds a Server.
func New(store *catalog.Store, log *slog.Logger, opts ...Option) *Server {
	s := &Server{store: store, log: log, metrics: newMetrics("product-service")}
	for _, o := range opts {
		o(s)
	}
	s.routes = []route{
		{"GET /healthz", s.health},
		{"GET /metrics", s.serveMetrics},
		{"GET /products", s.listProducts},
		{"POST /products", s.admin(s.createProduct)},
		{"GET /products/{id}", s.getProduct},
		{"PUT /products/{id}/stock", s.admin(s.setStock)},
		{"POST /reservations", s.reserve},
		{"DELETE /reservations/{order_id}", s.release},
	}
	return s
}

func (s *Server) gauges() []Gauge {
	var products, outOfStock, units int
	for _, p := range s.store.List() {
		products++
		units += p.Stock
		if p.Stock == 0 {
			outOfStock++
		}
	}
	return []Gauge{
		{Name: "gocart_products", Help: "Products in the catalogue.", Value: float64(products)},
		{Name: "gocart_products_out_of_stock", Help: "Products with no stock left.", Value: float64(outOfStock)},
		{Name: "gocart_stock_units", Help: "Units in stock across all products.", Value: float64(units)},
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

func (s *Server) listProducts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.store.List()})
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) createProduct(w http.ResponseWriter, r *http.Request) {
	var in catalog.NewProduct
	if !decode(w, r, &in) {
		return
	}
	p, err := s.store.Create(in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/products/"+p.ID)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) setStock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Stock int `json:"stock"`
	}
	if !decode(w, r, &in) {
		return
	}
	p, err := s.store.SetStock(r.PathValue("id"), in.Stock)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID string         `json:"order_id"`
		Items   []catalog.Line `json:"items"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, replayed, err := s.store.Reserve(in.OrderID, in.Items)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK // same request seen before: nothing new was reserved
	}
	writeJSON(w, status, res)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.Release(r.PathValue("order_id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decode reads a strict JSON body: unknown fields and trailing data are errors,
// because silently ignoring a misspelt field is how "quantity" becomes 0.
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
		writeProblem(w, r, Problem{Type: "payload-too-large", Title: "Request body too large", Status: http.StatusRequestEntityTooLarge})
		return false
	}
	writeProblem(w, r, Problem{Type: "invalid-request", Title: "Malformed request body", Status: http.StatusBadRequest, Detail: err.Error()})
	return false
}

// fail maps domain errors onto problem responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var short *catalog.InsufficientStockError
	var unknown *catalog.UnknownProductsError
	switch {
	case errors.As(err, &short):
		writeProblem(w, r, Problem{
			Type: "insufficient-stock", Title: "Insufficient stock", Status: http.StatusConflict,
			Detail: "One or more products do not have enough stock. Nothing was reserved.",
			Extra:  map[string]any{"shortages": short.Shortages},
		})
	case errors.As(err, &unknown):
		writeProblem(w, r, Problem{
			Type: "unknown-product", Title: "Unknown product", Status: http.StatusUnprocessableEntity,
			Detail: err.Error(), Extra: map[string]any{"product_ids": unknown.IDs},
		})
	case errors.Is(err, catalog.ErrReservationMismatch):
		writeProblem(w, r, Problem{
			Type: "reservation-mismatch", Title: "Reservation already exists with different items",
			Status: http.StatusConflict, Detail: err.Error(),
		})
	case errors.Is(err, catalog.ErrNotFound):
		writeProblem(w, r, Problem{Type: "not-found", Title: "Not found", Status: http.StatusNotFound})
	case errors.Is(err, catalog.ErrInvalid):
		writeProblem(w, r, Problem{
			Type: "validation-failed", Title: "Validation failed",
			Status: http.StatusUnprocessableEntity, Detail: err.Error(),
		})
	default:
		s.log.Error("unhandled error", "err", err, "request_id", requestIDFrom(r.Context()))
		writeProblem(w, r, Problem{Type: "internal", Title: "Internal error", Status: http.StatusInternalServerError})
	}
}
