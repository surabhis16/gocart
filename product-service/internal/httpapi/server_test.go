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

	"gocart/product-service/internal/catalog"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	store, err := catalog.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	srv := New(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv, srv.Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
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

func TestCatalogueEndpoints(t *testing.T) {
	_, h := newTestServer(t)

	rec := do(t, h, "GET", "/products", "")
	if rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	if items := decodeBody(t, rec)["items"].([]any); len(items) != 6 {
		t.Errorf("seeded catalogue has %d items, want 6", len(items))
	}

	rec = do(t, h, "GET", "/products/prd_lamp", "")
	if rec.Code != 200 || decodeBody(t, rec)["stock"].(float64) != 3 {
		t.Errorf("get lamp: %d %s", rec.Code, rec.Body)
	}
	wantProblem(t, do(t, h, "GET", "/products/prd_nope", ""), 404, "not-found")

	rec = do(t, h, "POST", "/products", `{"name":"Cable","price_minor":19900,"stock":7}`)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := decodeBody(t, rec)["id"].(string)
	if loc := rec.Header().Get("Location"); loc != "/products/"+id {
		t.Errorf("Location = %q", loc)
	}

	rec = do(t, h, "PUT", "/products/"+id+"/stock", `{"stock":70}`)
	if rec.Code != 200 || decodeBody(t, rec)["stock"].(float64) != 70 {
		t.Errorf("set stock: %d %s", rec.Code, rec.Body)
	}
	wantProblem(t, do(t, h, "PUT", "/products/"+id+"/stock", `{"stock":-1}`), 422, "validation-failed")
	wantProblem(t, do(t, h, "PUT", "/products/prd_nope/stock", `{"stock":1}`), 404, "not-found")
}

func TestReservationLifecycle(t *testing.T) {
	_, h := newTestServer(t)
	body := `{"order_id":"ord_1","items":[{"product_id":"prd_lamp","quantity":2}]}`

	rec := do(t, h, "POST", "/reservations", body)
	if rec.Code != 201 {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	res := decodeBody(t, rec)
	line := res["items"].([]any)[0].(map[string]any)
	if line["unit_price_minor"].(float64) != 129900 || line["name"] != "LED Desk Lamp" {
		t.Errorf("reserved line lacks price snapshot: %v", line)
	}

	// A retry is a 200, not a second deduction.
	if rec = do(t, h, "POST", "/reservations", body); rec.Code != 200 {
		t.Errorf("replay: status %d, want 200", rec.Code)
	}
	if stock := decodeBody(t, do(t, h, "GET", "/products/prd_lamp", ""))["stock"].(float64); stock != 1 {
		t.Errorf("lamp stock = %v after reserve+replay, want 1", stock)
	}

	wantProblem(t, do(t, h, "POST", "/reservations",
		`{"order_id":"ord_1","items":[{"product_id":"prd_lamp","quantity":3}]}`), 409, "reservation-mismatch")

	if rec = do(t, h, "DELETE", "/reservations/ord_1", ""); rec.Code != 204 {
		t.Errorf("release: %d", rec.Code)
	}
	if rec = do(t, h, "DELETE", "/reservations/ord_1", ""); rec.Code != 204 {
		t.Errorf("second release: %d, want 204 (idempotent)", rec.Code)
	}
	if stock := decodeBody(t, do(t, h, "GET", "/products/prd_lamp", ""))["stock"].(float64); stock != 3 {
		t.Errorf("lamp stock = %v after release, want 3", stock)
	}
}

func TestReservationFailures(t *testing.T) {
	_, h := newTestServer(t)

	p := wantProblem(t, do(t, h, "POST", "/reservations",
		`{"order_id":"ord_1","items":[{"product_id":"prd_keyboard","quantity":1},{"product_id":"prd_deskmat","quantity":1}]}`),
		409, "insufficient-stock")
	shortages := p["shortages"].([]any)
	if len(shortages) != 1 || shortages[0].(map[string]any)["product_id"] != "prd_deskmat" {
		t.Errorf("shortages = %v", shortages)
	}
	if stock := decodeBody(t, do(t, h, "GET", "/products/prd_keyboard", ""))["stock"].(float64); stock != 25 {
		t.Errorf("keyboard stock = %v after rejected reservation, want 25", stock)
	}

	p = wantProblem(t, do(t, h, "POST", "/reservations",
		`{"order_id":"ord_2","items":[{"product_id":"prd_ghost","quantity":1}]}`), 422, "unknown-product")
	if ids := p["product_ids"].([]any); len(ids) != 1 || ids[0] != "prd_ghost" {
		t.Errorf("product_ids = %v", ids)
	}

	wantProblem(t, do(t, h, "POST", "/reservations",
		`{"order_id":"ord_3","items":[{"product_id":"prd_hub","quantity":0}]}`), 422, "validation-failed")
}

func TestStrictBodyParsing(t *testing.T) {
	_, h := newTestServer(t)
	cases := map[string]string{
		"unknown field":  `{"order_id":"ord_1","items":[{"product_id":"prd_hub","quantity":1}],"coupon":"X"}`,
		"misspelt field": `{"order_id":"ord_1","items":[{"product_id":"prd_hub","qty":1}]}`,
		"not json":       `order please`,
		"trailing data":  `{"order_id":"ord_1","items":[{"product_id":"prd_hub","quantity":1}]} {}`,
		"empty":          ``,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			wantProblem(t, do(t, h, "POST", "/reservations", body), 400, "invalid-request")
		})
	}
	big := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
	wantProblem(t, do(t, h, "POST", "/products", big), 413, "payload-too-large")
}

func TestRequestIDIsEchoedAndSanitised(t *testing.T) {
	_, h := newTestServer(t)

	rec := do(t, h, "GET", "/products/prd_nope", "", RequestIDHeader, "trace-abc.123")
	if got := rec.Header().Get(RequestIDHeader); got != "trace-abc.123" {
		t.Errorf("request id = %q, want it echoed", got)
	}
	if id := decodeBody(t, rec)["request_id"]; id != "trace-abc.123" {
		t.Errorf("problem body request_id = %v", id)
	}

	rec = do(t, h, "GET", "/healthz", "", RequestIDHeader, "bad id\nwith newline")
	if got := rec.Header().Get(RequestIDHeader); got == "" || strings.ContainsAny(got, " \n") {
		t.Errorf("unsafe request id was not replaced: %q", got)
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	_, h := newTestServer(t)
	if rec := do(t, h, "DELETE", "/products/prd_hub", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /products/{id}: %d, want 405", rec.Code)
	}
}

// TestRoutesMatchOpenAPISpec keeps the code and docs/openapi/product.yaml
// honest in both directions: an endpoint that exists only in code, or only in
// the spec, fails the build.
func TestRoutesMatchOpenAPISpec(t *testing.T) {
	srv, _ := newTestServer(t)
	want := specOperations(t, filepath.Join("..", "..", "..", "docs", "openapi", "product.yaml"))
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

// specOperations pulls "METHOD /path" pairs out of the spec's paths section.
// The spec is hand-written with fixed indentation, which keeps this a few
// lines of standard library instead of a YAML dependency.
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
