// Package httpapi is the web gateway: it serves the single-page front end and
// forwards a fixed set of API calls to the services behind it.
//
// The browser talks to one address. Without this, it would need to know four
// service URLs and every service would need CORS rules.
//
// The gateway is where people sign in (OpenID Connect, see auth.go). Behind it
// the services do their own checking: for every request it forwards, the
// gateway attaches a short-lived signed statement of who the caller is, and
// the Order and Cart services only let customers touch their own data. Admin
// operations are only forwarded for administrators, with the admin key the
// browser never sees. Every error the services give comes back unchanged. The
// other things it adds are a request ID, a 502 problem when a service is
// unreachable, and an allow-list: a route not listed here does not exist, so
// the endpoints that exist only for service-to-service calls (reserving stock,
// creating charges) cannot be reached from a browser.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"gocart/web-service/internal/reqid"
)

const maxBodyBytes = 1 << 20

// Upstreams are the base URLs of the services behind the gateway.
type Upstreams struct{ Product, Order, Cart, Payment string }

type route struct {
	pattern string
	handler http.HandlerFunc
}

// Server is the gateway.
type Server struct {
	up      Upstreams
	ui      fs.FS
	log     *slog.Logger
	metrics *metrics
	routes  []route
	client  *http.Client
	auth    Auth
}

// New builds the gateway. ui is the built front end; it may be empty.
func New(up Upstreams, ui fs.FS, log *slog.Logger, opts ...Option) (*Server, error) {
	s := &Server{up: up, ui: ui, log: log, metrics: newMetrics("web-service"),
		client: &http.Client{Timeout: 1500 * time.Millisecond}}
	for _, o := range opts {
		o(s)
	}
	if err := s.auth.check(); err != nil {
		return nil, err
	}
	product, err := s.proxy(up.Product, "product-service")
	if err != nil {
		return nil, err
	}
	order, err := s.proxy(up.Order, "order-service")
	if err != nil {
		return nil, err
	}
	cart, err := s.proxy(up.Cart, "cart-service")
	if err != nil {
		return nil, err
	}
	payment, err := s.proxy(up.Payment, "payment-service")
	if err != nil {
		return nil, err
	}

	// Who may call what: public needs nothing, user needs a signed-in session,
	// admin needs a session whose person is an administrator.
	g := s.guard
	s.routes = []route{
		{"GET /healthz", s.health},
		{"GET /metrics", s.serveMetrics},

		{"GET /auth/login", s.login},
		{"GET /auth/callback", s.callback},
		{"POST /auth/logout", s.logout},
		{"GET /api/me", s.me},
		{"GET /api/status", g(admin, s.status)},

		{"GET /api/products", g(public, product)},
		{"POST /api/products", g(admin, product)},
		{"GET /api/products/{id}", g(public, product)},
		{"PUT /api/products/{id}/stock", g(admin, product)},

		{"POST /api/orders", g(user, order)},
		{"GET /api/orders", g(user, order)},
		{"GET /api/orders/{id}", g(user, order)},
		{"GET /api/orders/{id}/events", g(admin, order)},
		{"POST /api/orders/{id}/cancel", g(user, order)},
		{"POST /api/orders/{id}/ship", g(admin, order)},
		{"POST /api/orders/{id}/deliver", g(admin, order)},

		{"GET /api/carts/{customer_id}", g(user, cart)},
		{"DELETE /api/carts/{customer_id}", g(user, cart)},
		{"PUT /api/carts/{customer_id}/items/{product_id}", g(user, cart)},
		{"DELETE /api/carts/{customer_id}/items/{product_id}", g(user, cart)},
		{"POST /api/carts/{customer_id}/checkout", g(user, cart)},

		{"GET /api/charges/{order_id}", g(admin, payment)},

		{"/api/", s.noSuchAPI},
		{"/", s.app},
	}
	return s, nil
}

func (s *Server) gauges() []Gauge { return nil }

// Routes lists the patterns the gateway answers, for tests and documentation.
func (s *Server) Routes() []string {
	out := make([]string, len(s.routes))
	for i, rt := range s.routes {
		out[i] = rt.pattern
	}
	return out
}

// Handler returns the gateway wrapped in its middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes {
		mux.HandleFunc(rt.pattern, s.metrics.wrap(rt.pattern, rt.handler))
	}
	return requestID(accessLog(s.log, s.metrics.middleware(recoverer(s.log, securityHeaders(mux)))))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// securityHeaders applies to every response. The page loads only its own
// scripts and styles and may call only its own origin, so a script injected
// from somewhere else has nothing to run and nowhere to send data.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// proxy returns a handler that forwards to base with the /api prefix removed.
func (s *Server) proxy(base, name string) (http.HandlerFunc, error) {
	target, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, &url.Error{Op: "parse", URL: base, Err: errBadUpstream}
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/api")
			pr.Out.URL.RawPath = ""
			if id := reqid.From(pr.In.Context()); id != "" {
				pr.Out.Header.Set(reqid.Header, id)
			}
			s.vouch(pr)
		},
		Transport: &http.Transport{
			DialContext:           (&dialer{timeout: 3 * time.Second}).dial,
			ResponseHeaderTimeout: 25 * time.Second, // checkout waits for the Order service, which waits for two others
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       60 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("upstream did not answer", "service", name, "err", err, "request_id", reqid.From(r.Context()))
			w.Header().Set("Retry-After", "2")
			writeProblem(w, r, Problem{
				Type: "upstream-unavailable", Title: "A service did not answer", Status: http.StatusBadGateway,
				Detail: "The " + name + " is not reachable right now. Nothing was changed by this request unless the service says otherwise.",
				Extra:  map[string]any{"service": name},
			})
		},
	}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		rp.ServeHTTP(w, r)
	}, nil
}

func (s *Server) noSuchAPI(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, Problem{Type: "not-found", Title: "No such endpoint", Status: http.StatusNotFound,
		Detail: "The gateway does not expose " + r.Method + " " + r.URL.Path + "."})
}

type serviceStatus struct {
	Name      string `json:"name"`
	Up        bool   `json:"up"`
	LatencyMS int64  `json:"latency_ms"`
}

// status asks every service whether it is healthy, at the same time, so the
// admin console can show the whole system on one screen.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	targets := []struct{ name, base string }{
		{"product-service", s.up.Product}, {"order-service", s.up.Order},
		{"payment-service", s.up.Payment}, {"cart-service", s.up.Cart},
	}
	out := make([]serviceStatus, len(targets))
	done := make(chan struct{}, len(targets))
	for i, t := range targets {
		go func() {
			defer func() { done <- struct{}{} }()
			out[i] = s.probe(r.Context(), t.name, t.base)
		}()
	}
	for range targets {
		<-done
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

func (s *Server) probe(ctx context.Context, name, base string) serviceStatus {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/healthz", nil)
	if err != nil {
		return serviceStatus{Name: name}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return serviceStatus{Name: name}
	}
	defer resp.Body.Close()
	return serviceStatus{Name: name, Up: resp.StatusCode == http.StatusOK, LatencyMS: time.Since(start).Milliseconds()}
}
