// Package httpapi is the HTTP surface of the Payment service. The contract it
// implements lives in docs/openapi/payment.yaml, and a test fails if the two
// drift apart.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"gocart/payment-service/internal/ledger"
)

const maxBodyBytes = 1 << 20

type route struct {
	pattern string
	handler http.HandlerFunc
}

// Server wires the ledger to HTTP.
type Server struct {
	ledger   *ledger.Store
	log      *slog.Logger
	routes   []route
	metrics  *metrics
	adminKey string
}

// New builds a Server.
func New(l *ledger.Store, log *slog.Logger, opts ...Option) *Server {
	s := &Server{ledger: l, log: log, metrics: newMetrics("payment-service")}
	for _, o := range opts {
		o(s)
	}
	s.routes = []route{
		{"GET /healthz", s.health},
		{"GET /metrics", s.serveMetrics},
		{"POST /charges", s.charge},
		{"GET /charges/{order_id}", s.admin(s.getCharge)},
		{"DELETE /charges/{order_id}", s.cancelCharge},
	}
	return s
}

func (s *Server) gauges() []Gauge {
	var out []Gauge
	for st, n := range s.ledger.Counts() {
		out = append(out, Gauge{
			Name: "gocart_charges", Help: "Charges in the ledger, by status.",
			Labels: map[string]string{"status": string(st)}, Value: float64(n),
		})
	}
	return out
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

type chargeRequest struct {
	OrderID     string `json:"order_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Token       string `json:"payment_token"`
}

// charge answers three different things with three different statuses, because
// the caller must act differently on each:
//
//	201/200  approved: carry on
//	402      declined: definite; do not retry, tell the customer
//	503      the provider did not answer: outcome unknown; retry later
func (s *Server) charge(w http.ResponseWriter, r *http.Request) {
	var in chargeRequest
	if !decode(w, r, &in) {
		return
	}
	c, replayed, err := s.ledger.Charge(in.OrderID, in.AmountMinor, in.Currency, in.Token)
	if err != nil {
		s.fail(w, r, in.OrderID, err)
		return
	}
	switch c.Status {
	case ledger.StatusDeclined:
		writeProblem(w, r, Problem{
			Type: "payment-declined", Title: "Payment declined", Status: http.StatusPaymentRequired,
			Detail: "The card was declined. Retrying this order will not change the answer.",
			Extra:  map[string]any{"order_id": c.OrderID, "decline_code": c.DeclineCode},
		})
	case ledger.StatusVoided:
		s.fail(w, r, in.OrderID, ledger.ErrVoided)
	default:
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		w.Header().Set("Location", "/charges/"+c.OrderID)
		writeJSON(w, status, c)
	}
}

func (s *Server) getCharge(w http.ResponseWriter, r *http.Request) {
	c, err := s.ledger.Get(r.PathValue("order_id"))
	if err != nil {
		s.fail(w, r, "", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// cancelCharge is DELETE because the outcome is the same however many times it
// is sent: the order's charge ends up voided.
func (s *Server) cancelCharge(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.ledger.Cancel(r.PathValue("order_id")); err != nil {
		s.fail(w, r, "", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		writeProblem(w, r, Problem{Type: "payload-too-large", Title: "Request body too large", Status: http.StatusRequestEntityTooLarge})
		return false
	}
	writeProblem(w, r, Problem{Type: "invalid-request", Title: "Malformed request body", Status: http.StatusBadRequest, Detail: err.Error()})
	return false
}

// fail maps domain errors onto problem responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, orderID string, err error) {
	switch {
	case errors.Is(err, ledger.ErrProviderUnavailable):
		w.Header().Set("Retry-After", "2")
		writeProblem(w, r, Problem{
			Type: "provider-unavailable", Title: "Payment provider unavailable", Status: http.StatusServiceUnavailable,
			Detail: "The provider did not answer, so the outcome is unknown and nothing was recorded. Retrying is safe.",
		})
	case errors.Is(err, ledger.ErrVoided):
		writeProblem(w, r, Problem{
			Type: "charge-voided", Title: "Charge was cancelled", Status: http.StatusConflict,
			Detail: err.Error(), Extra: map[string]any{"order_id": orderID},
		})
	case errors.Is(err, ledger.ErrMismatch):
		writeProblem(w, r, Problem{
			Type: "charge-mismatch", Title: "Charge already exists with a different amount", Status: http.StatusConflict,
			Detail: err.Error(),
		})
	case errors.Is(err, ledger.ErrNotFound):
		writeProblem(w, r, Problem{Type: "not-found", Title: "Not found", Status: http.StatusNotFound})
	case errors.Is(err, ledger.ErrInvalid):
		writeProblem(w, r, Problem{
			Type: "validation-failed", Title: "Validation failed",
			Status: http.StatusUnprocessableEntity, Detail: err.Error(),
		})
	default:
		s.log.Error("unhandled error", "err", err, "request_id", requestIDFrom(r.Context()))
		writeProblem(w, r, Problem{Type: "internal", Title: "Internal error", Status: http.StatusInternalServerError})
	}
}
