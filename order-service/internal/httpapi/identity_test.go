package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"gocart/order-service/internal/eventstore"
	"gocart/order-service/internal/identity"
	"gocart/order-service/internal/service"
)

var idSecret = []byte("0123456789abcdef0123456789abcdef")

// as makes a header pair for a signed-in caller.
func as(sub, role string, ttl time.Duration) []string {
	return []string{identity.Header, identity.Sign(idSecret, identity.Claims{Sub: sub, Role: role, Exp: time.Now().Add(ttl).Unix()})}
}

func identityFixture(t *testing.T, require bool) *fixture {
	t.Helper()
	f := newFixture(t)
	store, _ := eventstore.OpenFile("")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := service.New(store, f.stub, f.pay, log, service.WithGrace(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f.srv = New(svc, log, WithAdminKey(testKey), WithIdentity(idSecret, require))
	f.h = f.srv.Handler()
	return f
}

func orderFor(t *testing.T, f *fixture, customer string) string {
	t.Helper()
	body := `{"customer_id":"` + customer + `","payment_token":"tok_visa","items":[{"product_id":"prd_a","quantity":1}]}`
	rec := f.doAnon(t, "POST", "/orders", body, as(customer, identity.RoleCustomer, time.Minute)...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("place: %d %s", rec.Code, rec.Body)
	}
	return asMap(t, rec)["id"].(string)
}

func TestACustomerCanOnlyPlaceOrdersForThemselves(t *testing.T) {
	f := identityFixture(t, true)
	rec := f.doAnon(t, "POST", "/orders", goodOrder, as("cust_2", identity.RoleCustomer, time.Minute)...)
	wantProblem(t, rec, http.StatusForbidden, "forbidden")
	if rec := f.doAnon(t, "POST", "/orders", goodOrder, as("cust_1", identity.RoleCustomer, time.Minute)...); rec.Code != http.StatusCreated {
		t.Fatalf("own order: %d %s", rec.Code, rec.Body)
	}
}

func TestSomeoneElsesOrderLooksLikeItDoesNotExist(t *testing.T) {
	f := identityFixture(t, true)
	id := orderFor(t, f, "cust_1")
	mine := as("cust_1", identity.RoleCustomer, time.Minute)
	theirs := as("cust_2", identity.RoleCustomer, time.Minute)

	if rec := f.doAnon(t, "GET", "/orders/"+id, "", mine...); rec.Code != http.StatusOK {
		t.Fatalf("owner read: %d", rec.Code)
	}
	wantProblem(t, f.doAnon(t, "GET", "/orders/"+id, "", theirs...), http.StatusNotFound, "not-found")
	wantProblem(t, f.doAnon(t, "POST", "/orders/"+id+"/cancel", "", theirs...), http.StatusNotFound, "not-found")
	if got := asMap(t, f.doAnon(t, "GET", "/orders/"+id, "", mine...))["status"]; got != "confirmed" {
		t.Fatalf("a stranger's cancel changed the order: %v", got)
	}
	if rec := f.doAnon(t, "POST", "/orders/"+id+"/cancel", "", mine...); rec.Code != http.StatusOK {
		t.Fatalf("owner cancel: %d %s", rec.Code, rec.Body)
	}
}

func TestListingIsScopedToTheCaller(t *testing.T) {
	f := identityFixture(t, true)
	orderFor(t, f, "cust_1")
	orderFor(t, f, "cust_2")
	me := as("cust_1", identity.RoleCustomer, time.Minute)

	items := func(path string, hdr []string) []any {
		rec := f.doAnon(t, "GET", path, "", hdr...)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		return asMap(t, rec)["items"].([]any)
	}
	if n := len(items("/orders", me)); n != 1 {
		t.Errorf("no filter: customer sees %d orders, want only their own (1)", n)
	}
	if n := len(items("/orders?customer_id=cust_1", me)); n != 1 {
		t.Errorf("own filter: %d", n)
	}
	wantProblem(t, f.doAnon(t, "GET", "/orders?customer_id=cust_2", "", me...), http.StatusForbidden, "forbidden")
	if n := len(items("/orders", as("someone", identity.RoleAdmin, time.Minute))); n != 2 {
		t.Errorf("admin sees %d orders, want 2", n)
	}
}

func TestAdminCanReadAndCancelAnyOrder(t *testing.T) {
	f := identityFixture(t, true)
	id := orderFor(t, f, "cust_1")
	admin := as("boss", identity.RoleAdmin, time.Minute)
	if rec := f.doAnon(t, "GET", "/orders/"+id, "", admin...); rec.Code != http.StatusOK {
		t.Fatalf("admin read: %d", rec.Code)
	}
	if rec := f.doAnon(t, "POST", "/orders/"+id+"/cancel", "", admin...); rec.Code != http.StatusOK {
		t.Fatalf("admin cancel: %d", rec.Code)
	}
}

func TestBadAssertionsAreRefusedNotIgnored(t *testing.T) {
	f := identityFixture(t, false)
	cases := map[string][]string{
		"expired":      as("cust_1", identity.RoleCustomer, -time.Second),
		"wrong secret": {identity.Header, identity.Sign([]byte("another-secret-another-secret-0000"), identity.Claims{Sub: "cust_1", Exp: time.Now().Add(time.Minute).Unix()})},
		"garbage":      {identity.Header, "hello"},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			wantProblem(t, f.doAnon(t, "GET", "/orders", "", hdr...), http.StatusUnauthorized, "unauthenticated")
		})
	}
}

func TestWithoutASecretAnAssertionCannotBeTrusted(t *testing.T) {
	f := newFixture(t) // no WithIdentity
	wantProblem(t, f.doAnon(t, "GET", "/orders", "", as("cust_1", identity.RoleCustomer, time.Minute)...), http.StatusUnauthorized, "unauthenticated")
}

func TestRequireIdentityRefusesAnonymousCustomerRoutes(t *testing.T) {
	f := identityFixture(t, true)
	id := orderFor(t, f, "cust_1")
	for _, c := range [][2]string{{"POST", "/orders"}, {"GET", "/orders"}, {"GET", "/orders/" + id}, {"POST", "/orders/" + id + "/cancel"}} {
		wantProblem(t, f.doAnon(t, c[0], c[1], goodOrder), http.StatusUnauthorized, "unauthenticated")
	}
	// health and metrics stay open, and admin routes still answer to the admin key
	if rec := f.doAnon(t, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("healthz: %d", rec.Code)
	}
	if rec := f.do(t, "GET", "/orders/"+id+"/events", ""); rec.Code != http.StatusOK {
		t.Errorf("admin events: %d", rec.Code)
	}
}

func TestWithoutRequireAnonymousCallsStillWork(t *testing.T) {
	f := identityFixture(t, false)
	if rec := f.doAnon(t, "POST", "/orders", goodOrder); rec.Code != http.StatusCreated {
		t.Fatalf("anonymous local call: %d %s", rec.Code, rec.Body)
	}
}
