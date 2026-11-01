package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/eventstore"
	"gocart/order-service/internal/productclient"
)

// fakeProducts stands in for the Product service. It can be switched off to
// simulate an outage, and it counts calls so tests can prove idempotency.
type fakeProducts struct {
	mu       sync.Mutex
	down     bool
	stock    map[string]int
	held     map[string][]domain.Item
	reserves int
	releases int
}

func newFake() *fakeProducts {
	return &fakeProducts{
		stock: map[string]int{"prd_a": 10, "prd_b": 5, "prd_scarce": 1},
		held:  map[string][]domain.Item{},
	}
}

func (f *fakeProducts) setDown(d bool) { f.mu.Lock(); f.down = d; f.mu.Unlock() }

func (f *fakeProducts) stockOf(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stock[id]
}

func (f *fakeProducts) Reserve(_ context.Context, orderID string, items []domain.Item) (productclient.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserves++
	if f.down {
		return productclient.Reservation{}, fmt.Errorf("%w: connection refused", productclient.ErrUnavailable)
	}
	if _, ok := f.held[orderID]; !ok { // idempotent per order, like the real service
		for _, it := range items {
			have, known := f.stock[it.ProductID]
			if !known {
				return productclient.Reservation{}, &productclient.RejectedError{
					Reason: productclient.ReasonUnknownProduct, Message: "unknown product(s): " + it.ProductID}
			}
			if have < it.Quantity {
				return productclient.Reservation{}, &productclient.RejectedError{
					Reason:  productclient.ReasonInsufficientStock,
					Message: fmt.Sprintf("%s: wanted %d, only %d available", it.ProductID, it.Quantity, have)}
			}
		}
		for _, it := range items {
			f.stock[it.ProductID] -= it.Quantity
		}
		f.held[orderID] = items
	}
	res := productclient.Reservation{OrderID: orderID, Currency: "INR"}
	for _, it := range f.held[orderID] {
		res.Items = append(res.Items, domain.PricedItem{
			ProductID: it.ProductID, Name: "Name of " + it.ProductID, Quantity: it.Quantity, UnitPriceMinor: 1000})
	}
	return res, nil
}

func (f *fakeProducts) Release(_ context.Context, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	if f.down {
		return fmt.Errorf("%w: connection refused", productclient.ErrUnavailable)
	}
	for _, it := range f.held[orderID] {
		f.stock[it.ProductID] += it.Quantity
	}
	delete(f.held, orderID)
	return nil
}

// clock is a controllable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	svc   *Service
	fake  *fakeProducts
	store eventstore.Store
	clk   *clock
}

func setup(t *testing.T) *env {
	t.Helper()
	store, err := eventstore.OpenFile("")
	if err != nil {
		t.Fatal(err)
	}
	return build(t, store, newFake(), &clock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)})
}

func build(t *testing.T, store eventstore.Store, fake *fakeProducts, clk *clock) *env {
	t.Helper()
	svc, err := New(store, fake, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithClock(clk.now), WithGrace(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, fake: fake, store: store, clk: clk}
}

var ctx = context.Background()

func items(pairs ...any) []domain.Item {
	var out []domain.Item
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, domain.Item{ProductID: pairs[i].(string), Quantity: pairs[i+1].(int)})
	}
	return out
}

func TestPlaceConfirmsAndPricesTheOrder(t *testing.T) {
	e := setup(t)
	res, err := e.svc.Place(ctx, "cust_1", "", items("prd_a", 2, "prd_b", 1))
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order
	if o.Status != domain.StatusConfirmed || o.TotalMinor != 3000 || o.Currency != "INR" || o.Version != 2 {
		t.Errorf("order = %+v", o)
	}
	if e.fake.stockOf("prd_a") != 8 || e.fake.stockOf("prd_b") != 4 {
		t.Error("stock was not deducted")
	}
}

func TestPlaceRejectedWhenStockIsShort(t *testing.T) {
	e := setup(t)
	res, err := e.svc.Place(ctx, "cust_1", "", items("prd_scarce", 3))
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order
	if o.Status != domain.StatusRejected || o.RejectReason != productclient.ReasonInsufficientStock {
		t.Errorf("order = %+v", o)
	}
	if o.RejectDetail == "" {
		t.Error("rejection carries no explanation")
	}
	if e.fake.stockOf("prd_scarce") != 1 {
		t.Error("a rejected order must not hold stock")
	}
}

func TestPlaceRejectedForUnknownProduct(t *testing.T) {
	e := setup(t)
	res, _ := e.svc.Place(ctx, "cust_1", "", items("prd_ghost", 1))
	if res.Order.Status != domain.StatusRejected || res.Order.RejectReason != productclient.ReasonUnknownProduct {
		t.Errorf("order = %+v", res.Order)
	}
}

func TestPlaceValidationErrorsCreateNothing(t *testing.T) {
	e := setup(t)
	_, err := e.svc.Place(ctx, "", "", items("prd_a", 1))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if all, _ := e.store.LoadAll(ctx); len(all) != 0 {
		t.Errorf("%d events stored for an invalid order", len(all))
	}
	if e.fake.reserves != 0 {
		t.Error("Product service was called for an invalid order")
	}
}

// The scenario the whole design exists for: Product is down when the customer
// orders. The order must exist, be honest about its state, and complete by
// itself once Product is back, without double-reserving.
func TestOutageLeavesOrderPlacedAndReconcileFinishesIt(t *testing.T) {
	e := setup(t)
	e.fake.setDown(true)

	res, err := e.svc.Place(ctx, "cust_1", "", items("prd_a", 2))
	if err != nil {
		t.Fatalf("Place must accept the order during an outage, got %v", err)
	}
	if res.Order.Status != domain.StatusPlaced {
		t.Fatalf("status = %s, want placed", res.Order.Status)
	}
	id := res.Order.ID

	// Too soon: the request that created it might still be working on it.
	e.fake.setDown(false)
	if s, _ := e.svc.Reconcile(ctx); s != 0 {
		t.Errorf("Reconcile settled %d orders inside the grace period", s)
	}

	e.clk.advance(6 * time.Second)
	if s, _ := e.svc.Reconcile(ctx); s != 1 {
		t.Fatalf("Reconcile settled %d orders, want 1", s)
	}
	v, _ := e.svc.Projection().Get(id)
	if v.Status != domain.StatusConfirmed {
		t.Errorf("status = %s after reconcile, want confirmed", v.Status)
	}
	if e.fake.stockOf("prd_a") != 8 {
		t.Errorf("stock = %d, want 8 (reserved exactly once)", e.fake.stockOf("prd_a"))
	}
	if s, _ := e.svc.Reconcile(ctx); s != 0 {
		t.Error("Reconcile has nothing left to do but reported work")
	}
}

func TestReconcileKeepsRetryingWhileProductStaysDown(t *testing.T) {
	e := setup(t)
	e.fake.setDown(true)
	res, _ := e.svc.Place(ctx, "cust_1", "", items("prd_a", 1))
	e.clk.advance(time.Minute)
	for i := 0; i < 3; i++ {
		if s, _ := e.svc.Reconcile(ctx); s != 0 {
			t.Fatal("settled while Product is down")
		}
	}
	if v, _ := e.svc.Projection().Get(res.Order.ID); v.Status != domain.StatusPlaced {
		t.Errorf("status = %s, want it to stay placed", v.Status)
	}
}

func TestIdempotencyKeyReturnsTheSameOrder(t *testing.T) {
	e := setup(t)
	first, err := e.svc.Place(ctx, "cust_1", "key-1", items("prd_a", 2))
	if err != nil || first.Replayed {
		t.Fatalf("first: replayed=%v err=%v", first.Replayed, err)
	}
	again, err := e.svc.Place(ctx, "cust_1", "key-1", items("prd_a", 2))
	if err != nil || !again.Replayed || again.Order.ID != first.Order.ID {
		t.Fatalf("retry: replayed=%v id=%s want %s err=%v", again.Replayed, again.Order.ID, first.Order.ID, err)
	}
	if e.fake.stockOf("prd_a") != 8 {
		t.Errorf("stock = %d after retry, want 8: the order was placed twice", e.fake.stockOf("prd_a"))
	}
	if all, _ := e.store.LoadAll(ctx); len(all) != 2 {
		t.Errorf("%d events stored, want 2", len(all))
	}
}

func TestIdempotencyKeyIsScopedToTheCustomerAndTheRequest(t *testing.T) {
	e := setup(t)
	a, _ := e.svc.Place(ctx, "cust_1", "key-1", items("prd_a", 1))
	b, err := e.svc.Place(ctx, "cust_2", "key-1", items("prd_a", 1))
	if err != nil || b.Replayed || b.Order.ID == a.Order.ID {
		t.Errorf("another customer's identical key collided: replayed=%v err=%v", b.Replayed, err)
	}
	if _, err := e.svc.Place(ctx, "cust_1", "key-1", items("prd_a", 5)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("same key, different items: err = %v, want ErrIdempotencyConflict", err)
	}
	// Order of lines is not a different request.
	if _, err := e.svc.Place(ctx, "cust_1", "key-2", items("prd_a", 1, "prd_b", 1)); err != nil {
		t.Fatal(err)
	}
	if r, err := e.svc.Place(ctx, "cust_1", "key-2", items("prd_b", 1, "prd_a", 1)); err != nil || !r.Replayed {
		t.Errorf("reordered lines treated as a new request: replayed=%v err=%v", r.Replayed, err)
	}
}

func TestRetryWithKeyFinishesAnOrderLeftPlaced(t *testing.T) {
	e := setup(t)
	e.fake.setDown(true)
	first, _ := e.svc.Place(ctx, "cust_1", "key-1", items("prd_a", 1))
	if first.Order.Status != domain.StatusPlaced {
		t.Fatal("setup: expected placed")
	}
	e.fake.setDown(false)
	again, err := e.svc.Place(ctx, "cust_1", "key-1", items("prd_a", 1))
	if err != nil || again.Order.Status != domain.StatusConfirmed || !again.Replayed {
		t.Errorf("retry: %+v err=%v", again.Order, err)
	}
}

func TestConcurrentRequestsWithOneKeyCreateOneOrder(t *testing.T) {
	e := setup(t)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.svc.Place(ctx, "cust_1", "same-key", items("prd_a", 1))
			if err != nil {
				t.Errorf("Place: %v", err)
				return
			}
			ids <- r.Order.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Errorf("%d distinct orders from one key", len(seen))
	}
	if e.fake.stockOf("prd_a") != 9 {
		t.Errorf("stock = %d, want 9: reserved more than once", e.fake.stockOf("prd_a"))
	}
}

func TestCancelReleasesStock(t *testing.T) {
	e := setup(t)
	res, _ := e.svc.Place(ctx, "cust_1", "", items("prd_a", 4))
	v, err := e.svc.Cancel(ctx, res.Order.ID, "changed my mind")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != domain.StatusCancelled || v.StockReleasePending {
		t.Errorf("order = %+v", v)
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Errorf("stock = %d, want 10 after cancel", e.fake.stockOf("prd_a"))
	}
}

func TestCancelDuringOutageIsOwedAndPaidLater(t *testing.T) {
	e := setup(t)
	res, _ := e.svc.Place(ctx, "cust_1", "", items("prd_a", 4))
	e.fake.setDown(true)

	v, err := e.svc.Cancel(ctx, res.Order.ID, "")
	if err != nil {
		t.Fatalf("Cancel must succeed even if the release cannot: %v", err)
	}
	if v.Status != domain.StatusCancelled || !v.StockReleasePending {
		t.Fatalf("order = %+v, want cancelled with release pending", v)
	}
	if e.fake.stockOf("prd_a") != 6 {
		t.Fatal("setup: stock should still be held")
	}

	e.fake.setDown(false)
	if _, rel := e.svc.Reconcile(ctx); rel != 1 {
		t.Fatalf("released %d, want 1", rel)
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Errorf("stock = %d after reconcile, want 10", e.fake.stockOf("prd_a"))
	}
	if v, _ := e.svc.Projection().Get(res.Order.ID); v.StockReleasePending {
		t.Error("release still marked pending")
	}
}

func TestCommandsOnTheWrongStateAreRefused(t *testing.T) {
	e := setup(t)
	rejected, _ := e.svc.Place(ctx, "c", "", items("prd_scarce", 9))
	if _, err := e.svc.Cancel(ctx, rejected.Order.ID, ""); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("cancel rejected order: %v", err)
	}
	ok, _ := e.svc.Place(ctx, "c", "", items("prd_a", 1))
	if _, err := e.svc.Ship(ctx, ok.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Cancel(ctx, ok.Order.ID, ""); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("cancel shipped order: %v", err)
	}
	if _, err := e.svc.Ship(ctx, ok.Order.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("ship twice: %v", err)
	}
	if _, err := e.svc.Cancel(ctx, "ord_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancel unknown: %v", err)
	}
}

func TestConcurrentCancelsOnlyOneWins(t *testing.T) {
	e := setup(t)
	res, _ := e.svc.Place(ctx, "c", "", items("prd_a", 3))
	var wg sync.WaitGroup
	var ok, refused atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch _, err := e.svc.Cancel(ctx, res.Order.ID, ""); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, domain.ErrInvalidTransition):
				refused.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || refused.Load() != 11 {
		t.Errorf("ok=%d refused=%d, want 1 and 11", ok.Load(), refused.Load())
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Errorf("stock = %d, want 10 (released once)", e.fake.stockOf("prd_a"))
	}
}

// The event-sourcing claim in one test: throw the read model away, restart on
// the same log, and every order comes back exactly as it was.
func TestRestartRebuildsEveryOrderFromTheLog(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	fake := newFake()

	store1, err := eventstore.OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	e1 := build(t, store1, fake, clk)
	confirmed, _ := e1.svc.Place(ctx, "cust_1", "", items("prd_a", 2))
	rejected, _ := e1.svc.Place(ctx, "cust_1", "", items("prd_scarce", 9))
	cancelled, _ := e1.svc.Place(ctx, "cust_2", "", items("prd_b", 1))
	_, _ = e1.svc.Cancel(ctx, cancelled.Order.ID, "no longer needed")
	fake.setDown(true)
	placed, _ := e1.svc.Place(ctx, "cust_2", "", items("prd_a", 1))
	before := map[string]any{}
	for _, id := range []string{confirmed.Order.ID, rejected.Order.ID, cancelled.Order.ID, placed.Order.ID} {
		v, _ := e1.svc.Projection().Get(id)
		before[id] = fmt.Sprintf("%+v", v)
	}
	_ = store1.Close()

	store2, err := eventstore.OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	e2 := build(t, store2, fake, clk)
	for id, want := range before {
		v, ok := e2.svc.Projection().Get(id)
		if !ok {
			t.Errorf("order %s missing after restart", id)
			continue
		}
		if got := fmt.Sprintf("%+v", v); got != want {
			t.Errorf("order %s changed across restart\n before: %s\n after:  %s", id, want, got)
		}
	}
	// The order that was still placed must still be finishable after restart.
	fake.setDown(false)
	clk.advance(time.Minute)
	if s, _ := e2.svc.Reconcile(ctx); s != 1 {
		t.Errorf("Reconcile after restart settled %d, want 1", s)
	}
}
