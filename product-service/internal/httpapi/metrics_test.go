package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"gocart/product-service/internal/catalog"
)

var sampleLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-z_]+="([^"\\]|\\.)*",?)*\})? [0-9.e+-]+$`)

func TestAdminEndpointsNeedTheKey(t *testing.T) {
	_, h := newTestServer(t)
	create := `{"name":"Cable","price_minor":19900,"stock":7}`

	rec := doAnon(t, h, "POST", "/products", create)
	wantProblem(t, rec, 401, "unauthorized")
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 must say how to authenticate")
	}
	wantProblem(t, doAnon(t, h, "POST", "/products", create, "Authorization", "Bearer wrong"), 401, "unauthorized")
	wantProblem(t, doAnon(t, h, "POST", "/products", create, "Authorization", "Basic "+testKey), 401, "unauthorized")
	wantProblem(t, doAnon(t, h, "POST", "/products", create, "Authorization", testKey), 401, "unauthorized")
	wantProblem(t, doAnon(t, h, "PUT", "/products/prd_lamp/stock", `{"stock":1}`), 401, "unauthorized")

	if rec := doAnon(t, h, "POST", "/products", create, "Authorization", "Bearer "+testKey); rec.Code != http.StatusCreated {
		t.Fatalf("with the key: %d %s", rec.Code, rec.Body)
	}
	// A refused request must not have changed anything.
	if n := len(decodeBody(t, do(t, h, "GET", "/products", ""))["items"].([]any)); n != 7 {
		t.Errorf("%d products, want the 6 seeded plus the one created", n)
	}
}

func TestPublicEndpointsNeedNoKey(t *testing.T) {
	_, h := newTestServer(t)
	for _, path := range []string{"/healthz", "/metrics", "/products", "/products/prd_lamp"} {
		if rec := doAnon(t, h, "GET", path, ""); rec.Code != 200 {
			t.Errorf("GET %s without a key: %d", path, rec.Code)
		}
	}
}

func TestWithoutAKeyConfiguredAdminFailsClosed(t *testing.T) {
	store, _ := catalog.Open("")
	srv := New(store, slog.New(slog.NewTextHandler(io.Discard, nil))) // no WithAdminKey
	h := srv.Handler()
	for _, key := range []string{"", "Bearer ", "Bearer anything"} {
		hdr := []string{}
		if key != "" {
			hdr = []string{"Authorization", key}
		}
		wantProblem(t, doAnon(t, h, "POST", "/products", `{"name":"x","price_minor":1,"stock":1}`, hdr...), 403, "admin-disabled")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, "GET", "/products/prd_lamp", "")
	do(t, h, "GET", "/products/prd_keyboard", "")
	do(t, h, "GET", "/products/prd_nope", "")
	doAnon(t, h, "POST", "/products", `{}`) // refused: no key
	do(t, h, "GET", "/no/such/route", "")

	rec := do(t, h, "GET", "/metrics", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()

	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		if !sampleLine.MatchString(line) {
			t.Errorf("not valid exposition format: %q", line)
		}
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("exposition must end with a newline")
	}

	want := []string{
		`gocart_http_requests_total{code="200",method="GET",route="GET /products/{id}",service="product-service"} 2`,
		`gocart_http_requests_total{code="404",method="GET",route="GET /products/{id}",service="product-service"} 1`,
		`gocart_http_requests_total{code="404",method="GET",route="unmatched",service="product-service"} 1`,
		`gocart_http_requests_total{code="401",method="POST",route="POST /products",service="product-service"} 1`,
		`gocart_admin_auth_failures_total{service="product-service"} 1`,
		`gocart_http_request_duration_seconds_count{route="GET /products/{id}",service="product-service"} 3`,
		`gocart_http_request_duration_seconds_bucket{le="+Inf",route="GET /products/{id}",service="product-service"} 3`,
		`gocart_products{service="product-service"} 6`,
	}
	for _, w := range want {
		if !strings.Contains(body, w+"\n") {
			t.Errorf("metrics lack %s\n%s", w, body)
		}
	}
	// Route patterns, not raw paths: ids must never become label values.
	for _, leak := range []string{"prd_lamp", "prd_keyboard", "prd_nope", "/no/such/route"} {
		if strings.Contains(body, leak) {
			t.Errorf("a raw path leaked into the labels: %s", leak)
		}
	}
}

func TestMetricsBucketsAreCumulative(t *testing.T) {
	_, h := newTestServer(t)
	for i := 0; i < 5; i++ {
		do(t, h, "GET", "/healthz", "")
	}
	body := do(t, h, "GET", "/metrics", "").Body.String()
	prev := -1
	re := regexp.MustCompile(`gocart_http_request_duration_seconds_bucket\{le="[^"]+",route="GET /healthz",service="product-service"\} (\d+)`)
	matches := re.FindAllStringSubmatch(body, -1)
	if len(matches) != 12 {
		t.Fatalf("%d buckets, want 11 plus +Inf", len(matches))
	}
	for _, m := range matches {
		var n int
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		if n < prev {
			t.Errorf("buckets decrease: %v", matches)
		}
		prev = n
	}
	if prev != 5 {
		t.Errorf("+Inf bucket = %d, want 5", prev)
	}
}
