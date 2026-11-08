package orderclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gocart/cart-service/internal/cart"
	"gocart/cart-service/internal/identity"
	"gocart/cart-service/internal/reqid"
)

var lines = []cart.Line{{ProductID: "prd_a", Name: "A", Quantity: 2}, {ProductID: "prd_b", Name: "B", Quantity: 1}}

func serve(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, nil)
}

func TestWhatIsSentToTheOrderService(t *testing.T) {
	var method, path, key, rid, ct string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		key, rid, ct = r.Header.Get("Idempotency-Key"), r.Header.Get(reqid.Header), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"ord_1"}`))
	}))
	defer srv.Close()

	ctx := reqid.With(context.Background(), "req-7")
	if _, err := New(srv.URL, nil).Place(ctx, "cart:c:3", "c", "tok_visa", lines); err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/orders" || key != "cart:c:3" || rid != "req-7" || ct != "application/json" {
		t.Errorf("%s %s key=%q rid=%q ct=%q", method, path, key, rid, ct)
	}
	if body["customer_id"] != "c" || body["payment_token"] != "tok_visa" {
		t.Errorf("body = %v", body)
	}
	items := body["items"].([]any)
	first := items[0].(map[string]any)
	if len(items) != 2 || first["product_id"] != "prd_a" || first["quantity"] != float64(2) {
		t.Errorf("items = %v", items)
	}
	if _, leaked := first["name"]; leaked {
		t.Error("the cart's display name is not part of the order request, and the Order service rejects unknown fields")
	}
}

func TestAnsweredCallsPassThrough(t *testing.T) {
	for status, accepted := range map[int]bool{201: true, 200: true, 202: true, 402: false, 409: false, 422: false} {
		c := serve(t, status, `{"id":"ord_1","type":"urn:x"}`)
		r, err := c.Place(context.Background(), "k", "c", "tok_visa", lines)
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if r.Status != status || r.Accepted() != accepted || r.Definite() == accepted {
			t.Errorf("%d: accepted=%v definite=%v", status, r.Accepted(), r.Definite())
		}
	}
}

func TestNoDefiniteAnswerIsUnavailable(t *testing.T) {
	// 5xx, 408 and 429 do not tell us whether the order exists. Treating them
	// as refusals would move the cart to a new key and risk a second order.
	for _, status := range []int{500, 502, 503, 504, 408, 429} {
		_, err := serve(t, status, `{}`).Place(context.Background(), "k", "c", "tok_visa", lines)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%d: %v", status, err)
		}
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, err := New(srv.URL, nil).Place(context.Background(), "k", "c", "tok_visa", lines); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestSuccessWithoutAnOrderIdIsNotTrusted(t *testing.T) {
	_, err := serve(t, 201, `{"status":"confirmed"}`).Place(context.Background(), "k", "c", "tok_visa", lines)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestTheShoppersIdentityIsPassedOn(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(identity.Header)
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"ord_1"}`))
	}))
	defer srv.Close()

	ctx := identity.With(context.Background(), identity.Claims{Sub: "c"}, "v1.assertion.sig")
	if _, err := New(srv.URL, nil).Place(ctx, "k", "c", "tok_visa", lines); err != nil {
		t.Fatal(err)
	}
	if got != "v1.assertion.sig" {
		t.Errorf("identity header = %q", got)
	}
	got = "unset"
	if _, err := New(srv.URL, nil).Place(context.Background(), "k", "c", "tok_visa", lines); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("an anonymous call sent an identity header: %q", got)
	}
}
