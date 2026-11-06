package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/eventstore"
	"gocart/order-service/internal/paymentclient"
	"gocart/order-service/internal/productclient"
	"gocart/order-service/internal/service"
)

// stubProducts is a Product service whose behaviour the test controls.
type stubProducts struct {
	mu       sync.Mutex
	down     bool
	reserves int
	held     map[string]bool
}

func (s *stubProducts) Reserve(_ context.Context, id string, items []domain.Item) (productclient.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserves++
	if s.down {
		return productclient.Reservation{}, fmt.Errorf("%w: down", productclient.ErrUnavailable)
	}
	for _, it := range items {
		switch it.ProductID {
		case "prd_gone":
			return productclient.Reservation{}, &productclient.RejectedError{
				Reason: productclient.ReasonInsufficientStock, Message: "prd_gone: wanted 1, only 0 available"}
		case "prd_ghost":
			return productclient.Reservation{}, &productclient.RejectedError{
				Reason: productclient.ReasonUnknownProduct, Message: "unknown product(s): prd_ghost"}
		}
	}
	s.held[id] = true
	res := productclient.Reservation{OrderID: id, Currency: "INR"}
	for _, it := range items {
		res.Items = append(res.Items, domain.PricedItem{ProductID: it.ProductID, Name: "N", Quantity: it.Quantity, UnitPriceMinor: 2500})
	}
	return res, nil
}

func (s *stubProducts) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return fmt.Errorf("%w: down", productclient.ErrUnavailable)
	}
	delete(s.held, id)
	return nil
}

// stubPayments is a Payment service with the same contract as the real one.
type stubPayments struct {
	mu      sync.Mutex
	down    bool
	charged map[string]bool
	voided  map[string]bool
}

func (s *stubPayments) Charge(_ context.Context, id string, _ int64, _, token string) (paymentclient.Charge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return paymentclient.Charge{}, fmt.Errorf("%w: down", paymentclient.ErrUnavailable)
	}
	if s.voided[id] {
		return paymentclient.Charge{}, paymentclient.ErrVoided
	}
	if token == "tok_declined" {
		return paymentclient.Charge{}, &paymentclient.DeclinedError{Code: "insufficient_funds"}
	}
	s.charged[id] = true
	return paymentclient.Charge{OrderID: id, Reference: "pay_" + id}, nil
}

func (s *stubPayments) Cancel(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return fmt.Errorf("%w: down", paymentclient.ErrUnavailable)
	}
	delete(s.charged, id)
	s.voided[id] = true
	return nil
}

type fixture struct {
	srv  *Server
	h    http.Handler
	stub *stubProducts
	pay  *stubPayments
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, _ := eventstore.OpenFile("")
	stub := &stubProducts{held: map[string]bool{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pay := &stubPayments{charged: map[string]bool{}, voided: map[string]bool{}}
	svc, err := service.New(store, stub, pay, log, service.WithGrace(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(svc, log, WithAdminKey(testKey))
	return &fixture{srv: srv, h: srv.Handler(), stub: stub, pay: pay}
}

const testKey = "test-admin-key"

// do sends a request carrying the admin key; doAnon sends it without one.
func (f *fixture) do(t *testing.T, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doAs(t, method, path, body, append([]string{"Authorization", "Bearer " + testKey}, hdr...)...)
}

func (f *fixture) doAnon(t *testing.T, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doAs(t, method, path, body, hdr...)
}

func (f *fixture) doAs(t *testing.T, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func asMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rec.Body)
	}
	return m
}

func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d\n%s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	m := asMap(t, rec)
	if m["type"] != "urn:gocart:problem:"+typ {
		t.Errorf("problem type = %v, want %s", m["type"], typ)
	}
	return m
}

const goodOrder = `{"customer_id":"cust_1","payment_token":"tok_visa","items":[{"product_id":"prd_a","quantity":2}]}`

func TestPlaceConfirmedIs201WithLocation(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, "POST", "/orders", goodOrder)
	if rec.Code != 201 {
		t.Fatalf("status %d\n%s", rec.Code, rec.Body)
	}
	o := asMap(t, rec)
	if o["status"] != "confirmed" || o["total_minor"].(float64) != 5000 {
		t.Errorf("order = %v", o)
	}
	if loc := rec.Header().Get("Location"); loc != "/orders/"+o["id"].(string) {
		t.Errorf("Location = %q", loc)
	}
}

func TestPlaceRejectedIs409ButTheOrderIsStillRecorded(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"tok_visa","items":[{"product_id":"prd_gone","quantity":1}]}`)
	p := wantProblem(t, rec, 409, "order-rejected")
	if p["reason"] != "insufficient_stock" || !strings.Contains(p["detail"].(string), "prd_gone") {
		t.Errorf("problem = %v", p)
	}
	id := p["order_id"].(string)
	got := f.do(t, "GET", "/orders/"+id, "")
	if got.Code != 200 || asMap(t, got)["status"] != "rejected" {
		t.Errorf("rejected order not retrievable: %d %s", got.Code, got.Body)
	}
}

func TestPlaceDuringOutageIs202AndSettlesLater(t *testing.T) {
	f := newFixture(t)
	f.stub.down = true
	rec := f.do(t, "POST", "/orders", goodOrder)
	if rec.Code != 202 {
		t.Fatalf("status %d, want 202\n%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("Location") == "" {
		t.Errorf("202 needs Location and Retry-After: %v", rec.Header())
	}
	if asMap(t, rec)["status"] != "placed" {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestPlaceDuringPaymentOutageIs202AwaitingPayment(t *testing.T) {
	f := newFixture(t)
	f.pay.down = true
	rec := f.do(t, "POST", "/orders", goodOrder)
	if rec.Code != 202 {
		t.Fatalf("status %d, want 202\n%s", rec.Code, rec.Body)
	}
	if asMap(t, rec)["status"] != "awaiting_payment" || rec.Header().Get("Location") == "" {
		t.Errorf("body = %s headers = %v", rec.Body, rec.Header())
	}
}

func TestDeclinedCardIs402ButTheOrderIsStillRecorded(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"tok_declined","items":[{"product_id":"prd_a","quantity":1}]}`)
	p := wantProblem(t, rec, 402, "payment-declined")
	if p["decline_code"] != "insufficient_funds" {
		t.Errorf("problem = %v", p)
	}
	id := p["order_id"].(string)
	got := asMap(t, f.do(t, "GET", "/orders/"+id, ""))
	if got["status"] != "payment_failed" || got["decline_code"] != "insufficient_funds" {
		t.Errorf("order = %v", got)
	}
	if got["stock_release_pending"] == true {
		t.Error("the stock should already be back")
	}
	// Repeating the request with the same key gives the same answer.
	body := `{"customer_id":"c","payment_token":"tok_declined","items":[{"product_id":"prd_a","quantity":1}]}`
	first := f.do(t, "POST", "/orders", body, "Idempotency-Key", "k-d")
	again := f.do(t, "POST", "/orders", body, "Idempotency-Key", "k-d")
	if first.Code != 402 || again.Code != 402 {
		t.Errorf("statuses %d, %d, want 402 both times", first.Code, again.Code)
	}
}

func TestPaymentTokenNeverAppearsInAResponse(t *testing.T) {
	f := newFixture(t)
	id := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)
	for _, path := range []string{"/orders", "/orders/" + id, "/orders/" + id + "/events"} {
		if body := f.do(t, "GET", path, "").Body.String(); strings.Contains(body, "tok_") {
			t.Errorf("GET %s leaked the payment token: %s", path, body)
		}
	}
}

func TestIdempotentRetryIs200AndDoesNotCreateASecondOrder(t *testing.T) {
	f := newFixture(t)
	first := f.do(t, "POST", "/orders", goodOrder, "Idempotency-Key", "k-1")
	again := f.do(t, "POST", "/orders", goodOrder, "Idempotency-Key", "k-1")
	if first.Code != 201 || again.Code != 200 {
		t.Fatalf("statuses %d then %d, want 201 then 200", first.Code, again.Code)
	}
	if asMap(t, first)["id"] != asMap(t, again)["id"] {
		t.Error("retry produced a different order")
	}
	list := asMap(t, f.do(t, "GET", "/orders", ""))["items"].([]any)
	if len(list) != 1 {
		t.Errorf("%d orders exist, want 1", len(list))
	}
	wantProblem(t, f.do(t, "POST", "/orders",
		`{"customer_id":"cust_1","payment_token":"tok_visa","items":[{"product_id":"prd_a","quantity":9}]}`, "Idempotency-Key", "k-1"),
		422, "idempotency-key-reuse")
	wantProblem(t, f.do(t, "POST", "/orders", goodOrder, "Idempotency-Key", "bad key!"), 400, "invalid-request")
}

func TestPlaceValidationAndStrictParsing(t *testing.T) {
	f := newFixture(t)
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"","payment_token":"tok_visa","items":[{"product_id":"a","quantity":1}]}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"tok_visa","items":[]}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"tok_visa","items":[{"product_id":"a","quantity":0}]}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"tok_visa","items":[{"product_id":"a","qty":1}]}`), 400, "invalid-request")
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"tok_visa","items":[{"product_id":"a","quantity":1}],"coupon":"X"}`), 400, "invalid-request")
	wantProblem(t, f.do(t, "POST", "/orders", `please send me a keyboard`), 400, "invalid-request")
	wantProblem(t, f.do(t, "POST", "/orders", ``), 400, "invalid-request")
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"c","items":[{"product_id":"a","quantity":1}]}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"c","payment_token":"4242424242424242","items":[{"product_id":"a","quantity":1}]}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "POST", "/orders", `{"customer_id":"`+strings.Repeat("x", 2<<20)+`"}`), 413, "payload-too-large")
	if f.stub.reserves != 0 || len(f.pay.charged) != 0 {
		t.Error("another service was called for an invalid request")
	}
}

func TestQueryEndpoints(t *testing.T) {
	f := newFixture(t)
	a := asMap(t, f.do(t, "POST", "/orders", `{"customer_id":"alice","payment_token":"tok_visa","items":[{"product_id":"prd_a","quantity":1}]}`))["id"].(string)
	f.do(t, "POST", "/orders", `{"customer_id":"bob","payment_token":"tok_visa","items":[{"product_id":"prd_a","quantity":1}]}`)
	f.do(t, "POST", "/orders", `{"customer_id":"alice","payment_token":"tok_visa","items":[{"product_id":"prd_gone","quantity":1}]}`)
	f.do(t, "POST", "/orders/"+a+"/ship", "")

	count := func(q string) int {
		rec := f.do(t, "GET", "/orders"+q, "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
		return len(asMap(t, rec)["items"].([]any))
	}
	if n := count(""); n != 3 {
		t.Errorf("all = %d", n)
	}
	if n := count("?customer_id=alice"); n != 2 {
		t.Errorf("alice = %d", n)
	}
	if n := count("?customer_id=alice&status=shipped"); n != 1 {
		t.Errorf("alice shipped = %d", n)
	}
	if n := count("?status=rejected"); n != 1 {
		t.Errorf("rejected = %d", n)
	}
	if n := count("?limit=1"); n != 1 {
		t.Errorf("limit 1 = %d", n)
	}
	wantProblem(t, f.do(t, "GET", "/orders?status=bogus", ""), 422, "validation-failed")
	wantProblem(t, f.do(t, "GET", "/orders?limit=0", ""), 422, "validation-failed")
	wantProblem(t, f.do(t, "GET", "/orders?limit=9999", ""), 422, "validation-failed")
	wantProblem(t, f.do(t, "GET", "/orders?limit=abc", ""), 422, "validation-failed")
	wantProblem(t, f.do(t, "GET", "/orders/ord_nope", ""), 404, "not-found")
}

func TestEventsEndpointShowsTheLog(t *testing.T) {
	f := newFixture(t)
	id := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)
	f.do(t, "POST", "/orders/"+id+"/cancel", `{"reason":"too slow"}`)

	rec := f.do(t, "GET", "/orders/"+id+"/events", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var types []string
	for i, e := range asMap(t, rec)["events"].([]any) {
		m := e.(map[string]any)
		if int(m["version"].(float64)) != i+1 {
			t.Errorf("event %d has version %v", i, m["version"])
		}
		types = append(types, m["type"].(string))
	}
	if got := strings.Join(types, ","); got != "OrderPlaced,StockReserved,PaymentCaptured,OrderCancelled,StockReleased,PaymentRefunded" {
		t.Errorf("history = %s", got)
	}
	wantProblem(t, f.do(t, "GET", "/orders/ord_nope/events", ""), 404, "not-found")
}

func TestCancelAndShipLifecycle(t *testing.T) {
	f := newFixture(t)
	id := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)

	// An empty body is fine for cancel; a bad one is not.
	wantProblem(t, f.do(t, "POST", "/orders/"+id+"/cancel", `{"why":"x"}`), 400, "invalid-request")
	wantProblem(t, f.do(t, "POST", "/orders/"+id+"/cancel", `{"reason":"`+strings.Repeat("r", 201)+`"}`), 422, "validation-failed")

	rec := f.do(t, "POST", "/orders/"+id+"/cancel", "")
	if rec.Code != 200 || asMap(t, rec)["status"] != "cancelled" {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	wantProblem(t, f.do(t, "POST", "/orders/"+id+"/cancel", ""), 409, "invalid-state")
	wantProblem(t, f.do(t, "POST", "/orders/"+id+"/ship", ""), 409, "invalid-state")
	wantProblem(t, f.do(t, "POST", "/orders/ord_nope/cancel", ""), 404, "not-found")
	wantProblem(t, f.do(t, "POST", "/orders/ord_nope/ship", ""), 404, "not-found")

	other := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)
	if rec := f.do(t, "POST", "/orders/"+other+"/ship", ""); rec.Code != 200 || asMap(t, rec)["status"] != "shipped" {
		t.Errorf("ship: %d %s", rec.Code, rec.Body)
	}
	wantProblem(t, f.do(t, "POST", "/orders/"+other+"/cancel", ""), 409, "invalid-state")

	// Delivery: only a shipped order, once, and then it is final.
	wantProblem(t, f.do(t, "POST", "/orders/"+id+"/deliver", ""), 409, "invalid-state")
	wantProblem(t, f.do(t, "POST", "/orders/ord_nope/deliver", ""), 404, "not-found")
	if rec := f.do(t, "POST", "/orders/"+other+"/deliver", ""); rec.Code != 200 || asMap(t, rec)["status"] != "delivered" {
		t.Errorf("deliver: %d %s", rec.Code, rec.Body)
	}
	wantProblem(t, f.do(t, "POST", "/orders/"+other+"/deliver", ""), 409, "invalid-state")
	wantProblem(t, f.do(t, "POST", "/orders/"+other+"/cancel", ""), 409, "invalid-state")
}

func TestCancelReportsThePendingRefund(t *testing.T) {
	f := newFixture(t)
	id := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)
	f.pay.down = true
	o := asMap(t, f.do(t, "POST", "/orders/"+id+"/cancel", ""))
	if o["status"] != "cancelled" || o["refund_pending"] != true || o["stock_release_pending"] == true {
		t.Errorf("order = %v", o)
	}
}

func TestFilterAcceptsEveryStatus(t *testing.T) {
	f := newFixture(t)
	for _, st := range domain.AllStatuses {
		if rec := f.do(t, "GET", "/orders?status="+string(st), ""); rec.Code != 200 {
			t.Errorf("status=%s: %d", st, rec.Code)
		}
	}
}

func TestCancelDuringOutageReportsThePendingRelease(t *testing.T) {
	f := newFixture(t)
	id := asMap(t, f.do(t, "POST", "/orders", goodOrder))["id"].(string)
	f.stub.down = true
	rec := f.do(t, "POST", "/orders/"+id+"/cancel", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	o := asMap(t, rec)
	if o["status"] != "cancelled" || o["stock_release_pending"] != true {
		t.Errorf("order = %v", o)
	}
}

func TestRequestIDIsEchoedAndSanitised(t *testing.T) {
	f := newFixture(t)
	rec := f.do(t, "GET", "/orders/ord_nope", "", "X-Request-ID", "trace-abc.123")
	if rec.Header().Get("X-Request-ID") != "trace-abc.123" || asMap(t, rec)["request_id"] != "trace-abc.123" {
		t.Errorf("not echoed: header=%q body=%s", rec.Header().Get("X-Request-ID"), rec.Body)
	}
	rec = f.do(t, "GET", "/healthz", "", "X-Request-ID", "bad id\nwith newline")
	if got := rec.Header().Get("X-Request-ID"); got == "" || strings.ContainsAny(got, " \n") {
		t.Errorf("unsafe id not replaced: %q", got)
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(t, "DELETE", "/orders/ord_1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /orders/{id}: %d, want 405", rec.Code)
	}
	if rec := f.do(t, "GET", "/orders/ord_1/cancel", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /orders/{id}/cancel: %d, want 405", rec.Code)
	}
}

// TestRoutesMatchOpenAPISpec keeps code and docs/openapi/order.yaml honest in
// both directions: an endpoint only in code, or only in the spec, fails.
func TestRoutesMatchOpenAPISpec(t *testing.T) {
	f := newFixture(t)
	want := specOperations(t, filepath.Join("..", "..", "..", "docs", "openapi", "order.yaml"))
	got := f.srv.Routes()
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("router and spec disagree\n code: %v\n spec: %v", got, want)
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
	inPaths := false
	path := ""
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, " ") && line != "" {
			inPaths = line == "paths:"
			path = ""
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
	if len(ops) == 0 {
		t.Fatal("found no operations in spec; has its layout changed?")
	}
	sort.Strings(ops)
	return ops
}
