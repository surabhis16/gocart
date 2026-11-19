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

	"gocart/cart-service/internal/cart"
	"gocart/cart-service/internal/identity"
	"gocart/cart-service/internal/orderclient"
	"gocart/cart-service/internal/productclient"
	"gocart/cart-service/internal/wishlist"
)

// fakeCatalog knows a fixed set of products and can be taken down.
type fakeCatalog struct {
	mu    sync.Mutex
	known map[string]string
	down  bool
	calls int
}

func (f *fakeCatalog) Name(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		return "", productclient.ErrUnavailable
	}
	if n, ok := f.known[id]; ok {
		return n, nil
	}
	return "", productclient.ErrUnknown
}

// fakeOrders behaves like the Order service where it matters here: one order
// per Idempotency-Key, whatever happens to the answer.
type fakeOrders struct {
	mu       sync.Mutex
	byKey    map[string]orderclient.Response
	keys     []string // every key received, in order
	next     int
	decline  bool // answer 402 (and record it, like the real service does)
	lose     bool // create the order but fail to answer
	pending  bool // answer 202
	lastBody []cart.Line
}

func (f *fakeOrders) Place(_ context.Context, key, customer, token string, lines []cart.Line) (orderclient.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	f.lastBody = lines
	if r, ok := f.byKey[key]; ok {
		if r.Status == 201 {
			r.Status = 200 // a replay
		}
		return r, nil
	}
	f.next++
	var r orderclient.Response
	switch {
	case f.decline:
		r = orderclient.Response{Status: 402, Body: []byte(fmt.Sprintf(`{"type":"urn:gocart:problem:payment-declined","status":402,"order_id":"ord_%d"}`, f.next))}
	case f.pending:
		r = orderclient.Response{Status: 202, Body: []byte(fmt.Sprintf(`{"id":"ord_%d","status":"awaiting_payment"}`, f.next))}
	default:
		r = orderclient.Response{Status: 201, Body: []byte(fmt.Sprintf(`{"id":"ord_%d","status":"confirmed","customer_id":%q}`, f.next, customer))}
	}
	f.byKey[key] = r
	if f.lose {
		f.lose = false
		return orderclient.Response{}, fmt.Errorf("%w: connection reset", orderclient.ErrUnavailable)
	}
	return r, nil
}

type fixture struct {
	srv      *Server
	h        http.Handler
	store    *cart.Store
	wishlist *wishlist.Store
	catalog  *fakeCatalog
	orders   *fakeOrders
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := cart.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	wishlistStore, err := wishlist.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		store:    store,
		wishlist: wishlistStore,
		catalog:  &fakeCatalog{known: map[string]string{"prd_lamp": "Desk lamp", "prd_cable": "Cable"}},
		orders:   &fakeOrders{byKey: map[string]orderclient.Response{}},
	}
	f.srv = New(store, f.catalog, f.orders, slog.New(slog.NewTextHandler(io.Discard, nil)), WithWishlist(wishlistStore), WithVersion("test"))
	f.h = f.srv.Handler()
	return f
}

func (f *fixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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

func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d\n%s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := asMap(t, rec)["type"]; got != "urn:gocart:problem:"+typ {
		t.Errorf("problem type = %v, want %s", got, typ)
	}
}

func (f *fixture) add(t *testing.T, customer, product string, qty int) {
	t.Helper()
	rec := f.do(t, "PUT", fmt.Sprintf("/carts/%s/items/%s", customer, product), fmt.Sprintf(`{"quantity":%d}`, qty))
	if rec.Code != 200 {
		t.Fatalf("add %s: %d %s", product, rec.Code, rec.Body)
	}
}

const pay = `{"payment_token":"tok_visa"}`

func TestAddingItemsBuildsTheCart(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(t, "GET", "/carts/cust_1", ""); rec.Code != 200 || len(asMap(t, rec)["items"].([]any)) != 0 {
		t.Fatalf("a new customer has an empty cart: %d %s", rec.Code, rec.Body)
	}
	f.add(t, "cust_1", "prd_lamp", 2)
	f.add(t, "cust_1", "prd_cable", 1)
	rec := f.do(t, "GET", "/carts/cust_1", "")
	items := asMap(t, rec)["items"].([]any)
	if len(items) != 2 || items[1].(map[string]any)["name"] != "Desk lamp" || items[1].(map[string]any)["quantity"] != float64(2) {
		t.Fatalf("%s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "price") {
		t.Error("a cart must not carry prices; the Product service owns them")
	}
}

func TestOnlyRealProductsCanBeAdded(t *testing.T) {
	f := newFixture(t)
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_ghost", `{"quantity":1}`), 422, "unknown-product")
	if len(f.store.Get("c").Items) != 0 {
		t.Error("an unknown product must not be added")
	}
}

func TestProductOutageIsNotMistakenForAnUnknownProduct(t *testing.T) {
	f := newFixture(t)
	f.catalog.down = true
	rec := f.do(t, "PUT", "/carts/c/items/prd_lamp", `{"quantity":1}`)
	wantProblem(t, rec, 503, "product-service-unavailable")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 503 should say when to retry")
	}
}

func TestQuantityChangesNeedNoProductLookup(t *testing.T) {
	f := newFixture(t)
	f.add(t, "c", "prd_lamp", 1)
	f.catalog.down = true
	calls := f.catalog.calls
	f.add(t, "c", "prd_lamp", 4)
	if f.catalog.calls != calls {
		t.Error("changing a quantity must not call the Product service")
	}
	if got := f.store.Get("c").Items[0]; got.Quantity != 4 || got.Name != "Desk lamp" {
		t.Errorf("%+v: the name copied when the line was added must survive", got)
	}
}

func TestValidationAndMalformedBodies(t *testing.T) {
	f := newFixture(t)
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_lamp", `{"quantity":0}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_lamp", `{"quantity":101}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_lamp", `{"qty":1}`), 400, "invalid-request")
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_lamp", `{"quantity":1} {}`), 400, "invalid-request")
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_lamp", ``), 400, "invalid-request")
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_lamp", `{"quantity":"two"}`), 400, "invalid-request")
	wantProblem(t, f.do(t, "GET", "/carts/bad%20id", ""), 422, "validation-failed")
	wantProblem(t, f.do(t, "PUT", "/carts/bad%20id/items/prd_lamp", `{"quantity":1}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "POST", "/carts/bad%20id/checkout", pay), 422, "validation-failed")
	wantProblem(t, f.do(t, "PUT", "/carts/c/items/prd_lamp", `{"quantity":1,"x":`+strings.Repeat("1", 2<<20)+`}`), 413, "payload-too-large")
}

func TestRemoveAndClear(t *testing.T) {
	f := newFixture(t)
	f.add(t, "c", "prd_lamp", 1)
	f.add(t, "c", "prd_cable", 1)
	for i := 0; i < 2; i++ { // twice: removing something already gone is fine
		if rec := f.do(t, "DELETE", "/carts/c/items/prd_lamp", ""); rec.Code != 200 || len(asMap(t, rec)["items"].([]any)) != 1 {
			t.Fatalf("remove #%d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	for i := 0; i < 2; i++ {
		if rec := f.do(t, "DELETE", "/carts/c", ""); rec.Code != 204 || rec.Body.Len() != 0 {
			t.Fatalf("clear #%d: %d %q", i+1, rec.Code, rec.Body)
		}
	}
	if len(f.store.Get("c").Items) != 0 {
		t.Error("cart not empty")
	}
}

func TestCheckoutCreatesAnOrderAndPassesItThrough(t *testing.T) {
	f := newFixture(t)
	f.add(t, "cust_1", "prd_lamp", 2)
	rev := f.store.Get("cust_1").Revision

	rec := f.do(t, "POST", "/carts/cust_1/checkout", pay)
	if rec.Code != 201 || asMap(t, rec)["id"] != "ord_1" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if f.orders.keys[0] != cart.CheckoutKey("cust_1", rev) {
		t.Errorf("key = %q", f.orders.keys[0])
	}
	if len(f.orders.lastBody) != 1 || f.orders.lastBody[0].Quantity != 2 {
		t.Errorf("the order must carry the cart's lines: %+v", f.orders.lastBody)
	}
	got := asMap(t, f.do(t, "GET", "/carts/cust_1", ""))
	if got["order_id"] != "ord_1" {
		t.Errorf("the cart must say which order it became: %v", got)
	}
}

func TestCheckoutOfAnEmptyCartOrWithoutATokenIsRefusedBeforeCallingOut(t *testing.T) {
	f := newFixture(t)
	wantProblem(t, f.do(t, "POST", "/carts/c/checkout", pay), 409, "cart-empty")
	f.add(t, "c", "prd_lamp", 1)
	wantProblem(t, f.do(t, "POST", "/carts/c/checkout", `{}`), 422, "validation-failed")
	wantProblem(t, f.do(t, "POST", "/carts/c/checkout", `{"payment_token":"tok_visa","amount":1}`), 400, "invalid-request")
	if len(f.orders.keys) != 0 {
		t.Errorf("the Order service must not be called: %v", f.orders.keys)
	}
}

// The central property. The Order service did not answer; the customer clicks
// again. There must be exactly one order, and the cart must not have changed.
func TestRetryAfterNoAnswerCannotCreateASecondOrder(t *testing.T) {
	f := newFixture(t)
	f.add(t, "c", "prd_lamp", 1)
	f.orders.lose = true // the order is created, but the answer never arrives

	first := f.do(t, "POST", "/carts/c/checkout", pay)
	wantProblem(t, first, 503, "checkout-unavailable")
	if first.Header().Get("Retry-After") == "" {
		t.Error("a 503 should say when to retry")
	}
	if c := f.store.Get("c"); c.CheckedOut() || len(c.Items) != 1 {
		t.Fatalf("with no answer the cart must be left exactly as it was: %+v", c)
	}

	second := f.do(t, "POST", "/carts/c/checkout", pay)
	if second.Code != 200 || asMap(t, second)["id"] != "ord_1" {
		t.Fatalf("the retry must find the order that was made: %d %s", second.Code, second.Body)
	}
	if f.orders.keys[0] != f.orders.keys[1] {
		t.Errorf("both attempts must send the same key: %v", f.orders.keys)
	}
	if len(f.orders.byKey) != 1 {
		t.Errorf("%d orders exist, want 1", len(f.orders.byKey))
	}
}

func TestRepeatedCheckoutAfterSuccessReplaysTheSameOrder(t *testing.T) {
	f := newFixture(t)
	f.add(t, "c", "prd_lamp", 1)
	a := f.do(t, "POST", "/carts/c/checkout", pay)
	b := f.do(t, "POST", "/carts/c/checkout", pay) // the first response was lost on its way back
	if a.Code != 201 || b.Code != 200 || asMap(t, a)["id"] != asMap(t, b)["id"] || len(f.orders.byKey) != 1 {
		t.Fatalf("%d %s / %d %s, %d orders", a.Code, a.Body, b.Code, b.Body, len(f.orders.byKey))
	}
}

func TestADeclinedCardKeepsTheCartAndTheNextAttemptGetsANewKey(t *testing.T) {
	f := newFixture(t)
	f.add(t, "c", "prd_lamp", 3)
	f.orders.decline = true

	rec := f.do(t, "POST", "/carts/c/checkout", `{"payment_token":"tok_declined"}`)
	wantProblem(t, rec, 402, "payment-declined")
	if asMap(t, rec)["order_id"] != "ord_1" {
		t.Errorf("the Order service's problem must arrive intact: %s", rec.Body)
	}
	c := f.store.Get("c")
	if c.CheckedOut() || len(c.Items) != 1 || c.Items[0].Quantity != 3 {
		t.Fatalf("a declined card must leave the cart ready for another try: %+v", c)
	}

	f.orders.decline = false
	ok := f.do(t, "POST", "/carts/c/checkout", pay)
	if ok.Code != 201 || asMap(t, ok)["id"] != "ord_2" {
		t.Fatalf("a new card on the same cart must make a new order, not replay the refusal: %d %s", ok.Code, ok.Body)
	}
	if f.orders.keys[0] == f.orders.keys[1] {
		t.Error("the second attempt reused the refused attempt's key")
	}
}

func TestAnOrderStillSettlingCountsAsPlaced(t *testing.T) {
	f := newFixture(t)
	f.add(t, "c", "prd_lamp", 1)
	f.orders.pending = true
	rec := f.do(t, "POST", "/carts/c/checkout", pay)
	if rec.Code != 202 || !f.store.Get("c").CheckedOut() {
		t.Fatalf("202 means the order exists and will settle; the cart must not be offered for a second checkout: %d %s", rec.Code, rec.Body)
	}
}

func TestChangingACheckedOutCartStartsAFreshOne(t *testing.T) {
	f := newFixture(t)
	f.add(t, "c", "prd_lamp", 1)
	f.do(t, "POST", "/carts/c/checkout", pay)
	f.add(t, "c", "prd_cable", 2)
	c := f.store.Get("c")
	if c.CheckedOut() || len(c.Items) != 1 || c.Items[0].ProductID != "prd_cable" {
		t.Fatalf("%+v", c)
	}
	f.do(t, "POST", "/carts/c/checkout", pay)
	if len(f.orders.byKey) != 2 {
		t.Errorf("the second cart must become a second order; %d orders", len(f.orders.byKey))
	}
	wantProblem(t, f.do(t, "POST", "/carts/nobody/checkout", pay), 409, "cart-empty")
}

func TestOnlyTheSameCustomersCartIsOrdered(t *testing.T) {
	f := newFixture(t)
	f.add(t, "alice", "prd_lamp", 1)
	f.add(t, "bob", "prd_cable", 1)
	f.do(t, "POST", "/carts/alice/checkout", pay)
	if f.store.Get("bob").CheckedOut() || f.orders.keys[0] == cart.CheckoutKey("bob", f.store.Get("bob").Revision) {
		t.Error("checkout leaked across customers")
	}
}

func TestHealthAndMetrics(t *testing.T) {
	f := newFixture(t)
	f.add(t, "a", "prd_lamp", 1)
	f.add(t, "b", "prd_lamp", 1)
	f.do(t, "POST", "/carts/b/checkout", pay)
	if rec := f.do(t, "GET", "/healthz", ""); rec.Code != 200 {
		t.Errorf("healthz: %d", rec.Code)
	}
	rec := f.do(t, "GET", "/metrics", "")
	text := rec.Body.String()
	for _, want := range []string{
		`gocart_carts{service="cart-service",state="open"} 1`,
		`gocart_carts{service="cart-service",state="checked_out"} 1`,
		`gocart_build_info{service="cart-service",version="test"} 1`,
		`gocart_http_requests_total{code="201",method="POST",route="POST /carts/{customer_id}/checkout",service="cart-service"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s\n%s", want, text)
		}
	}
	line := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-z_]+="([^"\\]|\\.)*",?)*\})? [0-9.e+-]+$`)
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if !strings.HasPrefix(l, "#") && !line.MatchString(l) {
			t.Errorf("not valid exposition format: %q", l)
		}
	}
	if strings.Contains(text, "admin") {
		t.Error("the cart has no admin endpoints, so it must not export admin metrics")
	}
}

func TestWishlistEndpoints(t *testing.T) {
	f := newFixture(t)

	// 1. Empty wishlist retrieval
	rec := f.do(t, "GET", "/wishlists/cust_ada", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get empty wishlist: %d %s", rec.Code, rec.Body)
	}
	m := asMap(t, rec)
	if m["customer_id"] != "cust_ada" {
		t.Errorf("customer_id = %v", m["customer_id"])
	}
	items, ok := m["items"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("expected empty items array, got %v", m["items"])
	}

	// 2. Add item with explicit quantity
	rec = f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", `{"quantity":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add wishlist item: %d %s", rec.Code, rec.Body)
	}
	m = asMap(t, rec)
	items = m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	it := items[0].(map[string]any)
	if it["product_id"] != "prd_lamp" || int(it["quantity"].(float64)) != 3 {
		t.Errorf("unexpected item fields: %+v", it)
	}
	if _, hasName := it["name"]; hasName {
		t.Errorf("wishlist item MUST NOT contain name field: %+v", it)
	}

	// 3. Update quantity with empty body defaults to 1 (absolute SET)
	rec = f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("default quantity set: %d %s", rec.Code, rec.Body)
	}
	m = asMap(t, rec)
	it = m["items"].([]any)[0].(map[string]any)
	if int(it["quantity"].(float64)) != 1 {
		t.Errorf("expected default quantity 1, got %v", it["quantity"])
	}

	// 3b. JSON "null" body also defaults quantity to 1
	rec = f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", "null")
	if rec.Code != http.StatusOK {
		t.Fatalf("null body quantity set: %d %s", rec.Code, rec.Body)
	}
	m = asMap(t, rec)
	it = m["items"].([]any)[0].(map[string]any)
	if int(it["quantity"].(float64)) != 1 {
		t.Errorf("expected null body to give quantity 1, got %v", it["quantity"])
	}

	// 4. Unknown product returns 422
	rec = f.do(t, "PUT", "/wishlists/cust_ada/items/prd_unknown", `{"quantity":1}`)
	wantProblem(t, rec, http.StatusUnprocessableEntity, "unknown-product")

	// 4b. Invalid product ID format is rejected BEFORE calling Product Service
	callsBefore := f.catalog.calls
	rec = f.do(t, "PUT", "/wishlists/cust_ada/items/invalid%20id", `{"quantity":1}`)
	wantProblem(t, rec, http.StatusUnprocessableEntity, "validation-failed")
	if f.catalog.calls != callsBefore {
		t.Errorf("catalog should not be queried when product id is malformed; calls = %d, want %d", f.catalog.calls, callsBefore)
	}

	// 5. Product service down: existing item still updates, new item returns 503
	f.catalog.down = true
	rec = f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", `{"quantity":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update existing item while catalog down: %d %s", rec.Code, rec.Body)
	}
	it = asMap(t, rec)["items"].([]any)[0].(map[string]any)
	if int(it["quantity"].(float64)) != 4 {
		t.Errorf("quantity not updated while catalog down: %+v", it)
	}

	rec = f.do(t, "PUT", "/wishlists/cust_ada/items/prd_cable", `{"quantity":1}`)
	wantProblem(t, rec, http.StatusServiceUnavailable, "product-service-unavailable")
	f.catalog.down = false

	// 6. Validation failures
	wantProblem(t, f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", `{"quantity":0}`), http.StatusUnprocessableEntity, "validation-failed")
	wantProblem(t, f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", `{"quantity":101}`), http.StatusUnprocessableEntity, "validation-failed")
	wantProblem(t, f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", `{"quantity":"not-number"}`), http.StatusBadRequest, "invalid-request")
	wantProblem(t, f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", `{"unknown_field":true}`), http.StatusBadRequest, "invalid-request")
	wantProblem(t, f.do(t, "PUT", "/wishlists/bad@customer/items/prd_lamp", `{"quantity":1}`), http.StatusUnprocessableEntity, "validation-failed")

	// 7. Remove item
	rec = f.do(t, "DELETE", "/wishlists/cust_ada/items/prd_lamp", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove item: %d %s", rec.Code, rec.Body)
	}
	if len(asMap(t, rec)["items"].([]any)) != 0 {
		t.Errorf("expected 0 items after remove")
	}

	// 8. Clear wishlist
	if _, err := f.wishlist.SetItem("cust_ada", wishlist.Item{ProductID: "prd_lamp", Quantity: 1}); err != nil {
		t.Fatalf("set item before clear: %v", err)
	}
	rec = f.do(t, "DELETE", "/wishlists/cust_ada", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear wishlist: %d", rec.Code)
	}
	if len(f.wishlist.Get("cust_ada").Items) != 0 {
		t.Errorf("wishlist not empty after clear")
	}

	// 9. Malformed bodies, extra tokens, and payload too large
	wantProblem(t, f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", "not-json"), http.StatusBadRequest, "invalid-request")
	wantProblem(t, f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", `{"quantity": 1} 123`), http.StatusBadRequest, "invalid-request")
	huge := `{"quantity": 1, "padding":"` + strings.Repeat("x", maxBodyBytes+10) + `"}`
	wantProblem(t, f.do(t, "PUT", "/wishlists/cust_ada/items/prd_lamp", huge), http.StatusRequestEntityTooLarge, "payload-too-large")
}

func TestWishlistCustomerIsolationAndIdentity(t *testing.T) {
	dir := t.TempDir()
	store, err := cart.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	wishlistStore, err := wishlist.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &fakeCatalog{known: map[string]string{"prd_lamp": "Desk lamp"}}
	srv := New(store, catalog, &fakeOrders{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithWishlist(wishlistStore), WithIdentity(idSecret, true))
	h := srv.Handler()

	as := func(sub string, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(identity.Header, identity.Sign(idSecret, identity.Claims{
			Sub: sub, Role: identity.RoleCustomer, Exp: time.Now().Add(time.Minute).Unix(),
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Customer 1 adds an item
	if rec := as("cust_1", "PUT", "/wishlists/cust_1/items/prd_lamp", `{"quantity":2}`); rec.Code != http.StatusOK {
		t.Fatalf("customer 1 add: %d %s", rec.Code, rec.Body)
	}

	// Customer 2 attempts to read, mutate, or delete customer 1's wishlist -> all 403 Forbidden
	for _, c := range [][3]string{
		{"GET", "/wishlists/cust_1", ""},
		{"PUT", "/wishlists/cust_1/items/prd_lamp", `{"quantity":5}`},
		{"DELETE", "/wishlists/cust_1/items/prd_lamp", ""},
		{"DELETE", "/wishlists/cust_1", ""},
	} {
		wantProblem(t, as("cust_2", c[0], c[1], c[2]), http.StatusForbidden, "forbidden")
	}

	// Anonymous request with REQUIRE_IDENTITY -> 401
	anonReq := httptest.NewRequest("GET", "/wishlists/cust_1", nil)
	anonRec := httptest.NewRecorder()
	h.ServeHTTP(anonRec, anonReq)
	wantProblem(t, anonRec, http.StatusUnauthorized, "unauthenticated")

	// Bad assertion -> 401
	badReq := httptest.NewRequest("GET", "/wishlists/cust_1", nil)
	badReq.Header.Set(identity.Header, "bad-token")
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, badReq)
	wantProblem(t, badRec, http.StatusUnauthorized, "unauthenticated")

	// Verify Customer 1's wishlist remained untouched by Customer 2's attempted attacks
	wl := wishlistStore.Get("cust_1")
	if len(wl.Items) != 1 || wl.Items[0].Quantity != 2 {
		t.Fatalf("customer 1 wishlist was altered by a stranger: %+v", wl.Items)
	}
}

func TestWishlistPersistenceAcrossServerReopen(t *testing.T) {
	dir := t.TempDir()
	store1, _ := cart.Open(dir)
	wlStore1, _ := wishlist.Open(dir)
	cat := &fakeCatalog{known: map[string]string{"prd_lamp": "Desk lamp"}}
	srv1 := New(store1, cat, &fakeOrders{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithWishlist(wlStore1))

	// Add item via server 1
	req1 := httptest.NewRequest("PUT", "/wishlists/cust_persist/items/prd_lamp", strings.NewReader(`{"quantity":3}`))
	rec1 := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("server 1 add: %d %s", rec1.Code, rec1.Body)
	}

	// Simulate restart: re-open stores and start server 2 on same dir
	store2, _ := cart.Open(dir)
	wlStore2, _ := wishlist.Open(dir)
	srv2 := New(store2, cat, &fakeOrders{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithWishlist(wlStore2))

	req2 := httptest.NewRequest("GET", "/wishlists/cust_persist", nil)
	rec2 := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("server 2 get: %d %s", rec2.Code, rec2.Body)
	}
	m := asMap(t, rec2)
	items := m["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["product_id"] != "prd_lamp" || int(items[0].(map[string]any)["quantity"].(float64)) != 3 {
		t.Fatalf("restored wishlist unexpected: %+v", m)
	}
	if _, hasName := items[0].(map[string]any)["name"]; hasName {
		t.Errorf("restored wishlist response must not contain name field: %+v", items[0])
	}
}

func TestMissingWishlistStoreConfiguration(t *testing.T) {
	dir := t.TempDir()
	store, err := cart.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cat := &fakeCatalog{known: map[string]string{"prd_lamp": "Desk lamp"}}
	// Server instantiated WITHOUT WithWishlist option
	srv := New(store, cat, &fakeOrders{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	doReq := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, c := range [][3]string{
		{"GET", "/wishlists/cust_1", ""},
		{"PUT", "/wishlists/cust_1/items/prd_lamp", `{"quantity":1}`},
		{"DELETE", "/wishlists/cust_1/items/prd_lamp", ""},
		{"DELETE", "/wishlists/cust_1", ""},
	} {
		rec := doReq(c[0], c[1], c[2])
		wantProblem(t, rec, http.StatusInternalServerError, "internal")
	}
}

// TestRoutesMatchOpenAPISpec keeps code and docs/openapi/cart.yaml honest in
// both directions: an endpoint only in code, or only in the spec, fails.
func TestRoutesMatchOpenAPISpec(t *testing.T) {
	f := newFixture(t)
	want := specOperations(t, filepath.Join("..", "..", "..", "docs", "openapi", "cart.yaml"))
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
	if len(ops) == 0 {
		t.Fatal("found no operations in spec; has its layout changed?")
	}
	sort.Strings(ops)
	return ops
}
