package httpapi

import (
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"gocart/order-service/internal/eventstore"
	"gocart/order-service/internal/service"
)

var sampleLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-z_]+="([^"\\]|\\.)*",?)*\})? [0-9.e+-]+$`)

func TestAdminEndpointsNeedTheKey(t *testing.T) {
	f := newFixture(t)
	id := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)

	for _, tc := range []struct{ method, path string }{
		{"POST", "/orders/" + id + "/ship"},
		{"GET", "/orders/" + id + "/events"},
	} {
		rec := f.doAnon(t, tc.method, tc.path, "")
		wantProblem(t, rec, 401, "unauthorized")
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: a 401 must say how to authenticate", tc.method, tc.path)
		}
		wantProblem(t, f.doAnon(t, tc.method, tc.path, "", "Authorization", "Bearer wrong"), 401, "unauthorized")
	}
	// A refused ship must not have shipped anything.
	if got := asMap(t, f.do(t, "GET", "/orders/"+id, ""))["status"]; got != "confirmed" {
		t.Errorf("status = %v after refused ship attempts", got)
	}
	if rec := f.doAnon(t, "POST", "/orders/"+id+"/ship", "", "Authorization", "Bearer "+testKey); rec.Code != 200 {
		t.Errorf("ship with the key: %d %s", rec.Code, rec.Body)
	}
}

// Placing, reading and cancelling are the customer's own actions. There are no
// customer logins yet, so they take no key; the known-limits table says so.
func TestCustomerEndpointsNeedNoKey(t *testing.T) {
	f := newFixture(t)
	rec := f.doAnon(t, "POST", "/orders", goodOrder)
	if rec.Code != 201 {
		t.Fatalf("place: %d %s", rec.Code, rec.Body)
	}
	id := asMap(t, rec)["id"].(string)
	for _, path := range []string{"/healthz", "/metrics", "/orders", "/orders/" + id} {
		if r := f.doAnon(t, "GET", path, ""); r.Code != 200 {
			t.Errorf("GET %s: %d", path, r.Code)
		}
	}
	if r := f.doAnon(t, "POST", "/orders/"+id+"/cancel", ""); r.Code != 200 {
		t.Errorf("cancel: %d", r.Code)
	}
}

func TestWithoutAKeyConfiguredAdminFailsClosed(t *testing.T) {
	store, _ := eventstore.OpenFile("")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pay := &stubPayments{charged: map[string]bool{}, voided: map[string]bool{}}
	svc, _ := service.New(store, &stubProducts{held: map[string]bool{}}, pay, log, service.WithGrace(time.Hour))
	f := &fixture{h: New(svc, log).Handler()} // no WithAdminKey
	for _, hdr := range [][]string{nil, {"Authorization", "Bearer "}, {"Authorization", "Bearer anything"}} {
		wantProblem(t, f.doAnon(t, "POST", "/orders/ord_1/ship", "", hdr...), 403, "admin-disabled")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f := newFixture(t)
	a := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)
	f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"tok_declined","items":[{"product_id":"prd_a","quantity":1}]}`)
	f.pay.down = true
	f.do(t, "POST", "/orders", goodOrder)
	f.pay.down = false
	f.do(t, "GET", "/orders/"+a, "")
	f.do(t, "GET", "/orders/ord_nope", "")
	f.doAnon(t, "POST", "/orders/"+a+"/ship", "")
	f.do(t, "GET", "/no/such/route", "")

	rec := f.do(t, "GET", "/metrics", "")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	text := rec.Body.String()
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		if !sampleLine.MatchString(line) {
			t.Errorf("not valid exposition format: %q", line)
		}
	}
	want := []string{
		`gocart_http_requests_total{code="201",method="POST",route="POST /orders",service="order-service"} 1`,
		`gocart_http_requests_total{code="402",method="POST",route="POST /orders",service="order-service"} 1`,
		`gocart_http_requests_total{code="202",method="POST",route="POST /orders",service="order-service"} 1`,
		`gocart_http_requests_total{code="404",method="GET",route="GET /orders/{id}",service="order-service"} 1`,
		`gocart_http_requests_total{code="401",method="POST",route="POST /orders/{id}/ship",service="order-service"} 1`,
		`gocart_admin_auth_failures_total{service="order-service"} 1`,
		`gocart_orders{service="order-service",status="confirmed"} 1`,
		`gocart_orders{service="order-service",status="payment_failed"} 1`,
		`gocart_orders{service="order-service",status="awaiting_payment"} 1`,
		`gocart_orders_pending_work{kind="awaiting_payment",service="order-service"} 1`,
		`gocart_orders_pending_work{kind="refund",service="order-service"} 0`,
	}
	for _, w := range want {
		if !strings.Contains(text, w+"\n") {
			t.Errorf("metrics lack %s\n%s", w, text)
		}
	}
	for _, leak := range []string{a, "ord_nope", "tok_", "cust_1", "/no/such/route"} {
		if strings.Contains(text, leak) {
			t.Errorf("a raw path, id or token leaked into the metrics: %s", leak)
		}
	}
}

func TestMetricsBucketsAreCumulative(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.do(t, "GET", "/healthz", "")
	}
	body := f.do(t, "GET", "/metrics", "").Body.String()
	prev := -1
	re := regexp.MustCompile(`gocart_http_request_duration_seconds_bucket\{le="[^"]+",route="GET /healthz",service="order-service"\} (\d+)`)
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
