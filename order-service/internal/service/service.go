// Package service is the Order service's command side: it decides, stores
// events, and coordinates with the Product service.
//
// The two services cannot share a transaction, so an order is a small saga:
//
//  1. store OrderPlaced               (the order exists, durably)
//  2. ask Product to reserve stock    (idempotent per order id)
//  3. store OrderConfirmed or OrderRejected
//
// A crash, timeout or outage between any two steps leaves the order "placed",
// which is a real, queryable state rather than a corrupt one. Reconcile finds
// such orders and finishes them. Because step 2 is idempotent, finishing is
// always safe, and nothing is ever double-reserved. Cancelling works the same
// way in reverse: OrderCancelled is stored first, and the stock release that
// follows is retried until it lands.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gocart/order-service/internal/domain"
	"gocart/order-service/internal/eventstore"
	"gocart/order-service/internal/productclient"
	"gocart/order-service/internal/projection"
	"gocart/order-service/internal/reqid"
)

var (
	// ErrNotFound means no such order.
	ErrNotFound = errors.New("order not found")
	// ErrIdempotencyConflict means an Idempotency-Key was reused with a
	// different request.
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")
	// ErrContention means an order kept changing underneath us.
	ErrContention = errors.New("order is being modified concurrently; try again")
)

// Products is what the Order service needs from the Product service.
type Products interface {
	Reserve(ctx context.Context, orderID string, items []domain.Item) (productclient.Reservation, error)
	Release(ctx context.Context, orderID string) error
}

// Service coordinates orders.
type Service struct {
	store    eventstore.Store
	proj     *projection.Projection
	products Products
	log      *slog.Logger
	now      func() time.Time

	// grace is how long a placed order may wait before Reconcile picks it up,
	// so that Reconcile does not race the request that is still handling it.
	grace time.Duration

	commitMu sync.Mutex // keeps "append" and "update the view" in the same order
}

// Option configures a Service.
type Option func(*Service)

// WithClock replaces the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithGrace sets how long a placed order is left alone before reconciliation.
func WithGrace(d time.Duration) Option { return func(s *Service) { s.grace = d } }

// New builds a Service and rebuilds the read model from the event log.
func New(store eventstore.Store, products Products, log *slog.Logger, opts ...Option) (*Service, error) {
	s := &Service{
		store: store, proj: projection.New(), products: products, log: log,
		now: time.Now, grace: 10 * time.Second,
	}
	for _, o := range opts {
		o(s)
	}
	events, err := store.LoadAll(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load event log: %w", err)
	}
	if err := s.proj.Rebuild(events); err != nil {
		return nil, fmt.Errorf("rebuild read model: %w", err)
	}
	log.Info("read model rebuilt from event log", "events", len(events), "orders", s.proj.Len())
	return s, nil
}

// Projection exposes the read model to the query side.
func (s *Service) Projection() *projection.Projection { return s.proj }

// PlaceResult is the outcome of Place.
type PlaceResult struct {
	Order    projection.View
	Replayed bool // an earlier request with the same Idempotency-Key was returned
}

// Place accepts an order and tries to reserve its stock.
//
// The returned order is confirmed, rejected, or (if the Product service could
// not be reached) still placed, in which case Reconcile will finish it. An
// Idempotency-Key makes retries by the customer safe: the same key and the
// same request returns the same order instead of creating a second one.
func (s *Service) Place(ctx context.Context, customerID, idempotencyKey string, items []domain.Item) (PlaceResult, error) {
	id := newOrderID()
	if idempotencyKey != "" {
		id = orderIDFromKey(customerID, idempotencyKey)
	}
	first, err := domain.Place(id, customerID, items, s.now())
	if err != nil {
		return PlaceResult{}, err
	}

	for attempt := 0; attempt < 3; attempt++ {
		existing, err := s.store.Load(ctx, id)
		if err != nil {
			return PlaceResult{}, err
		}
		if len(existing) > 0 {
			return s.replay(ctx, id, existing, first)
		}
		err = s.commit(ctx, first)
		if errors.Is(err, eventstore.ErrVersionConflict) {
			continue // a concurrent request with the same key won; replay it
		}
		if err != nil {
			return PlaceResult{}, err
		}
		s.log.Info("order placed", "order_id", id, "request_id", reqid.From(ctx))
		view, err := s.settle(ctx, id)
		if err != nil {
			// Not fatal: the order exists and Reconcile will finish it.
			s.log.Warn("order left placed; reconciler will retry", "order_id", id, "err", err, "request_id", reqid.From(ctx))
		}
		return PlaceResult{Order: view}, nil
	}
	return PlaceResult{}, ErrContention
}

// replay handles a repeated Idempotency-Key.
func (s *Service) replay(ctx context.Context, id string, existing []domain.Event, wanted domain.Event) (PlaceResult, error) {
	var was, want domain.OrderPlaced
	if err := json.Unmarshal(existing[0].Data, &was); err != nil {
		return PlaceResult{}, fmt.Errorf("stored order %s is unreadable: %w", id, err)
	}
	if err := json.Unmarshal(wanted.Data, &want); err != nil {
		return PlaceResult{}, err
	}
	if was.CustomerID != want.CustomerID || !sameItems(was.Items, want.Items) {
		return PlaceResult{}, ErrIdempotencyConflict
	}
	// Give an order that is still placed another chance, since the customer
	// is retrying precisely because they did not get an answer.
	if v, ok := s.proj.Get(id); ok && v.Status == domain.StatusPlaced {
		if view, err := s.settle(ctx, id); err == nil {
			return PlaceResult{Order: view, Replayed: true}, nil
		}
	}
	view, ok := s.proj.Get(id)
	if !ok {
		return PlaceResult{}, ErrNotFound
	}
	return PlaceResult{Order: view, Replayed: true}, nil
}

// Cancel cancels a confirmed order and returns its stock to the shelf.
func (s *Service) Cancel(ctx context.Context, id, reason string) (projection.View, error) {
	if _, err := s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
		e, err := o.Cancel(reason, s.now())
		return &e, err
	}); err != nil {
		return projection.View{}, err
	}
	s.log.Info("order cancelled", "order_id", id, "request_id", reqid.From(ctx))
	if err := s.releaseStock(ctx, id); err != nil {
		s.log.Warn("stock release deferred to reconciler", "order_id", id, "err", err, "request_id", reqid.From(ctx))
	}
	return s.view(id)
}

// Ship marks a confirmed order as shipped.
func (s *Service) Ship(ctx context.Context, id string) (projection.View, error) {
	if _, err := s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
		e, err := o.Ship(s.now())
		return &e, err
	}); err != nil {
		return projection.View{}, err
	}
	return s.view(id)
}

// Reconcile finishes work that was interrupted: it settles placed orders the
// Product service never answered, and releases stock for cancelled orders
// whose release never landed. It is safe to run at any time, as often as you
// like, from any number of replicas of this service's request handlers.
func (s *Service) Reconcile(ctx context.Context) (settled, released int) {
	placed, release := s.proj.Pending(s.now(), s.grace)
	for _, id := range placed {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.settle(ctx, id); err != nil {
			s.log.Warn("reconcile: order still unsettled", "order_id", id, "err", err)
			continue
		}
		settled++
	}
	for _, id := range release {
		if ctx.Err() != nil {
			return
		}
		if err := s.releaseStock(ctx, id); err != nil {
			s.log.Warn("reconcile: stock release still pending", "order_id", id, "err", err)
			continue
		}
		released++
	}
	if settled+released > 0 {
		s.log.Info("reconcile finished work", "settled", settled, "released", released)
	}
	return
}

// RunReconciler calls Reconcile every interval until ctx is cancelled.
func (s *Service) RunReconciler(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reconcile(ctx)
		}
	}
}

// settle asks the Product service about a placed order and records its answer.
// Calling it on an order that is no longer placed is a harmless no-op.
func (s *Service) settle(ctx context.Context, id string) (projection.View, error) {
	o, err := s.load(ctx, id)
	if err != nil {
		return projection.View{}, err
	}
	if o.Status != domain.StatusPlaced {
		return s.view(id)
	}
	items := make([]domain.Item, len(o.Items))
	for i, it := range o.Items {
		items[i] = domain.Item{ProductID: it.ProductID, Quantity: it.Quantity}
	}

	res, err := s.products.Reserve(ctx, id, items)
	var rejected *productclient.RejectedError
	switch {
	case err == nil:
		_, err = s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
			if o.Status != domain.StatusPlaced {
				return nil, nil // someone else got here first
			}
			e, err := o.Confirm(res.Items, res.Currency, s.now())
			return &e, err
		})
		if err != nil {
			return projection.View{}, err
		}
		s.log.Info("order confirmed", "order_id", id, "request_id", reqid.From(ctx))
	case errors.As(err, &rejected):
		_, err = s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
			if o.Status != domain.StatusPlaced {
				return nil, nil
			}
			e, err := o.Reject(rejected.Reason, rejected.Message, s.now())
			return &e, err
		})
		if err != nil {
			return projection.View{}, err
		}
		s.log.Info("order rejected", "order_id", id, "reason", rejected.Reason, "request_id", reqid.From(ctx))
	default:
		v, _ := s.view(id)
		return v, err
	}
	return s.view(id)
}

// releaseStock tells the Product service to put a cancelled order's stock back
// and records that it did.
func (s *Service) releaseStock(ctx context.Context, id string) error {
	if err := s.products.Release(ctx, id); err != nil {
		return err
	}
	_, err := s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
		if !o.ReleasePending {
			return nil, nil
		}
		e, err := o.MarkStockReleased(s.now())
		return &e, err
	})
	return err
}

// mutate is the command loop: load, decide, append. If another writer appended
// first, it reloads and decides again, so every decision is made against the
// latest state. A nil event from decide means there is nothing to do.
func (s *Service) mutate(ctx context.Context, id string, decide func(domain.Order) (*domain.Event, error)) (domain.Order, error) {
	for attempt := 0; attempt < 5; attempt++ {
		o, err := s.load(ctx, id)
		if err != nil {
			return domain.Order{}, err
		}
		e, err := decide(o)
		if err != nil {
			return o, err
		}
		if e == nil {
			return o, nil
		}
		if err := s.commit(ctx, *e); err != nil {
			if errors.Is(err, eventstore.ErrVersionConflict) {
				continue
			}
			return o, err
		}
		if err := o.Apply(*e); err != nil {
			return o, err
		}
		return o, nil
	}
	return domain.Order{}, ErrContention
}

func (s *Service) load(ctx context.Context, id string) (domain.Order, error) {
	events, err := s.store.Load(ctx, id)
	if err != nil {
		return domain.Order{}, err
	}
	if len(events) == 0 {
		return domain.Order{}, ErrNotFound
	}
	return domain.Replay(events)
}

// commit appends an event and updates the read model, in that order, under one
// lock so the view sees events in the order the store did.
func (s *Service) commit(ctx context.Context, e domain.Event) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := s.store.Append(ctx, e); err != nil {
		return err
	}
	if err := s.proj.Apply(e); err != nil {
		// The event is stored, so the command succeeded. The view is stale
		// until restart, which rebuilds it; make that loud.
		s.log.Error("read model could not apply a stored event; it will be rebuilt on restart",
			"order_id", e.StreamID, "version", e.Version, "err", err)
	}
	return nil
}

func (s *Service) view(id string) (projection.View, error) {
	v, ok := s.proj.Get(id)
	if !ok {
		return projection.View{}, ErrNotFound
	}
	return v, nil
}

func newOrderID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "ord_" + hex.EncodeToString(b[:])
}

// orderIDFromKey makes the order id a function of who asked and the key they
// used, so a retry maps back onto the same order stream.
func orderIDFromKey(customerID, key string) string {
	sum := sha256.Sum256([]byte(customerID + "\x00" + key))
	return "ord_" + hex.EncodeToString(sum[:8])
}

func sameItems(a, b []domain.Item) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(in []domain.Item) []domain.Item {
		out := append([]domain.Item(nil), in...)
		sort.Slice(out, func(i, j int) bool { return out[i].ProductID < out[j].ProductID })
		return out
	}
	x, y := norm(a), norm(b)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
