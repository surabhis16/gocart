package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is copied between services on purpose. It is about 150 lines of
// stable code; sharing it would mean a change to it forces every service to
// release together, which is the coupling the architecture avoids. (This copy has no admin-key code: the gateway holds no secrets and
// leaves every admin check to the service behind it.)

// ---- metrics ---------------------------------------------------------------
//
// Metrics are exposed in the Prometheus text format, written by hand: the
// format is small and stable, and it keeps the services free of dependencies.
// Request metrics are labelled by route pattern ("GET /orders/{id}"), never by
// the raw path, so a million different order ids are one series, not a million.

// Gauge is a point-in-time value contributed by a service's own domain, such
// as "orders by status".
type Gauge struct {
	Name   string
	Help   string
	Labels map[string]string
	Value  float64
}

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type reqKey struct{ method, route, code string }

type histogram struct {
	counts []uint64 // one per bucket, not cumulative; made cumulative when written
	sum    float64
	n      uint64
}

type metrics struct {
	service string
	version string
	start   time.Time

	mu       sync.Mutex
	requests map[reqKey]uint64
	latency  map[string]*histogram // by route
	signins  map[string]uint64     // sign-in attempts, by result
}

// logins counts a sign-in attempt by how it ended.
func (m *metrics) logins(result string) {
	m.mu.Lock()
	m.signins[result]++
	m.mu.Unlock()
}

func newMetrics(service string) *metrics {
	return &metrics{
		service:  service,
		start:    time.Now(),
		requests: map[reqKey]uint64{},
		latency:  map[string]*histogram{},
		signins:  map[string]uint64{},
	}
}

type routeKey struct{}

// routeLabel is filled in by wrap once the router has chosen a handler, so the
// outer middleware can label the request even though it runs before routing.
type routeLabel struct{ pattern string }

// middleware records every request, including ones the router rejects.
func (m *metrics) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lbl := &routeLabel{}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), routeKey{}, lbl)))
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		route := lbl.pattern
		if route == "" {
			route = "unmatched" // 404 and 405 from the router: one bucket, bounded
		}
		m.observe(r.Method, route, status, time.Since(start))
	})
}

// wrap tells the middleware which route a handler serves.
func (m *metrics) wrap(pattern string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if lbl, ok := r.Context().Value(routeKey{}).(*routeLabel); ok {
			lbl.pattern = pattern
		}
		h(w, r)
	}
}

func (m *metrics) observe(method, route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[reqKey{method, route, strconv.Itoa(status)}]++
	h := m.latency[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(durationBuckets))}
		m.latency[route] = h
	}
	secs := d.Seconds()
	h.sum += secs
	h.n++
	for i, le := range durationBuckets {
		if secs <= le {
			h.counts[i]++
			break
		}
	}
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func labels(kv map[string]string) string {
	if len(kv) == 0 {
		return ""
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + `="` + escapeLabel(kv[k]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// write renders everything in Prometheus text format, in a stable order.
func (m *metrics) write(w io.Writer, gauges []Gauge) {
	m.mu.Lock()
	reqs := make([]reqKey, 0, len(m.requests))
	for k := range m.requests {
		reqs = append(reqs, k)
	}
	sort.Slice(reqs, func(i, j int) bool {
		a, b := reqs[i], reqs[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.code < b.code
	})
	reqCounts := make([]uint64, len(reqs))
	for i, k := range reqs {
		reqCounts[i] = m.requests[k]
	}
	routes := make([]string, 0, len(m.latency))
	hists := map[string]histogram{}
	for r, h := range m.latency {
		routes = append(routes, r)
		hists[r] = histogram{counts: append([]uint64(nil), h.counts...), sum: h.sum, n: h.n}
	}
	logins := make(map[string]uint64, len(m.signins))
	for k, v := range m.signins {
		logins[k] = v
	}
	m.mu.Unlock()
	sort.Strings(routes)

	fmt.Fprintf(w, "# HELP gocart_build_info Which build is running.\n# TYPE gocart_build_info gauge\n")
	fmt.Fprintf(w, "gocart_build_info%s 1\n", labels(map[string]string{"service": m.service, "version": m.version}))
	fmt.Fprintf(w, "# HELP gocart_uptime_seconds Seconds since the process started.\n# TYPE gocart_uptime_seconds gauge\n")
	fmt.Fprintf(w, "gocart_uptime_seconds%s %.0f\n", labels(map[string]string{"service": m.service}), time.Since(m.start).Seconds())

	fmt.Fprintf(w, "# HELP gocart_signins_total Sign-in attempts, by how they ended.\n# TYPE gocart_signins_total counter\n")
	results := make([]string, 0, len(logins))
	for k := range logins {
		results = append(results, k)
	}
	sort.Strings(results)
	for _, k := range results {
		fmt.Fprintf(w, "gocart_signins_total%s %d\n", labels(map[string]string{"service": m.service, "result": k}), logins[k])
	}

	fmt.Fprintf(w, "# HELP gocart_http_requests_total Requests handled, by route pattern and status.\n# TYPE gocart_http_requests_total counter\n")
	for i, k := range reqs {
		fmt.Fprintf(w, "gocart_http_requests_total%s %d\n",
			labels(map[string]string{"service": m.service, "route": k.route, "method": k.method, "code": k.code}), reqCounts[i])
	}

	fmt.Fprintf(w, "# HELP gocart_http_request_duration_seconds Time to handle a request, by route pattern.\n# TYPE gocart_http_request_duration_seconds histogram\n")
	for _, route := range routes {
		h := hists[route]
		var cum uint64
		for i, le := range durationBuckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "gocart_http_request_duration_seconds_bucket%s %d\n",
				labels(map[string]string{"service": m.service, "route": route, "le": strconv.FormatFloat(le, 'g', -1, 64)}), cum)
		}
		base := map[string]string{"service": m.service, "route": route}
		inf := map[string]string{"service": m.service, "route": route, "le": "+Inf"}
		fmt.Fprintf(w, "gocart_http_request_duration_seconds_bucket%s %d\n", labels(inf), h.n)
		fmt.Fprintf(w, "gocart_http_request_duration_seconds_sum%s %g\n", labels(base), h.sum)
		fmt.Fprintf(w, "gocart_http_request_duration_seconds_count%s %d\n", labels(base), h.n)
	}

	seen := map[string]bool{}
	sort.SliceStable(gauges, func(i, j int) bool { return gauges[i].Name < gauges[j].Name })
	for _, g := range gauges {
		if !seen[g.Name] {
			seen[g.Name] = true
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.Name, g.Help, g.Name)
		}
		ls := map[string]string{"service": m.service}
		for k, v := range g.Labels {
			ls[k] = v
		}
		fmt.Fprintf(w, "%s%s %g\n", g.Name, labels(ls), g.Value)
	}
}

// Option configures a Server.
type Option func(*Server)

// WithVersion labels gocart_build_info.
func WithVersion(v string) Option { return func(s *Server) { s.metrics.version = v } }

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w, s.gauges())
}
