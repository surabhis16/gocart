package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gocart/cart-service/internal/identity"
)

var idSecret = []byte("0123456789abcdef0123456789abcdef")

func identityFixture(t *testing.T, require bool) *fixture {
	t.Helper()
	f := newFixture(t)
	f.srv = New(f.store, f.catalog, f.orders, slog.New(slog.NewTextHandler(io.Discard, nil)), WithIdentity(idSecret, require))
	f.h = f.srv.Handler()
	return f
}

func (f *fixture) as(t *testing.T, sub string, ttl time.Duration, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(identity.Header, identity.Sign(idSecret, identity.Claims{Sub: sub, Role: identity.RoleCustomer, Exp: time.Now().Add(ttl).Unix()}))
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestACustomerReachesOnlyTheirOwnCart(t *testing.T) {
	f := identityFixture(t, true)
	if rec := f.as(t, "cust_1", time.Minute, "PUT", "/carts/cust_1/items/prd_lamp", `{"quantity":1}`); rec.Code != http.StatusOK {
		t.Fatalf("own cart: %d %s", rec.Code, rec.Body)
	}
	for _, c := range [][3]string{
		{"GET", "/carts/cust_1", ""},
		{"PUT", "/carts/cust_1/items/prd_lamp", `{"quantity":9}`},
		{"DELETE", "/carts/cust_1/items/prd_lamp", ""},
		{"DELETE", "/carts/cust_1", ""},
		{"POST", "/carts/cust_1/checkout", `{"payment_token":"tok_visa"}`},
	} {
		wantProblem(t, f.as(t, "cust_2", time.Minute, c[0], c[1], c[2]), http.StatusForbidden, "forbidden")
	}
	if got := f.store.Get("cust_1"); len(got.Items) != 1 || got.Items[0].Quantity != 1 {
		t.Fatalf("a stranger changed the cart: %+v", got.Items)
	}
	if len(f.orders.keys) != 0 {
		t.Fatal("a stranger's checkout reached the Order service")
	}
}

func TestRequireIdentityRefusesAnonymousCartRoutes(t *testing.T) {
	f := identityFixture(t, true)
	wantProblem(t, f.do(t, "GET", "/carts/cust_1", ""), http.StatusUnauthorized, "unauthenticated")
	wantProblem(t, f.do(t, "PUT", "/carts/cust_1/items/prd_lamp", `{"quantity":1}`), http.StatusUnauthorized, "unauthenticated")
	if rec := f.do(t, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestBadAssertionsAreRefused(t *testing.T) {
	f := identityFixture(t, false)
	wantProblem(t, f.as(t, "cust_1", -time.Second, "GET", "/carts/cust_1", ""), http.StatusUnauthorized, "unauthenticated")
	req := httptest.NewRequest("GET", "/carts/cust_1", nil)
	req.Header.Set(identity.Header, "garbage")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	wantProblem(t, rec, http.StatusUnauthorized, "unauthenticated")
}

func TestWithoutRequireAnonymousCallsStillWork(t *testing.T) {
	f := identityFixture(t, false)
	if rec := f.do(t, "GET", "/carts/cust_1", ""); rec.Code != http.StatusOK {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}
