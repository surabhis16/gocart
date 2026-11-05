package httpapi

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gocart/payment-service/internal/gateway"
	"gocart/payment-service/internal/ledger"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	l, err := ledger.Open(t.TempDir(), gateway.New(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(l, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAdminKey(testKey))
	return srv, srv.Handler()
}

const testKey = "test-admin-key"

// do sends a request carrying the admin key; doAnon sends it without one.
func do(t *testing.T, h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doAs(t, h, method, path, body, append([]string{"Authorization", "Bearer " + testKey}, hdr...)...)
}

func doAnon(t *testing.T, h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	return doAs(t, h, method, path, body, hdr...)
}

func doAs(t *testing.T, h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return m
}

func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d\n%s", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	m := decodeBody(t, rec)
	if m["type"] != "urn:gocart:problem:"+typ {
		t.Errorf("problem type = %v, want %s", m["type"], typ)
	}
	return m
}

func body(order, token string, amount int) string {
	b, _ := json.Marshal(map[string]any{"order_id": order, "amount_minor": amount, "currency": "INR", "payment_token": token})
	return string(b)
}

func TestApprovedThenRepeated(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 5000))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	first := decodeBody(t, rec)
	if first["status"] != "approved" || first["reference"] == "" {
		t.Fatalf("%v", first)
	}
	if loc := rec.Header().Get("Location"); loc != "/charges/ord_1" {
		t.Errorf("Location = %q", loc)
	}

	rec = do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 5000))
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat: %d, want 200", rec.Code)
	}
	if decodeBody(t, rec)["reference"] != first["reference"] {
		t.Error("a repeat returned a different reference")
	}

	rec = do(t, h, "GET", "/charges/ord_1", "")
	if rec.Code != http.StatusOK || decodeBody(t, rec)["status"] != "approved" {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
}

func TestDeclinedIs402EveryTime(t *testing.T) {
	_, h := newTestServer(t)
	for i := 0; i < 2; i++ {
		p := wantProblem(t, do(t, h, "POST", "/charges", body("ord_1", "tok_insufficient_funds", 100)), 402, "payment-declined")
		if p["decline_code"] != "insufficient_funds" || p["order_id"] != "ord_1" {
			t.Fatalf("%v", p)
		}
	}
	// Definite: even a good card does not reopen a declined order.
	wantProblem(t, do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100)), 402, "payment-declined")
}

func TestProviderUnavailableIs503AndRetryable(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(t, h, "POST", "/charges", body("ord_1", "tok_flaky", 100))
	wantProblem(t, rec, 503, "provider-unavailable")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("503 should say when to retry")
	}
	wantProblem(t, do(t, h, "GET", "/charges/ord_1", ""), 404, "not-found")
	if rec := do(t, h, "POST", "/charges", body("ord_1", "tok_flaky", 100)); rec.Code != http.StatusCreated {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
}

func TestCancelThenChargeIsRefused(t *testing.T) {
	_, h := newTestServer(t)
	if rec := do(t, h, "DELETE", "/charges/ord_1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/charges/ord_1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel again: %d, want 204", rec.Code)
	}
	wantProblem(t, do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100)), 409, "charge-voided")
}

func TestCancelRefundsAnApprovedCharge(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100))
	if rec := do(t, h, "DELETE", "/charges/ord_1", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rec.Code)
	}
	got := decodeBody(t, do(t, h, "GET", "/charges/ord_1", ""))
	if got["status"] != "voided" || got["voided_at"] == nil {
		t.Fatalf("%v", got)
	}
	// A repeated charge request for a refunded order must not look like success.
	wantProblem(t, do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100)), 409, "charge-voided")
}

func TestAmountMismatchIs409(t *testing.T) {
	_, h := newTestServer(t)
	do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 100))
	wantProblem(t, do(t, h, "POST", "/charges", body("ord_1", "tok_visa", 101)), 409, "charge-mismatch")
}

func TestValidationFailures(t *testing.T) {
	_, h := newTestServer(t)
	cases := map[string]string{
		"card number":     body("ord_1", "4242424242424242", 100),
		"zero amount":     body("ord_1", "tok_visa", 0),
		"missing order":   body("", "tok_visa", 100),
		"lowercase money": `{"order_id":"o","amount_minor":1,"currency":"inr","payment_token":"tok_visa"}`,
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			wantProblem(t, do(t, h, "POST", "/charges", b), 422, "validation-failed")
		})
	}
}

func TestCardNumberIsNeverEchoedOrStored(t *testing.T) {
	srv, h := newTestServer(t)
	rec := do(t, h, "POST", "/charges", body("ord_1", "4242424242424242", 100))
	if strings.Contains(rec.Body.String(), "4242424242424242") {
		t.Error("the response echoed the card number")
	}
	if _, err := srv.ledger.Get("ord_1"); err == nil {
		t.Error("a refused request left a charge behind")
	}
}

func TestStrictBodyParsing(t *testing.T) {
	_, h := newTestServer(t)
	for name, b := range map[string]string{
		"not json":      `{`,
		"unknown field": `{"order_id":"o","amount_minor":1,"currency":"INR","payment_token":"tok_visa","amount":9}`,
		"trailing data": body("o", "tok_visa", 1) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantProblem(t, do(t, h, "POST", "/charges", b), 400, "invalid-request")
		})
	}
	big := `{"order_id":"` + strings.Repeat("a", 2<<20) + `"}`
	wantProblem(t, do(t, h, "POST", "/charges", big), 413, "payload-too-large")
}

func TestRequestIDIsEchoedAndSanitised(t *testing.T) {
	_, h := newTestServer(t)
	rec := do(t, h, "GET", "/healthz", "", RequestIDHeader, "abc-123")
	if got := rec.Header().Get(RequestIDHeader); got != "abc-123" {
		t.Errorf("echoed %q", got)
	}
	rec = do(t, h, "GET", "/healthz", "", RequestIDHeader, "bad id\nwith newline")
	if got := rec.Header().Get(RequestIDHeader); got == "" || strings.ContainsAny(got, " \n") {
		t.Errorf("unsafe id passed through: %q", got)
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	_, h := newTestServer(t)
	if rec := do(t, h, "PUT", "/charges/ord_1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /charges/{id}: %d, want 405", rec.Code)
	}
}

// TestRoutesMatchOpenAPISpec keeps the code and docs/openapi/payment.yaml
// honest in both directions.
func TestRoutesMatchOpenAPISpec(t *testing.T) {
	srv, _ := newTestServer(t)
	want := specOperations(t, filepath.Join("..", "..", "..", "docs", "openapi", "payment.yaml"))
	got := srv.Routes()
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
	f, err := os.Open(file)
	if err != nil {
		t.Fatalf("open spec: %v", err)
	}
	defer f.Close()
	var ops []string
	inPaths := false
	path := ""
	sc := bufio.NewScanner(f)
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
