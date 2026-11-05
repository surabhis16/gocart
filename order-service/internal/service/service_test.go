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
	"gocart/order-service/internal/paymentclient"
	"gocart/order-service/internal/productclient"
	"gocart/order-service/internal/projection"
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

// fakePayments stands in for the Payment service, with the same contract: a
// charge is identified by its order, asking twice never charges twice, a
// cancelled order cannot be charged, and cancelling refunds a taken charge.
type fakePayments struct {
	mu      sync.Mutex
	down    bool
	charges map[string]*fakeCharge
	calls   int // Charge calls, including repeats
	cancels int

	// loseResponse makes the provider take the money but the caller never hear
	// about it, the situation that makes "unavailable" different from "declined".
	loseResponse bool
	// beforeCharge and afterRecord run, outside the lock, around the provider
	// call, so a test can make something else happen while a charge is in flight.
	beforeCharge func()
	afterRecord  func()
}

type fakeCharge struct {
	amount   int64
	currency string
	state    string // approved, voided
	ref      string
}

func newFakePayments() *fakePayments { return &fakePayments{charges: map[string]*fakeCharge{}} }

func (f *fakePayments) setDown(d bool) { f.mu.Lock(); f.down = d; f.mu.Unlock() }

// count returns how many charges are in the given state.
func (f *fakePayments) count(state string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.charges {
		if c.state == state {
			n++
		}
	}
	return n
}

func (f *fakePayments) Charge(_ context.Context, orderID string, amount int64, currency, token string) (paymentclient.Charge, error) {
	f.mu.Lock()
	f.calls++
	before := f.beforeCharge
	f.mu.Unlock()
	if before != nil {
		before()
	}

	f.mu.Lock()
	if f.down {
		f.mu.Unlock()
		return paymentclient.Charge{}, fmt.Errorf("%w: connection refused", paymentclient.ErrUnavailable)
	}
	c, ok := f.charges[orderID]
	switch {
	case ok && c.state == "voided":
		f.mu.Unlock()
		return paymentclient.Charge{}, paymentclient.ErrVoided
	case ok:
		ref := c.ref
		lose := f.loseResponse
		f.mu.Unlock()
		if lose {
			return paymentclient.Charge{}, fmt.Errorf("%w: timeout", paymentclient.ErrUnavailable)
		}
		return paymentclient.Charge{OrderID: orderID, Reference: ref}, nil
	}
	if token == "tok_declined" {
		f.mu.Unlock()
		return paymentclient.Charge{}, &paymentclient.DeclinedError{Code: "card_declined"}
	}
	c = &fakeCharge{amount: amount, currency: currency, state: "approved", ref: "pay_" + orderID}
	f.charges[orderID] = c
	lose, after := f.loseResponse, f.afterRecord
	f.mu.Unlock()
	if after != nil {
		after()
	}
	if lose {
		return paymentclient.Charge{}, fmt.Errorf("%w: timeout", paymentclient.ErrUnavailable)
	}
	return paymentclient.Charge{OrderID: orderID, Reference: c.ref}, nil
}

func (f *fakePayments) Cancel(_ context.Context, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	if f.down {
		return fmt.Errorf("%w: connection refused", paymentclient.ErrUnavailable)
	}
	if c, ok := f.charges[orderID]; ok {
		c.state = "voided"
	} else {
		f.charges[orderID] = &fakeCharge{state: "voided"} // the tombstone
	}
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
	pay   *fakePayments
	store eventstore.Store
	clk   *clock
}

const ttl = 10 * time.Minute

func setup(t *testing.T) *env {
	t.Helper()
	store, err := eventstore.OpenFile("")
	if err != nil {
		t.Fatal(err)
	}
	return build(t, store, newFake(), newFakePayments(), &clock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)})
}

func build(t *testing.T, store eventstore.Store, fake *fakeProducts, pay *fakePayments, clk *clock) *env {
	t.Helper()
	svc, err := New(store, fake, pay, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithClock(clk.now), WithGrace(5*time.Second), WithPaymentTTL(ttl))
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, fake: fake, pay: pay, store: store, clk: clk}
}

// place orders with a good card unless the test says otherwise.
func (e *env) place(customer, key string, its []domain.Item) (PlaceResult, error) {
	return e.svc.Place(ctx, customer, key, "tok_visa", its)
}

var ctx = context.Background()

func items(pairs ...any) []domain.Item {
	var out []domain.Item
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, domain.Item{ProductID: pairs[i].(string), Quantity: pairs[i+1].(int)})
	}
	return out
}

func TestPlaceConfirmsPricesAndChargesTheOrder(t *testing.T) {
	e := setup(t)
	res, err := e.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_a", 2, "prd_b", 1))
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order
	if o.Status != domain.StatusConfirmed || o.TotalMinor != 3000 || o.Currency != "INR" || o.Version != 3 {
		t.Errorf("order = %+v", o)
	}
	if e.fake.stockOf("prd_a") != 8 || e.fake.stockOf("prd_b") != 4 {
		t.Error("stock was not deducted")
	}
	if o.PaymentReference == "" || e.pay.count("approved") != 1 {
		t.Errorf("payment: reference=%q approved=%d", o.PaymentReference, e.pay.count("approved"))
	}
	if c := e.pay.charges[o.ID]; c.amount != 3000 || c.currency != "INR" {
		t.Errorf("charged %+v, want exactly the reserved total", c)
	}
}

func TestPlaceRejectedWhenStockIsShort(t *testing.T) {
	e := setup(t)
	res, err := e.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_scarce", 3))
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
	res, _ := e.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_ghost", 1))
	if res.Order.Status != domain.StatusRejected || res.Order.RejectReason != productclient.ReasonUnknownProduct {
		t.Errorf("order = %+v", res.Order)
	}
}

func TestPlaceValidationErrorsCreateNothing(t *testing.T) {
	e := setup(t)
	_, err := e.svc.Place(ctx, "", "", "tok_visa", items("prd_a", 1))
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

	res, err := e.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_a", 2))
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
	res, _ := e.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_a", 1))
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
	first, err := e.svc.Place(ctx, "cust_1", "key-1", "tok_visa", items("prd_a", 2))
	if err != nil || first.Replayed {
		t.Fatalf("first: replayed=%v err=%v", first.Replayed, err)
	}
	again, err := e.svc.Place(ctx, "cust_1", "key-1", "tok_visa", items("prd_a", 2))
	if err != nil || !again.Replayed || again.Order.ID != first.Order.ID {
		t.Fatalf("retry: replayed=%v id=%s want %s err=%v", again.Replayed, again.Order.ID, first.Order.ID, err)
	}
	if e.fake.stockOf("prd_a") != 8 {
		t.Errorf("stock = %d after retry, want 8: the order was placed twice", e.fake.stockOf("prd_a"))
	}
	if all, _ := e.store.LoadAll(ctx); len(all) != 3 {
		t.Errorf("%d events stored, want 3 (placed, stock reserved, payment captured)", len(all))
	}
}

func TestIdempotencyKeyIsScopedToTheCustomerAndTheRequest(t *testing.T) {
	e := setup(t)
	a, _ := e.svc.Place(ctx, "cust_1", "key-1", "tok_visa", items("prd_a", 1))
	b, err := e.svc.Place(ctx, "cust_2", "key-1", "tok_visa", items("prd_a", 1))
	if err != nil || b.Replayed || b.Order.ID == a.Order.ID {
		t.Errorf("another customer's identical key collided: replayed=%v err=%v", b.Replayed, err)
	}
	if _, err := e.svc.Place(ctx, "cust_1", "key-1", "tok_visa", items("prd_a", 5)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("same key, different items: err = %v, want ErrIdempotencyConflict", err)
	}
	// Order of lines is not a different request.
	if _, err := e.svc.Place(ctx, "cust_1", "key-2", "tok_visa", items("prd_a", 1, "prd_b", 1)); err != nil {
		t.Fatal(err)
	}
	if r, err := e.svc.Place(ctx, "cust_1", "key-2", "tok_visa", items("prd_b", 1, "prd_a", 1)); err != nil || !r.Replayed {
		t.Errorf("reordered lines treated as a new request: replayed=%v err=%v", r.Replayed, err)
	}
}

func TestRetryWithKeyFinishesAnOrderLeftPlaced(t *testing.T) {
	e := setup(t)
	e.fake.setDown(true)
	first, _ := e.svc.Place(ctx, "cust_1", "key-1", "tok_visa", items("prd_a", 1))
	if first.Order.Status != domain.StatusPlaced {
		t.Fatal("setup: expected placed")
	}
	e.fake.setDown(false)
	again, err := e.svc.Place(ctx, "cust_1", "key-1", "tok_visa", items("prd_a", 1))
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
			r, err := e.svc.Place(ctx, "cust_1", "same-key", "tok_visa", items("prd_a", 1))
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
	res, _ := e.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_a", 4))
	v, err := e.svc.Cancel(ctx, res.Order.ID, "changed my mind")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != domain.StatusCancelled || v.StockReleasePending || v.RefundPending {
		t.Errorf("order = %+v", v)
	}
	if e.pay.count("voided") != 1 {
		t.Error("the payment was not refunded")
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Errorf("stock = %d, want 10 after cancel", e.fake.stockOf("prd_a"))
	}
}

func TestCancelDuringOutageIsOwedAndPaidLater(t *testing.T) {
	e := setup(t)
	res, _ := e.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_a", 4))
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
	rejected, _ := e.svc.Place(ctx, "c", "", "tok_visa", items("prd_scarce", 9))
	if _, err := e.svc.Cancel(ctx, rejected.Order.ID, ""); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("cancel rejected order: %v", err)
	}
	ok, _ := e.svc.Place(ctx, "c", "", "tok_visa", items("prd_a", 1))
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
	res, _ := e.svc.Place(ctx, "c", "", "tok_visa", items("prd_a", 3))
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
	pay := newFakePayments()
	e1 := build(t, store1, fake, pay, clk)
	confirmed, _ := e1.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_a", 2))
	rejected, _ := e1.svc.Place(ctx, "cust_1", "", "tok_visa", items("prd_scarce", 9))
	cancelled, _ := e1.svc.Place(ctx, "cust_2", "", "tok_visa", items("prd_b", 1))
	_, _ = e1.svc.Cancel(ctx, cancelled.Order.ID, "no longer needed")
	fake.setDown(true)
	placed, _ := e1.svc.Place(ctx, "cust_2", "", "tok_visa", items("prd_a", 1))
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
	e2 := build(t, store2, fake, pay, clk)
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

// ---- payments: the second half of the saga ----

func status(e *env, id string) domain.Status {
	v, ok := e.svc.Projection().Get(id)
	if !ok {
		return ""
	}
	return v.Status
}

func TestDeclinedCardFailsTheOrderAndReturnsTheStock(t *testing.T) {
	e := setup(t)
	res, err := e.svc.Place(ctx, "cust_1", "", "tok_declined", items("prd_a", 3))
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order
	if o.Status != domain.StatusPaymentFailed || o.DeclineCode != "card_declined" {
		t.Fatalf("order = %+v", o)
	}
	if o.StockReleasePending || o.RefundPending {
		t.Errorf("debts left over: %+v", o)
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Errorf("stock = %d, want 10: a declined order must give its stock back", e.fake.stockOf("prd_a"))
	}
	if e.pay.count("approved") != 0 {
		t.Error("a declined order has a charge")
	}
	// Definite: no amount of reconciling retries a decline.
	e.clk.advance(time.Hour)
	callsBefore := e.pay.calls
	e.svc.Reconcile(ctx)
	if e.pay.calls != callsBefore {
		t.Error("the reconciler asked the card network again after a decline")
	}
}

func TestDeclineWhileProductIsDownOwesStockUntilReconciled(t *testing.T) {
	e := setup(t)
	// Stock is reserved, then Product goes down before the decline lands.
	e.pay.beforeCharge = func() { e.fake.setDown(true) }
	res, _ := e.svc.Place(ctx, "cust_1", "", "tok_declined", items("prd_a", 3))
	e.pay.beforeCharge = nil
	if res.Order.Status != domain.StatusPaymentFailed || !res.Order.StockReleasePending {
		t.Fatalf("order = %+v, want payment_failed with the release pending", res.Order)
	}
	if e.fake.stockOf("prd_a") != 7 {
		t.Fatal("setup: stock should still be held")
	}
	e.fake.setDown(false)
	if _, n := e.svc.Reconcile(ctx); n != 1 {
		t.Fatalf("compensated %d, want 1", n)
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Errorf("stock = %d, want 10", e.fake.stockOf("prd_a"))
	}
}

// Payment is down. The customer must get an honest "we are working on it", the
// stock must stay held, and the order must finish by itself, charged once.
func TestPaymentOutageLeavesOrderAwaitingAndReconcileFinishesIt(t *testing.T) {
	e := setup(t)
	e.pay.setDown(true)
	res, err := e.place("cust_1", "", items("prd_a", 2))
	if err != nil {
		t.Fatalf("Place must accept the order during a payment outage, got %v", err)
	}
	if res.Order.Status != domain.StatusAwaitingPayment {
		t.Fatalf("status = %s, want awaiting_payment", res.Order.Status)
	}
	if e.fake.stockOf("prd_a") != 8 {
		t.Error("stock must stay held while payment is pending")
	}
	id := res.Order.ID

	e.pay.setDown(false)
	if s, _ := e.svc.Reconcile(ctx); s != 0 {
		t.Errorf("Reconcile settled %d inside the grace period", s)
	}
	e.clk.advance(6 * time.Second)
	if s, _ := e.svc.Reconcile(ctx); s != 1 {
		t.Fatalf("Reconcile settled %d, want 1", s)
	}
	if status(e, id) != domain.StatusConfirmed {
		t.Errorf("status = %s, want confirmed", status(e, id))
	}
	if e.pay.count("approved") != 1 {
		t.Errorf("%d charges, want exactly 1", e.pay.count("approved"))
	}
	if e.fake.stockOf("prd_a") != 8 {
		t.Errorf("stock = %d, want 8 (reserved once)", e.fake.stockOf("prd_a"))
	}
}

// The hardest case in payments: the provider took the money, and the answer
// was lost. Treating that as a decline would cancel an order that was paid for.
// Treating it as success without asking would be a guess. The saga does neither:
// it asks again, and the provider says what it already recorded.
func TestLostResponseIsNotADeclineAndIsNeverChargedTwice(t *testing.T) {
	e := setup(t)
	e.pay.loseResponse = true
	res, _ := e.place("cust_1", "", items("prd_a", 1))
	if res.Order.Status != domain.StatusAwaitingPayment {
		t.Fatalf("status = %s, want awaiting_payment", res.Order.Status)
	}
	if e.pay.count("approved") != 1 {
		t.Fatal("setup: the provider should have taken the money")
	}

	e.pay.loseResponse = false
	e.clk.advance(6 * time.Second)
	e.svc.Reconcile(ctx)
	if status(e, res.Order.ID) != domain.StatusConfirmed {
		t.Errorf("status = %s, want confirmed", status(e, res.Order.ID))
	}
	if n := e.pay.count("approved"); n != 1 {
		t.Errorf("%d charges, want 1: the retry charged again", n)
	}
	if e.pay.calls != 2 {
		t.Errorf("provider asked %d times, want 2 (the original and one retry)", e.pay.calls)
	}
}

func TestRetryWithKeyFinishesAnOrderAwaitingPayment(t *testing.T) {
	e := setup(t)
	e.pay.setDown(true)
	first, _ := e.place("cust_1", "key-1", items("prd_a", 1))
	if first.Order.Status != domain.StatusAwaitingPayment {
		t.Fatal("setup: expected awaiting_payment")
	}
	e.pay.setDown(false)
	again, err := e.place("cust_1", "key-1", items("prd_a", 1))
	if err != nil || again.Order.Status != domain.StatusConfirmed || !again.Replayed {
		t.Errorf("retry: %+v err=%v", again.Order, err)
	}
	if e.pay.count("approved") != 1 {
		t.Error("charged more than once")
	}
}

func TestSameKeyWithADifferentCardIsADifferentRequest(t *testing.T) {
	e := setup(t)
	if _, err := e.svc.Place(ctx, "c", "k", "tok_visa", items("prd_a", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Place(ctx, "c", "k", "tok_other", items("prd_a", 1)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCardNumberIsRefusedBeforeAnythingIsStoredOrCalled(t *testing.T) {
	e := setup(t)
	_, err := e.svc.Place(ctx, "c", "", "4242424242424242", items("prd_a", 1))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v", err)
	}
	if all, _ := e.store.LoadAll(ctx); len(all) != 0 {
		t.Error("a refused order left events in the log")
	}
	if e.fake.reserves != 0 || e.pay.calls != 0 {
		t.Error("a refused order reached another service")
	}
}

// ---- expiry: stock must not be held forever ----

func TestUnpaidOrderExpiresAndGivesEverythingBack(t *testing.T) {
	e := setup(t)
	e.pay.setDown(true)
	res, _ := e.place("cust_1", "", items("prd_a", 4))
	id := res.Order.ID
	if e.fake.stockOf("prd_a") != 6 {
		t.Fatal("setup: stock should be held")
	}

	// Inside the time limit, with payment still down, the order just waits.
	e.clk.advance(ttl - time.Second)
	e.svc.Reconcile(ctx)
	if status(e, id) != domain.StatusAwaitingPayment {
		t.Fatalf("status = %s, want it to keep waiting until the limit", status(e, id))
	}

	e.clk.advance(2 * time.Second)
	e.pay.setDown(false)
	e.svc.Reconcile(ctx)
	v, _ := e.svc.Projection().Get(id)
	if v.Status != domain.StatusExpired || v.StockReleasePending || v.RefundPending {
		t.Fatalf("order = %+v, want expired with nothing owed", v)
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Errorf("stock = %d, want 10: expiry must release the stock", e.fake.stockOf("prd_a"))
	}
	if e.pay.count("approved") != 0 {
		t.Error("an expired order must not hold a charge")
	}
}

// When the order expires the charge might be in flight. The void that follows
// must make a late charge fail, instead of letting it land on a dead order.
func TestExpiredOrderCannotBeChargedLater(t *testing.T) {
	e := setup(t)
	e.pay.setDown(true)
	res, _ := e.place("cust_1", "", items("prd_a", 1))
	e.clk.advance(ttl + time.Second)
	e.pay.setDown(false)
	e.svc.Reconcile(ctx)
	if status(e, res.Order.ID) != domain.StatusExpired {
		t.Fatal("setup: expected expired")
	}
	_, err := e.pay.Charge(ctx, res.Order.ID, 1000, "INR", "tok_visa")
	if !errors.Is(err, paymentclient.ErrVoided) {
		t.Errorf("a late charge for an expired order returned %v, want ErrVoided", err)
	}
}

// The money was taken but the answer was lost, and then the order expired.
// Expiry must hand the money back.
func TestExpiryRefundsAChargeWhoseAnswerWasLost(t *testing.T) {
	e := setup(t)
	e.pay.loseResponse = true
	res, _ := e.place("cust_1", "", items("prd_a", 1))
	if e.pay.count("approved") != 1 {
		t.Fatal("setup: the provider should have taken the money")
	}
	e.pay.setDown(true) // and now nobody can confirm it
	e.clk.advance(ttl + time.Second)
	e.svc.Reconcile(ctx)
	v, _ := e.svc.Projection().Get(res.Order.ID)
	if v.Status != domain.StatusExpired || !v.RefundPending {
		t.Fatalf("order = %+v, want expired with the refund still owed", v)
	}

	e.pay.setDown(false)
	e.svc.Reconcile(ctx)
	if e.pay.count("approved") != 0 || e.pay.count("voided") != 1 {
		t.Errorf("approved=%d voided=%d: the money was not given back", e.pay.count("approved"), e.pay.count("voided"))
	}
	if v, _ := e.svc.Projection().Get(res.Order.ID); v.RefundPending || v.StockReleasePending {
		t.Errorf("debts remain: %+v", v)
	}
}

// The race: the order expires while a charge is in flight. Whatever the
// provider decides, the customer must not end up charged for a dead order.
func TestOrderExpiringDuringAChargeEndsUpRefunded(t *testing.T) {
	t.Run("expiry lands before the provider records the charge", func(t *testing.T) {
		e := setup(t)
		e.pay.beforeCharge = func() {
			e.clk.advance(ttl + time.Second)
			e.svc.expireIfStale(ctx, e.onlyOrderID(t))
		}
		res, _ := e.place("c", "", items("prd_a", 1))
		if res.Order.Status != domain.StatusExpired {
			t.Fatalf("status = %s, want expired", res.Order.Status)
		}
		if e.pay.count("approved") != 0 {
			t.Error("a charge landed on an expired order")
		}
		if e.fake.stockOf("prd_a") != 10 {
			t.Error("stock not returned")
		}
	})

	t.Run("expiry lands after the provider recorded the charge", func(t *testing.T) {
		e := setup(t)
		e.pay.afterRecord = func() {
			e.clk.advance(ttl + time.Second)
			e.svc.expireIfStale(ctx, e.onlyOrderID(t))
		}
		res, _ := e.place("c", "", items("prd_a", 1))
		if res.Order.Status != domain.StatusExpired {
			t.Fatalf("status = %s, want expired: a payment must not confirm an expired order", res.Order.Status)
		}
		if e.pay.count("approved") != 0 || e.pay.count("voided") != 1 {
			t.Errorf("approved=%d voided=%d: the customer is out of pocket", e.pay.count("approved"), e.pay.count("voided"))
		}
		if v, _ := e.svc.Projection().Get(res.Order.ID); v.RefundPending || v.StockReleasePending {
			t.Errorf("debts remain: %+v", v)
		}
	})
}

// onlyOrderID returns the id of the single order in the read model.
func (e *env) onlyOrderID(t *testing.T) string {
	t.Helper()
	all := e.svc.Projection().List(projection.Filter{})
	if len(all) != 1 {
		t.Fatalf("expected one order, found %d", len(all))
	}
	return all[0].ID
}

// ---- cancelling a paid order ----

func TestCancelRefundsThePayment(t *testing.T) {
	e := setup(t)
	res, _ := e.place("c", "", items("prd_a", 2))
	v, err := e.svc.Cancel(ctx, res.Order.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.RefundPending || v.StockReleasePending || e.pay.count("voided") != 1 || e.pay.count("approved") != 0 {
		t.Errorf("order=%+v voided=%d", v, e.pay.count("voided"))
	}
}

// Stock and money are separate debts. Paying one must not wait for the other.
func TestCancelWithPaymentDownStillReturnsTheStock(t *testing.T) {
	e := setup(t)
	res, _ := e.place("c", "", items("prd_a", 4))
	e.pay.setDown(true)
	v, err := e.svc.Cancel(ctx, res.Order.ID, "")
	if err != nil {
		t.Fatalf("Cancel must succeed even if the refund cannot: %v", err)
	}
	if v.StockReleasePending || !v.RefundPending {
		t.Fatalf("order = %+v, want stock released and refund pending", v)
	}
	if e.fake.stockOf("prd_a") != 10 {
		t.Error("the stock release waited for the refund")
	}

	e.pay.setDown(false)
	if _, n := e.svc.Reconcile(ctx); n != 1 {
		t.Fatalf("compensated %d, want 1", n)
	}
	if v, _ := e.svc.Projection().Get(res.Order.ID); v.RefundPending {
		t.Error("refund still pending")
	}
	if e.pay.count("voided") != 1 {
		t.Error("payment not refunded")
	}
}

func TestCancelWithBothServicesDownOwesBothAndPaysBothLater(t *testing.T) {
	e := setup(t)
	res, _ := e.place("c", "", items("prd_a", 4))
	e.pay.setDown(true)
	e.fake.setDown(true)
	v, err := e.svc.Cancel(ctx, res.Order.ID, "")
	if err != nil || !v.StockReleasePending || !v.RefundPending {
		t.Fatalf("order=%+v err=%v", v, err)
	}
	e.pay.setDown(false)
	e.fake.setDown(false)
	e.svc.Reconcile(ctx)
	if v, _ := e.svc.Projection().Get(res.Order.ID); v.StockReleasePending || v.RefundPending {
		t.Errorf("debts remain: %+v", v)
	}
}

// ---- managing change: a log written by version 0.1 ----

func TestVersion01OrdersSurviveTheUpgrade(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	store, _ := eventstore.OpenFile("")
	for _, ev := range []domain.Event{
		{StreamID: "ord_legacy", Version: 1, Type: domain.TypeOrderPlaced, At: at,
			Data: []byte(`{"customer_id":"cust_old","items":[{"product_id":"prd_a","quantity":2}]}`)},
		{StreamID: "ord_legacy", Version: 2, Type: domain.TypeOrderConfirmed, At: at,
			Data: []byte(`{"items":[{"product_id":"prd_a","name":"A","quantity":2,"unit_price_minor":1000}],"total_minor":2000,"currency":"INR"}`)},
	} {
		if err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	e := build(t, store, newFake(), newFakePayments(), &clock{t: at.Add(time.Hour)})

	v, ok := e.svc.Projection().Get("ord_legacy")
	if !ok || v.Status != domain.StatusConfirmed || v.TotalMinor != 2000 {
		t.Fatalf("legacy order did not load: %+v ok=%v", v, ok)
	}
	// Never charged, so nothing to refund and nothing to ask the Payment service.
	v, err := e.svc.Cancel(ctx, "ord_legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.RefundPending || e.pay.cancels != 0 {
		t.Errorf("a never-charged order tried to refund: %+v cancels=%d", v, e.pay.cancels)
	}
}

// Reconcile must be harmless to run repeatedly and concurrently with requests.
func TestReconcileIsIdempotentUnderConcurrency(t *testing.T) {
	e := setup(t)
	e.pay.setDown(true)
	var ids []string
	for i := 0; i < 5; i++ {
		r, _ := e.place("c", "", items("prd_a", 1))
		ids = append(ids, r.Order.ID)
	}
	e.pay.setDown(false)
	e.clk.advance(6 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.svc.Reconcile(ctx) }()
	}
	wg.Wait()
	for _, id := range ids {
		if status(e, id) != domain.StatusConfirmed {
			t.Errorf("%s: %s", id, status(e, id))
		}
	}
	if e.pay.count("approved") != 5 {
		t.Errorf("%d charges, want 5", e.pay.count("approved"))
	}
	if e.fake.stockOf("prd_a") != 5 {
		t.Errorf("stock = %d, want 5", e.fake.stockOf("prd_a"))
	}
}
