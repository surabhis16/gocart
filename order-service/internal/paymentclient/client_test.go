package paymentclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gocart/order-service/internal/reqid"
)

func fast(url string, opts ...Option) *Client {
	return New(url, append([]Option{WithBackoff(time.Millisecond)}, opts...)...)
}

const approved = `{"order_id":"ord_1","status":"approved","reference":"pay_abc","amount_minor":100,"currency":"INR","a_future_field":1}`

func TestApprovedChargeIgnoresFieldsItDoesNotKnow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(approved))
	}))
	defer srv.Close()
	c, err := fast(srv.URL).Charge(context.Background(), "ord_1", 100, "INR", "tok_visa")
	if err != nil || c.Reference != "pay_abc" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestChargeSendsTheExpectedRequest(t *testing.T) {
	var path, method, id, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method, id = r.URL.Path, r.Method, r.Header.Get(reqid.Header)
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(approved))
	}))
	defer srv.Close()
	ctx := reqid.With(context.Background(), "trace-7")
	if _, err := fast(srv.URL).Charge(ctx, "ord_1", 2550, "INR", "tok_visa"); err != nil {
		t.Fatal(err)
	}
	if path != "/charges" || method != "POST" || id != "trace-7" {
		t.Errorf("path=%s method=%s id=%s", path, method, id)
	}
	for _, want := range []string{`"order_id":"ord_1"`, `"amount_minor":2550`, `"currency":"INR"`, `"payment_token":"tok_visa"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s lacks %s", body, want)
		}
	}
}

func TestDeclineIsDefiniteAndNeverRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"type":"urn:gocart:problem:payment-declined","decline_code":"insufficient_funds"}`))
	}))
	defer srv.Close()
	_, err := fast(srv.URL, WithAttempts(5)).Charge(context.Background(), "ord_1", 100, "INR", "tok_x")
	var d *DeclinedError
	if !errors.As(err, &d) || d.Code != "insufficient_funds" {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("a definite decline was asked %d times", calls.Load())
	}
}

func TestUnavailableIsRetriedThenReportedAsUnknown(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := fast(srv.URL, WithAttempts(3)).Charge(context.Background(), "ord_1", 100, "INR", "tok_down")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	var d *DeclinedError
	if errors.As(err, &d) {
		t.Fatal("an unknown outcome was reported as a decline")
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestRetrySucceedsWhenTheProviderRecovers(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(approved))
	}))
	defer srv.Close()
	c, err := fast(srv.URL).Charge(context.Background(), "ord_1", 100, "INR", "tok_flaky")
	if err != nil || c.Reference != "pay_abc" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestTimeoutIsUnavailableNotDeclined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(approved))
	}))
	defer srv.Close()
	hc := &http.Client{Timeout: 30 * time.Millisecond}
	_, err := fast(srv.URL, WithHTTPClient(hc), WithAttempts(1)).Charge(context.Background(), "ord_1", 100, "INR", "tok_slow")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnreadableSuccessIsNotTreatedAsPaid(t *testing.T) {
	for name, body := range map[string]string{
		"not json":         `<html>`,
		"no reference":     `{"status":"approved"}`,
		"not approved":     `{"status":"declined","reference":"x"}`,
		"voided but a 200": `{"status":"voided","reference":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			if _, err := fast(srv.URL, WithAttempts(1)).Charge(context.Background(), "o", 1, "INR", "tok_x"); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestVoidedChargeIsRecognised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"type":"urn:gocart:problem:charge-voided"}`))
	}))
	defer srv.Close()
	if _, err := fast(srv.URL).Charge(context.Background(), "o", 1, "INR", "tok_x"); !errors.Is(err, ErrVoided) {
		t.Fatalf("err = %v", err)
	}
}

func TestOtherRefusalsAreNeitherDeclineNorUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"type":"urn:gocart:problem:validation-failed"}`))
	}))
	defer srv.Close()
	_, err := fast(srv.URL).Charge(context.Background(), "o", 1, "INR", "tok_x")
	var d *DeclinedError
	if err == nil || errors.As(err, &d) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "validation-failed") {
		t.Errorf("error should name the problem: %v", err)
	}
}

func TestCancelSuccessAndFailure(t *testing.T) {
	var path, method string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := fast(srv.URL)
	if err := c.Cancel(context.Background(), "ord_1"); err != nil {
		t.Fatal(err)
	}
	if path != "/charges/ord_1" || method != "DELETE" {
		t.Errorf("%s %s", method, path)
	}
	status = http.StatusServiceUnavailable
	if err := c.Cancel(context.Background(), "ord_1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	status = http.StatusUnprocessableEntity
	if err := c.Cancel(context.Background(), "ord_1"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(srv.URL, WithBackoff(time.Hour)).Charge(ctx, "o", 1, "INR", "tok_x")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
