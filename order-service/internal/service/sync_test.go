package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/eventstore"
	"gocart/order-service/internal/projection"
)

// twoReplicas builds two Order services over ONE event store and ONE set of
// downstream services: what two pods of the Order service look like.
func twoReplicas(t *testing.T) (a, b *env) {
	t.Helper()
	store, err := eventstore.OpenFile("")
	if err != nil {
		t.Fatal(err)
	}
	fake, pay := newFake(), newFakePayments()
	clk := &clock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	a = build(t, store, fake, pay, clk)
	b = build(t, store, fake, pay, clk)
	return a, b
}

func allViews(e *env) []projection.View {
	return e.svc.Projection().List(projection.Filter{Limit: 200})
}

func TestAReplicaLearnsOfOrdersPlacedThroughAnother(t *testing.T) {
	a, b := twoReplicas(t)
	res, err := a.place("cust_1", "", items("prd_a", 1))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.ID

	if _, ok := b.svc.Projection().Get(id); ok {
		t.Fatal("precondition: the other replica should not know the order yet")
	}
	if err := b.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	v, ok := b.svc.Projection().Get(id)
	if !ok || v.Status != domain.StatusConfirmed {
		t.Fatalf("after Sync: %+v ok=%v", v, ok)
	}
	if !reflect.DeepEqual(allViews(a), allViews(b)) {
		t.Errorf("views differ after sync\nA: %+v\nB: %+v", allViews(a), allViews(b))
	}
}

func TestACommandOnAnotherReplicasOrderWorksAndHealsTheViewAtOnce(t *testing.T) {
	a, b := twoReplicas(t)
	res, _ := a.place("cust_1", "", items("prd_a", 2))
	id := res.Order.ID

	// B has never heard of this order, yet cancelling it through B must work:
	// commands decide from the store, not from the view.
	v, err := b.svc.Cancel(ctx, id, "changed my mind")
	if err != nil {
		t.Fatalf("cancel through the other replica: %v", err)
	}
	if v.Status != domain.StatusCancelled {
		t.Fatalf("status = %s", v.Status)
	}
	// B's view caught up on the spot (it reloaded the order's history when it
	// could not apply the new event), so no Sync was needed for B.
	got, ok := b.svc.Projection().Get(id)
	if !ok || got.Status != domain.StatusCancelled || got.Version != 6 {
		t.Errorf("B's view: %+v ok=%v", got, ok)
	}
	// A is behind until it syncs.
	if mine, _ := a.svc.Projection().Get(id); mine.Status != domain.StatusConfirmed {
		t.Errorf("precondition: A should still show confirmed, shows %s", mine.Status)
	}
	if err := a.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if mine, _ := a.svc.Projection().Get(id); mine.Status != domain.StatusCancelled || mine.RefundPending || mine.StockReleasePending {
		t.Errorf("A after sync: %+v", mine)
	}
	if a.fake.stockOf("prd_a") != 10 || a.pay.count("voided") != 1 {
		t.Errorf("stock=%d voided=%d, want everything given back", a.fake.stockOf("prd_a"), a.pay.count("voided"))
	}
}

func TestReplicasStayConsistentUnderConcurrentWork(t *testing.T) {
	a, b := twoReplicas(t)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := a
			if i%2 == 1 {
				r = b
			}
			res, err := r.svc.Place(ctx, fmt.Sprintf("cust_%d", i%5), fmt.Sprintf("key-%d", i), "tok_visa", items("prd_a", 1))
			if err != nil {
				return // stock runs out partway; rejections are fine here
			}
			if i%3 == 0 && res.Order.Status == domain.StatusConfirmed {
				other := b
				if i%2 == 1 {
					other = a
				}
				_, _ = other.svc.Cancel(ctx, res.Order.ID, "")
			}
		}(i)
	}
	wg.Wait()
	for _, e := range []*env{a, b} {
		if err := e.svc.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	va, vb := allViews(a), allViews(b)
	if len(va) == 0 {
		t.Fatal("no orders were created")
	}
	if !reflect.DeepEqual(va, vb) {
		t.Fatalf("the two replicas disagree\nA: %d orders\nB: %d orders", len(va), len(vb))
	}
	// And both agree with the truth, rebuilt from the log from scratch.
	events, _ := a.store.LoadAll(ctx)
	fresh := projection.New()
	if err := fresh.Rebuild(events); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(va, fresh.List(projection.Filter{Limit: 200})) {
		t.Error("the replicas' view differs from a rebuild of the log")
	}
}

func TestTwoReplicasReconcilingTheSameBacklogFinishEachOrderOnce(t *testing.T) {
	a, b := twoReplicas(t)
	a.fake.setDown(true) // shared: both replicas see Product down
	var ids []string
	for i := 0; i < 6; i++ {
		res, err := a.place(fmt.Sprintf("cust_%d", i), fmt.Sprintf("k%d", i), items("prd_a", 1))
		if err != nil || res.Order.Status != domain.StatusPlaced {
			t.Fatalf("order %d: %+v %v", i, res.Order.Status, err)
		}
		ids = append(ids, res.Order.ID)
	}
	if err := b.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	a.fake.setDown(false)
	a.clk.advance(time.Minute)

	var wg sync.WaitGroup
	for _, e := range []*env{a, b, a, b} {
		wg.Add(1)
		go func(e *env) { defer wg.Done(); e.svc.Reconcile(ctx) }(e)
	}
	wg.Wait()
	for _, e := range []*env{a, b} {
		_ = e.svc.Sync(ctx)
	}
	for _, id := range ids {
		for name, e := range map[string]*env{"A": a, "B": b} {
			if v, _ := e.svc.Projection().Get(id); v.Status != domain.StatusConfirmed {
				t.Errorf("replica %s: order %s is %s", name, id, v.Status)
			}
		}
		history, _ := a.svc.History(ctx, id)
		var types []string
		for _, ev := range history {
			types = append(types, ev.Type)
		}
		if fmt.Sprint(types) != "[OrderPlaced StockReserved PaymentCaptured]" {
			t.Errorf("order %s history = %v, want each step recorded exactly once", id, types)
		}
	}
	if a.fake.stockOf("prd_a") != 4 {
		t.Errorf("stock = %d, want 4 (6 reserved once each)", a.fake.stockOf("prd_a"))
	}
	if a.pay.count("approved") != 6 {
		t.Errorf("%d charges, want 6", a.pay.count("approved"))
	}
}

// lossyFeed wraps a store whose change feed names nothing: a writer that died
// between appending and recording the change, every time.
type lossyFeed struct{ eventstore.Store }

func (lossyFeed) ChangedSince(context.Context, time.Time) ([]string, error) { return nil, nil }

func TestAFullResyncCatchesWhatTheFeedMissed(t *testing.T) {
	file, _ := eventstore.OpenFile("")
	fake, pay := newFake(), newFakePayments()
	clk := &clock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	a := build(t, file, fake, pay, clk)
	b := build(t, lossyFeed{file}, fake, pay, clk)
	b.svc.resyncEvery = 20 * time.Millisecond

	res, _ := a.place("c", "", items("prd_a", 1))
	if err := b.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.svc.Projection().Get(res.Order.ID); ok {
		t.Fatal("the lossy feed should have missed the order")
	}
	time.Sleep(40 * time.Millisecond)
	if err := b.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if v, ok := b.svc.Projection().Get(res.Order.ID); !ok || v.Status != domain.StatusConfirmed {
		t.Errorf("the full resync did not catch it: %+v ok=%v", v, ok)
	}
}

// flakyFeed fails its first read.
type flakyFeed struct {
	eventstore.Store
	failed bool
}

func (f *flakyFeed) ChangedSince(ctx context.Context, since time.Time) ([]string, error) {
	if !f.failed {
		f.failed = true
		return nil, errors.New("cassandra is having a moment")
	}
	return f.Store.ChangedSince(ctx, since)
}

func TestAFailedSyncIsRetriedWithoutLosingAnything(t *testing.T) {
	file, _ := eventstore.OpenFile("")
	fake, pay := newFake(), newFakePayments()
	clk := &clock{t: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	a := build(t, file, fake, pay, clk)
	b := build(t, &flakyFeed{Store: file}, fake, pay, clk)

	res, _ := a.place("c", "", items("prd_a", 1))
	if err := b.svc.Sync(ctx); err == nil {
		t.Fatal("expected the first Sync to fail")
	}
	if err := b.svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.svc.Projection().Get(res.Order.ID); !ok {
		t.Error("the order was lost across a failed sync")
	}
}

func TestRunSyncKeepsAReplicaCurrentUntilStopped(t *testing.T) {
	a, b := twoReplicas(t)
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { b.svc.RunSync(c, 5*time.Millisecond); close(done) }()

	res, _ := a.place("c", "", items("prd_a", 1))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := b.svc.Projection().Get(res.Order.ID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background sync never delivered the order")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunSync did not stop")
	}
}

func TestACustomerRetryingThroughAnotherReplicaGetsTheSameOrder(t *testing.T) {
	a, b := twoReplicas(t)
	first, err := a.place("cust_1", "retry-key", items("prd_a", 1))
	if err != nil {
		t.Fatal(err)
	}
	// The retry lands on a replica that has not synced. It must find the order
	// in the store, not create a second one or report that it does not exist.
	again, err := b.place("cust_1", "retry-key", items("prd_a", 1))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Order.ID != first.Order.ID {
		t.Errorf("replayed=%v id=%s, want the original %s", again.Replayed, again.Order.ID, first.Order.ID)
	}
	if a.fake.stockOf("prd_a") != 9 {
		t.Errorf("stock = %d, want 9 (reserved once)", a.fake.stockOf("prd_a"))
	}
}

func TestPollingTheLocationOfAnOrderCreatedElsewhereWorks(t *testing.T) {
	a, b := twoReplicas(t)
	res, _ := a.place("cust_1", "", items("prd_a", 1))
	v, err := b.svc.Order(res.Order.ID)
	if err != nil || v.Status != domain.StatusConfirmed {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := b.svc.Order("ord_does_not_exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
