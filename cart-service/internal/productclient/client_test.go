package productclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"gocart/cart-service/internal/reqid"
)

func serve(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/products/prd_lamp" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, nil)
}

func TestNameOfAKnownProduct(t *testing.T) {
	// Unknown fields must be ignored: the Product service may add them freely.
	c := serve(t, 200, `{"id":"prd_lamp","name":"Desk lamp","price_minor":129900,"future_field":{"x":1}}`)
	name, err := c.Name(context.Background(), "prd_lamp")
	if err != nil || name != "Desk lamp" {
		t.Fatalf("%q, %v", name, err)
	}
}

func TestUnknownProductIsADefiniteAnswer(t *testing.T) {
	_, err := serve(t, 404, `{}`).Name(context.Background(), "prd_lamp")
	if !errors.Is(err, ErrUnknown) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestAnythingElseIsUnavailableNotUnknown(t *testing.T) {
	// If a 500 or garbage were read as "no such product", a Product outage
	// would make every add-to-cart claim the product does not exist.
	for name, c := range map[string]*Client{
		"500":        serve(t, 500, `oops`),
		"503":        serve(t, 503, ``),
		"garbage":    serve(t, 200, `<html>`),
		"no name":    serve(t, 200, `{"id":"prd_lamp"}`),
		"unexpected": serve(t, 401, `{}`),
	} {
		if _, err := c.Name(context.Background(), "prd_lamp"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestUnreachableIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, err := New(srv.URL, nil).Name(context.Background(), "prd_lamp"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestRequestIDIsForwardedAndTheIDIsEscaped(t *testing.T) {
	var gotID, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotPath = r.Header.Get(reqid.Header), r.URL.EscapedPath()
		w.Write([]byte(`{"name":"x"}`))
	}))
	defer srv.Close()
	ctx := reqid.With(context.Background(), "abc123")
	if _, err := New(srv.URL, nil).Name(ctx, "a/b?c"); err != nil {
		t.Fatal(err)
	}
	if gotID != "abc123" || gotPath != "/products/a%2Fb%3Fc" {
		t.Errorf("id=%q path=%q", gotID, gotPath)
	}
}
