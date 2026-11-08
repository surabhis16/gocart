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
	"strings"
	"time"

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/identity"
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
	svc      *service.Service
	log      *slog.Logger
	routes   []route
	metrics  *metrics
	adminKey string

	identitySecret  []byte // verifies the gateway's assertion; empty: assertions are refused
	requireIdentity bool   // customer routes answer 401 to a caller that is not signed in
}

// New builds a Server.
func New(svc *service.Service, log *slog.Logger, opts ...Option) *Server {
	s := &Server{svc: svc, log: log, metrics: newMetrics("order-service")}
	for _, o := range opts {
		o(s)
	}
	s.routes = []route{
		{"GET /healthz", s.health},
		{"GET /metrics", s.serveMetrics},
		{"POST /orders", s.placeOrder},
		{"GET /orders", s.listOrders},
		{"GET /orders/{id}", s.getOrder},
		{"GET /orders/{id}/events", s.admin(s.orderEvents)},
		{"POST /orders/{id}/cancel", s.cancelOrder},
		{"POST /orders/{id}/ship", s.admin(s.shipOrder)},
		{"POST /orders/{id}/deliver", s.admin(s.deliverOrder)},
	}
	return s
}

func (s *Server) gauges() []Gauge {
	var out []Gauge
	for st, n := range s.svc.Projection().StatusCounts() {
		out = append(out, Gauge{
			Name: "gocart_orders", Help: "Orders, by status.",
			Labels: map[string]string{"status": string(st)}, Value: float64(n),
		})
	}
	// Work the reconciler still has to do. Anything here that keeps growing
	// means a downstream service has been unavailable for a while.
	w := s.svc.Projection().Pending(time.Now(), 0)
	for kind, ids := range map[string][]string{"placed": w.Placed, "awaiting_payment": w.Unpaid, "stock_release": w.Release, "refund": w.Refund} {
		out = append(out, Gauge{
			Name: "gocart_orders_pending_work", Help: "Orders that still need the reconciler to finish something, by kind.",
			Labels: map[string]string{"kind": kind}, Value: float64(len(ids)),
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
	return requestID(accessLog(s.log, s.metrics.middleware(recoverer(s.log, s.identified(mux)))))
}

// identified verifies the gateway's assertion when a request carries one and
// makes the caller available to the handlers. A bad or expired assertion is a
// 401 and never a quiet downgrade to "anonymous".
func (s *Server) identified(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get(identity.Header)
		if tok == "" {
			next.ServeHTTP(w, r)
			return
		}
		claims, err := identity.Verify(s.identitySecret, tok, time.Now())
		if len(s.identitySecret) == 0 || err != nil {
			s.metrics.authFailures.Add(1)
			writeProblem(w, r, Problem{
				Type: "unauthenticated", Title: "Identity not accepted", Status: http.StatusUnauthorized,
				Detail: "The identity assertion is missing, expired or not signed by the gateway. Sign in again.",
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(identity.With(r.Context(), claims, tok)))
	})
}

// caller says who is asking. signedIn is false for a service-to-service or
// local call that carries no assertion; with requireIdentity set that is
// refused here, and ok is false because the 401 has been written.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (c identity.Claims, signedIn, ok bool) {
	if c, signedIn = identity.From(r.Context()); signedIn {
		return c, true, true
	}
	if s.requireIdentity {
		s.metrics.authFailures.Add(1)
		writeProblem(w, r, Problem{
			Type: "unauthenticated", Title: "Sign in required", Status: http.StatusUnauthorized,
			Detail: "This request carries no identity. Orders are only available to the customer who placed them.",
		})
		return c, false, false
	}
	return c, false, true
}

// owns: a signed-in customer sees their own orders, an administrator sees all.
func owns(c identity.Claims, signedIn bool, customerID string) bool {
	return !signedIn || c.Admin() || c.Sub == customerID
}

func forbidden(w http.ResponseWriter, r *http.Request, detail string) {
	writeProblem(w, r, Problem{Type: "forbidden", Title: "Not your order", Status: http.StatusForbidden, Detail: detail})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) placeOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CustomerID   string        `json:"customer_id"`
		Items        []domain.Item `json:"items"`
		PaymentToken string        `json:"payment_token"`
	}
	who, signedIn, ok := s.caller(w, r)
	if !ok {
		return
	}
	if !decode(w, r, &in, false) {
		return
	}
	if signedIn && who.Sub != in.CustomerID {
		forbidden(w, r, "An order can only be placed for the customer who is signed in.")
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
	res, err := s.svc.Place(r.Context(), in.CustomerID, key, in.PaymentToken, in.Items)
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
	case domain.StatusPaymentFailed:
		// Definite: the card network said no. The stock is already going back.
		writeProblem(w, r, Problem{
			Type: "payment-declined", Title: "Payment declined", Status: http.StatusPaymentRequired,
			Detail: "The card was declined. The order is recorded as payment_failed and its stock has been released.",
			Extra:  map[string]any{"order_id": o.ID, "decline_code": o.DeclineCode},
		})
	case domain.StatusExpired:
		// Only seen when an old Idempotency-Key is replayed after the time limit.
		writeProblem(w, r, Problem{
			Type: "order-expired", Title: "Order expired", Status: http.StatusConflict,
			Detail: "Payment was not completed in time, so the order was released.",
			Extra:  map[string]any{"order_id": o.ID},
		})
	case domain.StatusPlaced, domain.StatusAwaitingPayment:
		// Accepted but not decided: the Product or Payment service did not
		// answer. The order is safe and will be settled automatically.
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
	who, signedIn, ok := s.caller(w, r)
	if !ok {
		return
	}
	// Served from the read model; if it has never heard of the id, the service
	// checks the store, since another replica may have created the order a
	// moment ago.
	v, err := s.svc.Order(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !owns(who, signedIn, v.CustomerID) {
		// Someone else's order is indistinguishable from one that does not exist.
		s.fail(w, r, service.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	who, signedIn, ok := s.caller(w, r)
	if !ok {
		return
	}
	f := projection.Filter{CustomerID: q.Get("customer_id")}
	if signedIn && !who.Admin() {
		if f.CustomerID != "" && f.CustomerID != who.Sub {
			forbidden(w, r, "You can only list your own orders.")
			return
		}
		f.CustomerID = who.Sub
	}
	if st := q.Get("status"); st != "" {
		valid := false
		names := make([]string, len(domain.AllStatuses))
		for i, v := range domain.AllStatuses {
			names[i] = string(v)
			valid = valid || string(v) == st
		}
		if !valid {
			writeProblem(w, r, Problem{
				Type: "validation-failed", Title: "Validation failed", Status: http.StatusUnprocessableEntity,
				Detail: "status must be one of " + strings.Join(names, ", "),
			})
			return
		}
		f.Status = domain.Status(st)
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
		items[i] = out{Version: e.Version, Type: e.Type, At: e.At, Data: redact(e)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"order_id": r.PathValue("id"), "events": items})
}

func (s *Server) cancelOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	who, signedIn, ok := s.caller(w, r)
	if !ok {
		return
	}
	if !decode(w, r, &in, true) {
		return
	}
	if signedIn && !who.Admin() {
		if cur, err := s.svc.Order(r.PathValue("id")); err == nil && !owns(who, signedIn, cur.CustomerID) {
			s.fail(w, r, service.ErrNotFound)
			return
		}
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

func (s *Server) deliverOrder(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.Deliver(r.Context(), r.PathValue("id"))
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

// redact removes the payment token from an OrderPlaced payload. The token is
// stored in the log so a payment can be retried, but nothing that reads the
// history over HTTP needs it.
func redact(e domain.Event) json.RawMessage {
	if e.Type != domain.TypeOrderPlaced {
		return e.Data
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.Data, &m); err != nil {
		return e.Data
	}
	delete(m, "payment_token")
	b, err := json.Marshal(m)
	if err != nil {
		return e.Data
	}
	return b
}
