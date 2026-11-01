package productclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/reqid"
)

func fast(url string, opts ...Option) *Client {
	return New(url, append([]Option{WithBackoff(time.Millisecond)}, opts...)...)
}

func items() []domain.Item { return []domain.Item{{ProductID: "prd_a", Quantity: 2}} }

const okBody = `{"order_id":"ord_1","currency":"INR","reserved_at":"2026-10-30T09:00:00Z","some_future_field":true,
 "items":[{"product_id":"prd_a","name":"A","quantity":2,"unit_price_minor":1000}]}`

func TestReserveSuccessIgnoresFieldsItDoesNotKnow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()
	res, err := fast(srv.URL).Reserve(context.Background(), "ord_1", items())
	if err != nil {
		t.Fatal(err)
	}
	if res.Currency != "INR" || len(res.Items) != 1 || res.Items[0].UnitPriceMinor != 1000 || res.Items[0].Name != "A" {
		t.Errorf("res = %+v", res)
	}
}

func TestReserveSendsTheRequestIDAndABody(t *testing.T) {
	var gotID, gotCT, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotCT, gotPath = r.Header.Get(reqid.Header), r.Header.Get("Content-Type"), r.URL.Path
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()
	ctx := reqid.With(context.Background(), "trace-42")
	if _, err := fast(srv.URL).Reserve(ctx, "ord_1", items()); err != nil {
		t.Fatal(err)
	}
	if gotID != "trace-42" || gotCT != "application/json" || gotPath != "/reservations" {
		t.Errorf("id=%q content-type=%q path=%q", gotID, gotCT, gotPath)
	}
}

func TestDefiniteAnswersAreNeverRetried(t *testing.T) {
	cases := map[string]struct {
		body       string
		status     int
		wantReason string
		wantMsg    string
	}{
		"insufficient stock": {
			`{"type":"urn:gocart:problem:insufficient-stock","status":409,"shortages":[{"product_id":"prd_a","requested":2,"available":1}]}`,
			409, ReasonInsufficientStock, "prd_a: wanted 2, only 1 available"},
		"unknown product": {
			`{"type":"urn:gocart:problem:unknown-product","status":422,"product_ids":["prd_x","prd_y"]}`,
			422, ReasonUnknownProduct, "unknown product(s): prd_x, prd_y"},
		"validation": {
			`{"type":"urn:gocart:problem:validation-failed","status":422,"detail":"quantity must be between 1 and 100"}`,
			422, ReasonInvalidItems, "quantity must be between 1 and 100"},
		"unreadable body": {`<html>nope</html>`, 400, ReasonRefused, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := fast(srv.URL).Reserve(context.Background(), "ord_1", items())
			var rej *RejectedError
			if !errors.As(err, &rej) || rej.Reason != tc.wantReason || rej.Message != tc.wantMsg {
				t.Fatalf("err = %v, want reason %q message %q", err, tc.wantReason, tc.wantMsg)
			}
			if errors.Is(err, ErrUnavailable) {
				t.Error("a definite rejection must not look like an outage")
			}
			if calls.Load() != 1 {
				t.Errorf("%d calls, want exactly 1: a definite answer was retried", calls.Load())
			}
		})
	}
}

func TestTransientFailuresAreRetriedThenSucceed(t *testing.T) {
	for _, status := range []int{500, 502, 503, 429} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(okBody))
		}))
		if _, err := fast(srv.URL).Reserve(context.Background(), "ord_1", items()); err != nil {
			t.Errorf("status %d: %v", status, err)
		}
		if calls.Load() != 3 {
			t.Errorf("status %d: %d calls, want 3", status, calls.Load())
		}
		srv.Close()
	}
}

func TestGivesUpAsUnavailableAfterTheConfiguredAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := fast(srv.URL, WithAttempts(4)).Reserve(context.Background(), "ord_1", items())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls.Load() != 4 {
		t.Errorf("%d calls, want 4", calls.Load())
	}
}

func TestConnectionRefusedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening any more
	if _, err := fast(url, WithAttempts(2)).Reserve(context.Background(), "ord_1", items()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if err := fast(url, WithAttempts(2)).Release(context.Background(), "ord_1"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Release err = %v, want ErrUnavailable", err)
	}
}

func TestTimeoutIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	c := fast(srv.URL, WithAttempts(1), WithHTTPClient(&http.Client{Timeout: 20 * time.Millisecond}))
	if _, err := c.Reserve(context.Background(), "ord_1", items()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func TestSuccessWeCannotReadIsNotTrusted(t *testing.T) {
	for name, body := range map[string]string{
		"not json":    `ok`,
		"no items":    `{"order_id":"ord_1","currency":"INR","items":[]}`,
		"no currency": `{"order_id":"ord_1","items":[{"product_id":"a","name":"A","quantity":1,"unit_price_minor":1}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		_, err := fast(srv.URL).Reserve(context.Background(), "ord_1", items())
		srv.Close()
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v; building an order on a response we cannot read would be worse than waiting", name, err)
		}
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := New(srv.URL, WithBackoff(5*time.Second)).Reserve(ctx, "ord_1", items())
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > time.Second {
		t.Errorf("err = %v after %v; should give up at once", err, time.Since(start))
	}
}

func TestReleaseStatuses(t *testing.T) {
	for status, wantErr := range map[int]error{204: nil, 200: nil, 404: nil} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/reservations/ord_1" {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		if err := fast(srv.URL).Release(context.Background(), "ord_1"); !errors.Is(err, wantErr) {
			t.Errorf("status %d: err = %v", status, err)
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(422) }))
	defer srv.Close()
	err := fast(srv.URL).Release(context.Background(), "ord_1")
	if err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("422 on release: err = %v, want a definite failure", err)
	}
}

func TestOrderIDIsEscapedInThePath(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.EscapedPath()
		w.WriteHeader(204)
	}))
	defer srv.Close()
	_ = fast(srv.URL).Release(context.Background(), "a/b c")
	if raw != "/reservations/a%2Fb%20c" {
		t.Errorf("path = %q", raw)
	}
}
