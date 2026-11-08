package httpapi

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// backend records what reached it and answers with a canned response.
type backend struct {
	srv     *httptest.Server
	method  string
	path    string
	query   string
	headers http.Header
	body    string
	status  int
	reply   string
}

func newBackend(t *testing.T, status int, reply string) *backend {
	t.Helper()
	b := &backend{status: status, reply: reply}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.method, b.path, b.query, b.headers, b.body = r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(raw)
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-From", "backend")
		w.WriteHeader(b.status)
		w.Write([]byte(b.reply))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

var app = fstest.MapFS{
	"index.html":        {Data: []byte("<!doctype html><title>GoCart</title><div id=root></div>")},
	"assets/app-abc.js": {Data: []byte("console.log('hi')")},
}

type fixture struct {
	h                       http.Handler
	srv                     *Server
	product, order, cart, y *backend
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		product: newBackend(t, 200, `{"items":[]}`), order: newBackend(t, 201, `{"id":"ord_1"}`),
		cart: newBackend(t, 200, `{"items":[]}`), y: newBackend(t, 200, `{"status":"approved"}`),
	}
	srv, err := New(Upstreams{Product: f.product.srv.URL, Order: f.order.srv.URL, Cart: f.cart.srv.URL, Payment: f.y.srv.URL},
		app, slog.New(slog.NewTextHandler(io.Discard, nil)), WithVersion("test"), WithAuth(testAuth(t)))
	if err != nil {
		t.Fatal(err)
	}
	f.srv, f.h = srv, srv.Handler()
	return f
}

// do sends a request as a signed-in administrator, the way the app does:
// with the session cookie and the header that marks it as the app's own call.
// anon, asCustomer and send are the other ways in.
func (f *fixture) do(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	return f.send(method, path, body, append([]string{"Cookie", f.cookie(true), csrfHeader, csrfValue}, hdr...)...)
}

func (f *fixture) asCustomer(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	return f.send(method, path, body, append([]string{"Cookie", f.cookie(false), csrfHeader, csrfValue}, hdr...)...)
}

func (f *fixture) anon(method, path, body string) *httptest.ResponseRecorder {
	return f.send(method, path, body, csrfHeader, csrfValue)
}

func (f *fixture) send(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestEachAPIRouteReachesItsServiceWithThePrefixRemoved(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		method, path string
		to           *backend
		want         string
	}{
		{"GET", "/api/products", f.product, "/products"},
		{"GET", "/api/products/prd_lamp", f.product, "/products/prd_lamp"},
		{"POST", "/api/products", f.product, "/products"},
		{"PUT", "/api/products/prd_lamp/stock", f.product, "/products/prd_lamp/stock"},
		{"POST", "/api/orders", f.order, "/orders"},
		{"GET", "/api/orders", f.order, "/orders"},
		{"GET", "/api/orders/ord_1", f.order, "/orders/ord_1"},
		{"GET", "/api/orders/ord_1/events", f.order, "/orders/ord_1/events"},
		{"POST", "/api/orders/ord_1/cancel", f.order, "/orders/ord_1/cancel"},
		{"POST", "/api/orders/ord_1/ship", f.order, "/orders/ord_1/ship"},
		{"POST", "/api/orders/ord_1/deliver", f.order, "/orders/ord_1/deliver"},
		{"GET", "/api/carts/ada", f.cart, "/carts/ada"},
		{"DELETE", "/api/carts/ada", f.cart, "/carts/ada"},
		{"PUT", "/api/carts/ada/items/prd_lamp", f.cart, "/carts/ada/items/prd_lamp"},
		{"DELETE", "/api/carts/ada/items/prd_lamp", f.cart, "/carts/ada/items/prd_lamp"},
		{"POST", "/api/carts/ada/checkout", f.cart, "/carts/ada/checkout"},
		{"GET", "/api/charges/ord_1", f.y, "/charges/ord_1"},
	}
	for _, c := range cases {
		c.to.method, c.to.path = "", ""
		rec := f.do(c.method, c.path, `{"x":1}`)
		if rec.Code != c.to.status || c.to.method != c.method || c.to.path != c.want {
			t.Errorf("%s %s: status %d, backend saw %s %s; want %s %s", c.method, c.path, rec.Code, c.to.method, c.to.path, c.method, c.want)
		}
	}
}

func TestInternalEndpointsAreNotReachableFromABrowser(t *testing.T) {
	f := newFixture(t)
	for _, c := range [][2]string{
		{"POST", "/api/reservations"}, {"DELETE", "/api/reservations/ord_1"},
		{"POST", "/api/charges"}, {"DELETE", "/api/charges/ord_1"},
		{"GET", "/api/metrics"}, {"GET", "/api/healthz"}, {"DELETE", "/api/products/prd_lamp"},
		{"GET", "/api/orders/ord_1/../../reservations"},
	} {
		rec := f.do(c[0], c[1], "")
		if rec.Code != 404 && rec.Code != 301 && rec.Code != 307 {
			t.Errorf("%s %s reached something: %d", c[0], c[1], rec.Code)
		}
		if rec.Code == 404 && rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s %s: a missing API route must answer with a problem, not HTML (%s)", c[0], c[1], rec.Header().Get("Content-Type"))
		}
	}
	for _, b := range []*backend{f.product, f.order, f.cart, f.y} {
		if b.path != "" {
			t.Errorf("a backend received %s %s", b.method, b.path)
		}
	}
}

func TestQueryStringBodyAndIdempotencyKeyAreForwardedUntouched(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/api/orders/ord_1/ship?x=1&y=a%20b", `{"hello":"world"}`,
		"Idempotency-Key", "k-1", "Content-Type", "application/json")
	if rec.Code != 201 {
		t.Fatalf("%d", rec.Code)
	}
	if f.order.query != "x=1&y=a%20b" || f.order.body != `{"hello":"world"}` {
		t.Errorf("query=%q body=%q", f.order.query, f.order.body)
	}
	if f.order.headers.Get("Idempotency-Key") != "k-1" || f.order.headers.Get("Content-Type") != "application/json" {
		t.Errorf("headers the service needs must reach it: %v", f.order.headers)
	}
}

func TestServiceAnswersPassThroughUnchanged(t *testing.T) {
	f := newFixture(t)
	f.cart.status, f.cart.reply = 402, `{"type":"urn:gocart:problem:payment-declined","status":402}`
	rec := f.do("POST", "/api/carts/ada/checkout", `{"payment_token":"tok_declined"}`)
	if rec.Code != 402 || rec.Header().Get("X-From") != "backend" || !strings.Contains(rec.Body.String(), "payment-declined") {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
}

func TestRequestIDIsAdoptedForwardedAndReturned(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/api/products", "", "X-Request-ID", "trace-42")
	if rec.Header().Get("X-Request-ID") != "trace-42" || f.product.headers.Get("X-Request-ID") != "trace-42" {
		t.Errorf("response %q, backend %q", rec.Header().Get("X-Request-ID"), f.product.headers.Get("X-Request-ID"))
	}
	f.do("GET", "/api/products", "", "X-Request-ID", "bad id with spaces")
	if id := f.product.headers.Get("X-Request-ID"); id == "" || strings.Contains(id, " ") {
		t.Errorf("an unsafe ID must be replaced, got %q", id)
	}
}

func TestAServiceThatIsDownBecomesA502Problem(t *testing.T) {
	f := newFixture(t)
	f.order.srv.Close()
	rec := f.do("POST", "/api/orders", `{}`)
	var p map[string]any
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 502 || rec.Header().Get("Content-Type") != "application/problem+json" ||
		p["type"] != "urn:gocart:problem:upstream-unavailable" || p["service"] != "order-service" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %v", rec.Code, p)
	}
	if rec := f.do("GET", "/api/products", ""); rec.Code != 200 {
		t.Errorf("one service being down must not affect the others: %d", rec.Code)
	}
}

func TestOversizedBodiesAreRefused(t *testing.T) {
	f := newFixture(t)
	rec := f.do("POST", "/api/orders", strings.Repeat("x", 2<<20))
	if rec.Code == 201 {
		t.Error("a 2 MiB body must not be forwarded as if it were fine")
	}
}

func TestStatusReportsEachServiceAndSurvivesOneBeingDown(t *testing.T) {
	f := newFixture(t)
	f.y.srv.Close()
	rec := f.do("GET", "/api/status", "")
	var out struct {
		Services []struct {
			Name string `json:"name"`
			Up   bool   `json:"up"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v", rec.Code, err)
	}
	up := map[string]bool{}
	for _, s := range out.Services {
		up[s.Name] = s.Up
	}
	if len(up) != 4 || !up["product-service"] || !up["order-service"] || !up["cart-service"] || up["payment-service"] {
		t.Errorf("%v", up)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a health reading must not be cached")
	}
}

func TestTheAppIsServedWithFallbackAndCaching(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/", "/orders", "/orders/ord_123", "/admin/orders/ord_9", "/index.html"} {
		rec := f.do("GET", path, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id=root`) || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	rec := f.do("GET", "/assets/app-abc.js", "")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || !strings.Contains(rec.Body.String(), "console.log") {
		t.Errorf("asset: %d %v", rec.Code, rec.Header())
	}
	if rec := f.do("GET", "/assets/missing.js", ""); rec.Code != 404 {
		t.Errorf("a missing file must be a 404, not the app: %d", rec.Code)
	}
	if rec := f.do("POST", "/orders", ""); rec.Code != 405 {
		t.Errorf("only GET is served for the app: %d", rec.Code)
	}
}

func TestAnUnbuiltFrontEndSaysSo(t *testing.T) {
	srv, _ := New(Upstreams{"http://a:1", "http://b:1", "http://c:1", "http://d:1"}, fstest.MapFS{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAuth(testAuth(t)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "npm run build") {
		t.Errorf("%d %q", rec.Code, rec.Body)
	}
}

func TestSecurityHeadersAreOnEverything(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/", "/api/products", "/api/nope", "/healthz"} {
		h := f.do("GET", p, "").Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: %v", p, h)
		}
	}
}

func TestBadUpstreamConfigurationIsRefusedAtStart(t *testing.T) {
	for _, bad := range []string{"", "order-service:8081", "/just/a/path"} {
		_, err := New(Upstreams{Product: "http://p:1", Order: bad, Cart: "http://c:1", Payment: "http://y:1"}, app, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAuth(testAuth(t)))
		if err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestMetricsAndHealth(t *testing.T) {
	f := newFixture(t)
	f.do("GET", "/api/products", "")
	if f.do("GET", "/healthz", "").Code != 200 {
		t.Error("healthz")
	}
	text := f.do("GET", "/metrics", "").Body.String()
	for _, want := range []string{
		`gocart_build_info{service="web-service",version="test"} 1`,
		`route="GET /api/products"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s\n%s", want, text)
		}
	}
}

// Every API route the gateway forwards must be one a service really has, so
// the allow-list cannot quietly point at nothing. The services' own specs are
// the source of truth.
func TestForwardedRoutesExistInTheServiceSpecs(t *testing.T) {
	ops := map[string]bool{}
	for _, name := range []string{"product", "order", "cart", "payment"} {
		for _, op := range specOperations(t, filepath.Join("..", "..", "..", "docs", "openapi", name+".yaml")) {
			ops[op] = true
		}
	}
	f := newFixture(t)
	for _, r := range f.srv.Routes() {
		method, p, ok := strings.Cut(r, " ")
		if !ok || !strings.HasPrefix(p, "/api/") || p == "/api/" || p == "/api/status" || p == "/api/me" {
			continue
		}
		upstream := method + " " + strings.TrimPrefix(p, "/api")
		if !ops[upstream] {
			t.Errorf("the gateway forwards %s, which no service spec defines", upstream)
		}
	}
}

var (
	specPath = regexp.MustCompile(`^  (/[^:\s]*):\s*$`)
	specOp   = regexp.MustCompile(`^    (get|put|post|delete|patch):\s*$`)
)

func specOperations(t *testing.T, file string) []string {
	t.Helper()
	fh, err := os.Open(file)
	if err != nil {
		t.Fatalf("open spec: %v", err)
	}
	defer fh.Close()
	var ops []string
	inPaths, path := false, ""
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, " ") && line != "" {
			inPaths, path = line == "paths:", ""
			continue
		}
		if !inPaths {
			continue
		}
		if m := specPath.FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if m := specOp.FindStringSubmatch(line); m != nil && path != "" {
			ops = append(ops, strings.ToUpper(m[1])+" "+path)
		}
	}
	sort.Strings(ops)
	if len(ops) == 0 {
		t.Fatalf("no operations found in %s", file)
	}
	return ops
}
