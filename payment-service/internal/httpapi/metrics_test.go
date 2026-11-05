package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"gocart/payment-service/internal/gateway"
	"gocart/payment-service/internal/ledger"
)

var sampleLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-z_]+="([^"\\]|\\.)*",?)*\})? [0-9.e+-]+$`)

func TestAdminEndpointsNeedTheKey(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100))

	rec := doAnon(t, h, "GET", "/charges/ord_1", "")
	wantProblem(t, rec, 401, "unauthorized")
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("a 401 must say how to authenticate")
	}
	wantProblem(t, doAnon(t, h, "GET", "/charges/ord_1", "", "Authorization", "Bearer wrong"), 401, "unauthorized")
	wantProblem(t, doAnon(t, h, "GET", "/charges/ord_1", "", "Authorization", "Basic "+testKey), 401, "unauthorized")
	if rec := doAnon(t, h, "GET", "/charges/ord_1", "", "Authorization", "Bearer "+testKey); rec.Code != 200 {
		t.Fatalf("with the key: %d %s", rec.Code, rec.Body)
	}
}

// Charging and cancelling are service-to-service calls, not admin ones: the
// Order service makes them and holds no admin key.
func TestServiceToServiceEndpointsNeedNoKey(t *testing.T) {
	_, h := newTestServer(t)
	if rec := doAnon(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100)); rec.Code != http.StatusCreated {
		t.Errorf("POST /charges: %d", rec.Code)
	}
	if rec := doAnon(t, h, "DELETE", "/charges/ord_1", ""); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE /charges/{id}: %d", rec.Code)
	}
	for _, path := range []string{"/healthz", "/metrics"} {
		if rec := doAnon(t, h, "GET", path, ""); rec.Code != 200 {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}
}

func TestWithoutAKeyConfiguredAdminFailsClosed(t *testing.T) {
	l, _ := ledger.Open(t.TempDir(), gateway.New(time.Millisecond))
	h := New(l, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
	for _, hdr := range [][]string{nil, {"Authorization", "Bearer "}, {"Authorization", "Bearer anything"}} {
		wantProblem(t, doAnon(t, h, "GET", "/charges/ord_1", "", hdr...), 403, "admin-disabled")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100))
	do(t, h, "POST", "/charges", body("ord_2", "tok_declined", 100))
	do(t, h, "POST", "/charges", body("ord_3", "tok_flaky", 100))
	do(t, h, "DELETE", "/charges/ord_1", "")
	doAnon(t, h, "GET", "/charges/ord_1", "")
	do(t, h, "GET", "/no/such/route", "")

	rec := do(t, h, "GET", "/metrics", "")
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
		`gocart_http_requests_total{code="201",method="POST",route="POST /charges",service="payment-service"} 1`,
		`gocart_http_requests_total{code="402",method="POST",route="POST /charges",service="payment-service"} 1`,
		`gocart_http_requests_total{code="503",method="POST",route="POST /charges",service="payment-service"} 1`,
		`gocart_http_requests_total{code="204",method="DELETE",route="DELETE /charges/{order_id}",service="payment-service"} 1`,
		`gocart_http_requests_total{code="404",method="GET",route="unmatched",service="payment-service"} 1`,
		`gocart_admin_auth_failures_total{service="payment-service"} 1`,
		`gocart_charges{service="payment-service",status="declined"} 1`,
		`gocart_charges{service="payment-service",status="voided"} 1`,
		`gocart_charges{service="payment-service",status="approved"} 0`,
	}
	for _, w := range want {
		if !strings.Contains(text, w+"\n") {
			t.Errorf("metrics lack %s\n%s", w, text)
		}
	}
	for _, leak := range []string{"ord_1", "ord_2", "tok_", "/no/such/route"} {
		if strings.Contains(text, leak) {
			t.Errorf("a raw path or token leaked into the metrics: %s", leak)
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
	re := regexp.MustCompile(`gocart_http_request_duration_seconds_bucket\{le="[^"]+",route="GET /healthz",service="payment-service"\} (\d+)`)
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
