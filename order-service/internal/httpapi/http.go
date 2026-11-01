// Package httpapi is the HTTP surface of the Order service. The contract it
// implements lives in docs/openapi/order.yaml, written before this code, and a
// test fails if the two drift apart.
//
// Commands (POST) go through the service, which replays events and decides.
// Queries (GET /orders...) read the projection and never replay anything.
// GET /orders/{id}/events is the exception on purpose: it shows the log itself.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/projection"
	"gocart/order-service/internal/reqid"
	"gocart/order-service/internal/service"
)

const (
	maxBodyBytes   = 1 << 20
	maxListLimit   = 200
	idempotencyHdr = "Idempotency-Key"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type route struct {
	pattern string
	handler http.HandlerFunc
}

// Server wires the Order service to HTTP.
type Server struct {
	svc    *service.Service
	log    *slog.Logger
	routes []route
}

// New builds a Server.
func New(svc *service.Service, log *slog.Logger) *Server {
	s := &Server{svc: svc, log: log}
	s.routes = []route{
		{"GET /healthz", s.health},
		{"POST /orders", s.placeOrder},
		{"GET /orders", s.listOrders},
		{"GET /orders/{id}", s.getOrder},
		{"GET /orders/{id}/events", s.orderEvents},
		{"POST /orders/{id}/cancel", s.cancelOrder},
		{"POST /orders/{id}/ship", s.shipOrder},
	}
	return s
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
		mux.HandleFunc(rt.pattern, rt.handler)
	}
	return requestID(accessLog(s.log, recoverer(s.log, mux)))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CustomerID string        `json:"customer_id"`
		Items      []domain.Item `json:"items"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	key := r.Header.Get(idempotencyHdr)
	if key != "" && !idempotencyKeyPattern.MatchString(key) {
		writeProblem(w, r, Problem{
			Type: "invalid-request", Title: "Invalid Idempotency-Key", Status: http.StatusBadRequest,
			Detail: "Idempotency-Key must be 1-128 characters from A-Z a-z 0-9 . _ : -",
		})
		return
	}
	res, err := s.svc.Place(r.Context(), in.CustomerID, key, in.Items)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o := res.Order
	switch o.Status {
	case domain.StatusRejected:
		// The order is recorded (GET /orders/{id} shows it) but the answer to
		// "can I have this?" is no, so the caller gets a failure status.
		writeProblem(w, r, Problem{
			Type: "order-rejected", Title: "Order rejected", Status: http.StatusConflict,
			Detail: o.RejectDetail,
			Extra:  map[string]any{"order_id": o.ID, "reason": o.RejectReason},
		})
	case domain.StatusPlaced:
		// Accepted but not decided: the Product service did not answer. The
		// order is safe and will be settled automatically.
		w.Header().Set("Location", "/orders/"+o.ID)
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusAccepted, o)
	default:
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		w.Header().Set("Location", "/orders/"+o.ID)
		writeJSON(w, status, o)
	}
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	v, ok := s.svc.Projection().Get(r.PathValue("id"))
	if !ok {
		writeProblem(w, r, Problem{Type: "not-found", Title: "Order not found", Status: http.StatusNotFound})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := projection.Filter{CustomerID: q.Get("customer_id")}
	if st := q.Get("status"); st != "" {
		switch domain.Status(st) {
		case domain.StatusPlaced, domain.StatusConfirmed, domain.StatusRejected, domain.StatusCancelled, domain.StatusShipped:
			f.Status = domain.Status(st)
		default:
			writeProblem(w, r, Problem{
				Type: "validation-failed", Title: "Validation failed", Status: http.StatusUnprocessableEntity,
				Detail: "status must be one of placed, confirmed, rejected, cancelled, shipped",
			})
			return
		}
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxListLimit {
			writeProblem(w, r, Problem{
				Type: "validation-failed", Title: "Validation failed", Status: http.StatusUnprocessableEntity,
				Detail: "limit must be an integer between 1 and " + strconv.Itoa(maxListLimit),
			})
			return
		}
		f.Limit = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.svc.Projection().List(f)})
}

func (s *Server) orderEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.svc.History(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type out struct {
		Version int             `json:"version"`
		Type    string          `json:"type"`
		At      time.Time       `json:"at"`
		Data    json.RawMessage `json:"data"`
	}
	items := make([]out, len(events))
	for i, e := range events {
		items[i] = out{Version: e.Version, Type: e.Type, At: e.At, Data: e.Data}
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": r.PathValue("id"), "events": items})
}

func (s *Server) cancelOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in, true) {
		return
	}
	v, err := s.svc.Cancel(r.Context(), r.PathValue("id"), in.Reason)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) shipOrder(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.Ship(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// decode reads a strict JSON body: unknown fields and trailing data are errors,
// because silently ignoring a misspelt field is how "quantity" becomes 0. With
// allowEmpty, a missing body is accepted and leaves dst untouched.
func decode(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if errors.Is(err, io.EOF) && allowEmpty {
		return true
	}
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

// fail maps domain and service errors onto problem responses.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeProblem(w, r, Problem{Type: "not-found", Title: "Order not found", Status: http.StatusNotFound})
	case errors.Is(err, domain.ErrValidation):
		writeProblem(w, r, Problem{Type: "validation-failed", Title: "Validation failed", Status: http.StatusUnprocessableEntity, Detail: err.Error()})
	case errors.Is(err, domain.ErrInvalidTransition):
		writeProblem(w, r, Problem{Type: "invalid-state", Title: "Order is in the wrong state for this action", Status: http.StatusConflict, Detail: err.Error()})
	case errors.Is(err, service.ErrIdempotencyConflict):
		writeProblem(w, r, Problem{Type: "idempotency-key-reuse", Title: "Idempotency-Key reused with a different request", Status: http.StatusUnprocessableEntity, Detail: err.Error()})
	case errors.Is(err, service.ErrContention):
		w.Header().Set("Retry-After", "1")
		writeProblem(w, r, Problem{Type: "concurrent-modification", Title: "Order is being modified concurrently", Status: http.StatusConflict, Detail: err.Error()})
	default:
		s.log.Error("unhandled error", "err", err, "request_id", reqid.From(r.Context()))
		writeProblem(w, r, Problem{Type: "internal", Title: "Internal error", Status: http.StatusInternalServerError})
	}
}
