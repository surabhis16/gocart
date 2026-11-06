// Package service is the Order service's command side: it decides, stores
// events, and coordinates with the Product and Payment services.
//
// The services cannot share a transaction, so an order is a saga: a sequence of
// local steps, each stored as an event, with a compensating step for every one
// that cannot simply be forgotten.
//
//  1. store OrderPlaced                (the order exists, durably)
//  2. Product: reserve stock           (idempotent per order id)  -> StockReserved
//  3. Payment: charge the customer     (idempotent per order id)  -> PaymentCaptured
//
// and the ways it can end badly, each with what it owes back:
//
//	Product says no                      -> OrderRejected          (nothing held, nothing owed)
//	card declined                        -> PaymentDeclined        (stock owed back)
//	nobody pays within the time limit    -> ReservationExpired     (stock owed back, charge voided)
//	customer cancels a paid order        -> OrderCancelled         (stock owed back, charge refunded)
//
// A crash, timeout or outage between any two steps leaves the order in a real,
// queryable state (placed, awaiting_payment, or cancelled with a debt) rather
// than a corrupt one. Reconcile finds such orders and finishes them. Every
// remote step is idempotent per order id, so finishing is always safe: nothing
// is ever double-reserved or double-charged.
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
	"gocart/order-service/internal/paymentclient"
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

// Payments is what the Order service needs from the Payment service.
type Payments interface {
	Charge(ctx context.Context, orderID string, amountMinor int64, currency, token string) (paymentclient.Charge, error)
	Cancel(ctx context.Context, orderID string) error
}

// Service coordinates orders.
type Service struct {
	store    eventstore.Store
	proj     *projection.Projection
	products Products
	payments Payments
	log      *slog.Logger
	now      func() time.Time

	// grace is how long an order may wait before Reconcile picks it up, so that
	// Reconcile does not race the request that is still handling it.
	grace time.Duration
	// paymentTTL is how long stock is held for an order nobody has paid for.
	// Without it, a customer who abandons checkout holds stock forever.
	paymentTTL time.Duration

	commitMu sync.Mutex // keeps "append" and "update the view" in the same order

	// Syncing with other replicas: see sync.go.
	syncMu      sync.Mutex
	lastSync    time.Time
	lastFull    time.Time
	resyncEvery time.Duration
}

// Option configures a Service.
type Option func(*Service)

// WithClock replaces the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithGrace sets how long a placed order is left alone before reconciliation.
func WithGrace(d time.Duration) Option { return func(s *Service) { s.grace = d } }

// WithPaymentTTL sets how long stock is held while an order awaits payment.
func WithPaymentTTL(d time.Duration) Option { return func(s *Service) { s.paymentTTL = d } }

// New builds a Service and rebuilds the read model from the event log.
func New(store eventstore.Store, products Products, payments Payments, log *slog.Logger, opts ...Option) (*Service, error) {
	s := &Service{
		store: store, proj: projection.New(), products: products, payments: payments, log: log,
		now: time.Now, grace: 10 * time.Second, paymentTTL: 15 * time.Minute,
		resyncEvery: 5 * time.Minute,
	}
	for _, o := range opts {
		o(s)
	}
	// Taken before the load, so anything appended while loading is picked up by
	// the first Sync rather than falling between the two.
	s.lastSync = time.Now()
	s.lastFull = s.lastSync
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

// Order returns one order from the read model. Unlike List, which is served
// from this replica's view and can trail other replicas by a sync interval,
// Order looks in the store when the view has never heard of the id, because the
// usual reason is a customer polling the Location of an order that another
// replica has just created.
func (s *Service) Order(id string) (projection.View, error) { return s.view(id) }

// Projection exposes the read model to the query side.
func (s *Service) Projection() *projection.Projection { return s.proj }

// PlaceResult is the outcome of Place.
type PlaceResult struct {
	Order    projection.View
	Replayed bool // an earlier request with the same Idempotency-Key was returned
}

// Place accepts an order, reserves its stock and takes payment.
//
// The returned order is confirmed, rejected (no stock), payment_failed (card
// declined), or, if a service could not be reached, still placed or
// awaiting_payment, in which case Reconcile will finish it. An Idempotency-Key
// makes retries by the customer safe: the same key and the same request
// returns the same order instead of creating a second one.
func (s *Service) Place(ctx context.Context, customerID, idempotencyKey, paymentToken string, items []domain.Item) (PlaceResult, error) {
	id := newOrderID()
	if idempotencyKey != "" {
		id = orderIDFromKey(customerID, idempotencyKey)
	}
	first, err := domain.Place(id, customerID, items, paymentToken, s.now())
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
	if was.CustomerID != want.CustomerID || was.PaymentToken != want.PaymentToken || !sameItems(was.Items, want.Items) {
		return PlaceResult{}, ErrIdempotencyConflict
	}
	// Give an order that is still unfinished another chance, since the
	// customer is retrying precisely because they did not get an answer.
	if v, err := s.view(id); err == nil && (v.Status == domain.StatusPlaced || v.Status == domain.StatusAwaitingPayment) {
		if view, err := s.settle(ctx, id); err == nil {
			return PlaceResult{Order: view, Replayed: true}, nil
		}
	}
	view, err := s.view(id)
	if err != nil {
		return PlaceResult{}, err
	}
	return PlaceResult{Order: view, Replayed: true}, nil
}

// Cancel cancels a confirmed order, returns its stock to the shelf and refunds
// the payment.
func (s *Service) Cancel(ctx context.Context, id, reason string) (projection.View, error) {
	if _, err := s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
		e, err := o.Cancel(reason, s.now())
		return &e, err
	}); err != nil {
		return projection.View{}, err
	}
	s.log.Info("order cancelled", "order_id", id, "request_id", reqid.From(ctx))
	if err := s.compensate(ctx, id); err != nil {
		s.log.Warn("compensation deferred to reconciler", "order_id", id, "err", err, "request_id", reqid.From(ctx))
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

// Deliver marks a shipped order as delivered.
func (s *Service) Deliver(ctx context.Context, id string) (projection.View, error) {
	if _, err := s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
		e, err := o.Deliver(s.now())
		return &e, err
	}); err != nil {
		return projection.View{}, err
	}
	return s.view(id)
}

// Reconcile finishes work that was interrupted. It settles placed orders the
// Product service never answered, finishes payment for orders still awaiting
// it (or expires them once the time limit has passed), and pays back whatever
// stock or money a finished order still owes. It is safe to run at any time,
// as often as you like.
func (s *Service) Reconcile(ctx context.Context) (settled, compensated int) {
	w := s.proj.Pending(s.now(), s.grace)
	for _, id := range w.Placed {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.settle(ctx, id); err != nil {
			s.log.Warn("reconcile: order still unsettled", "order_id", id, "err", err)
			continue
		}
		settled++
	}
	for _, id := range w.Unpaid {
		if ctx.Err() != nil {
			return
		}
		if s.expireIfStale(ctx, id) {
			settled++
			continue
		}
		if _, err := s.settle(ctx, id); err != nil {
			s.log.Warn("reconcile: payment still unsettled", "order_id", id, "err", err)
			continue
		}
		settled++
	}
	// Re-read: the loops above may have created new debts (a decline or an
	// expiry owes stock), and those should not wait a whole interval.
	w = s.proj.Pending(s.now(), s.grace)
	seen := map[string]bool{}
	for _, id := range append(append([]string(nil), w.Release...), w.Refund...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		if ctx.Err() != nil {
			return
		}
		if err := s.compensate(ctx, id); err != nil {
			s.log.Warn("reconcile: compensation still pending", "order_id", id, "err", err)
			continue
		}
		compensated++
	}
	if settled+compensated > 0 {
		s.log.Info("reconcile finished work", "settled", settled, "compensated", compensated)
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

// settle drives an order as far forward as it can go: reserve stock if that
// has not happened, then take payment if that has not happened. It stops, with
// an error, at the first service that does not answer; the order stays in the
// state it reached, and a later call carries on from there. Calling it on an
// order that has finished is a harmless no-op.
func (s *Service) settle(ctx context.Context, id string) (projection.View, error) {
	o, err := s.load(ctx, id)
	if err != nil {
		return projection.View{}, err
	}
	if o.Status == domain.StatusPlaced {
		if o, err = s.reserve(ctx, o); err != nil {
			v, _ := s.view(id)
			return v, err
		}
	}
	if o.Status == domain.StatusAwaitingPayment {
		if err = s.pay(ctx, o); err != nil {
			v, _ := s.view(id)
			return v, err
		}
	}
	return s.view(id)
}

// reserve asks the Product service to hold stock and records the answer.
func (s *Service) reserve(ctx context.Context, o domain.Order) (domain.Order, error) {
	items := make([]domain.Item, len(o.Items))
	for i, it := range o.Items {
		items[i] = domain.Item{ProductID: it.ProductID, Quantity: it.Quantity}
	}
	res, err := s.products.Reserve(ctx, o.ID, items)
	var rejected *productclient.RejectedError
	switch {
	case err == nil:
		o, err = s.mutate(ctx, o.ID, func(o domain.Order) (*domain.Event, error) {
			if o.Status != domain.StatusPlaced {
				return nil, nil // someone else got here first
			}
			e, err := o.ReserveStock(res.Items, res.Currency, s.now())
			return &e, err
		})
		if err != nil {
			return o, err
		}
		s.log.Info("stock reserved", "order_id", o.ID, "total_minor", o.TotalMinor, "request_id", reqid.From(ctx))
		return o, nil
	case errors.As(err, &rejected):
		o, err = s.mutate(ctx, o.ID, func(o domain.Order) (*domain.Event, error) {
			if o.Status != domain.StatusPlaced {
				return nil, nil
			}
			e, err := o.Reject(rejected.Reason, rejected.Message, s.now())
			return &e, err
		})
		if err != nil {
			return o, err
		}
		s.log.Info("order rejected", "order_id", o.ID, "reason", rejected.Reason, "request_id", reqid.From(ctx))
		return o, nil
	default:
		return o, err
	}
}

// pay asks the Payment service to charge for an order awaiting payment.
//
// Three answers, three different consequences. Approved confirms the order. A
// decline is final: the order fails and its stock goes back. "Could not tell"
// changes nothing and returns an error, because the charge may well have gone
// through: the next attempt asks again, and the Payment service answers with
// whatever it recorded rather than charging twice.
func (s *Service) pay(ctx context.Context, o domain.Order) error {
	charge, err := s.payments.Charge(ctx, o.ID, o.TotalMinor, o.Currency, o.PaymentToken)
	var declined *paymentclient.DeclinedError
	switch {
	case err == nil:
		o, err = s.mutate(ctx, o.ID, func(o domain.Order) (*domain.Event, error) {
			if o.Status != domain.StatusAwaitingPayment {
				return nil, nil
			}
			e, err := o.CapturePayment(charge.Reference, s.now())
			return &e, err
		})
		if err != nil {
			return err
		}
		if o.Status != domain.StatusConfirmed {
			// The order expired while the charge was in flight. Money was taken
			// for an order that no longer exists; the void owed since expiry
			// will give it back.
			s.log.Warn("payment arrived after the order ended; the refund owed will return it",
				"order_id", o.ID, "status", o.Status, "reference", charge.Reference)
			return nil
		}
		s.log.Info("payment captured; order confirmed", "order_id", o.ID, "reference", charge.Reference, "request_id", reqid.From(ctx))
		return nil
	case errors.As(err, &declined):
		if _, err := s.mutate(ctx, o.ID, func(o domain.Order) (*domain.Event, error) {
			if o.Status != domain.StatusAwaitingPayment {
				return nil, nil
			}
			e, err := o.DeclinePayment(declined.Code, s.now())
			return &e, err
		}); err != nil {
			return err
		}
		s.log.Info("payment declined; order failed", "order_id", o.ID, "code", declined.Code, "request_id", reqid.From(ctx))
		if err := s.compensate(ctx, o.ID); err != nil {
			s.log.Warn("stock release deferred to reconciler", "order_id", o.ID, "err", err)
		}
		return nil
	case errors.Is(err, paymentclient.ErrVoided):
		// Only possible once the order has expired and been voided.
		return nil
	default:
		return err
	}
}

// expireIfStale gives up on an order that has waited too long for payment. It
// reports whether it did.
func (s *Service) expireIfStale(ctx context.Context, id string) bool {
	o, err := s.load(ctx, id)
	if err != nil || o.Status != domain.StatusAwaitingPayment || s.now().Sub(o.UpdatedAt) < s.paymentTTL {
		return false
	}
	// The event is stored first. From that moment the order cannot be
	// confirmed, and the void that follows closes the window in which a charge
	// that was already in flight could still land: it either finds the charge
	// and refunds it, or leaves a tombstone that makes a late charge fail.
	o, err = s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
		if o.Status != domain.StatusAwaitingPayment {
			return nil, nil
		}
		e, err := o.Expire(s.now())
		return &e, err
	})
	if err != nil || o.Status != domain.StatusExpired {
		return false
	}
	s.log.Info("order expired unpaid; stock goes back", "order_id", id, "held_for", s.now().Sub(o.PlacedAt).Round(time.Second).String())
	if err := s.compensate(ctx, id); err != nil {
		s.log.Warn("compensation deferred to reconciler", "order_id", id, "err", err)
	}
	return true
}

// compensate pays back whatever a finished order still owes: stock to the
// Product service and a void or refund to the Payment service. The two are
// independent, so one failing does not hold up the other.
func (s *Service) compensate(ctx context.Context, id string) error {
	o, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	var errs []error
	if o.ReleasePending {
		errs = append(errs, s.releaseStock(ctx, id))
	}
	if o.RefundPending {
		errs = append(errs, s.refundPayment(ctx, id))
	}
	return errors.Join(errs...)
}

// releaseStock tells the Product service to put an order's stock back and
// records that it did.
func (s *Service) releaseStock(ctx context.Context, id string) error {
	if err := s.products.Release(ctx, id); err != nil {
		return fmt.Errorf("release stock: %w", err)
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

// refundPayment tells the Payment service to void an order's charge and
// records that it did.
func (s *Service) refundPayment(ctx context.Context, id string) error {
	if err := s.payments.Cancel(ctx, id); err != nil {
		return fmt.Errorf("refund payment: %w", err)
	}
	_, err := s.mutate(ctx, id, func(o domain.Order) (*domain.Event, error) {
		if !o.RefundPending {
			return nil, nil
		}
		e, err := o.MarkPaymentRefunded(s.now())
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
		// The event is stored, so the command succeeded. The usual reason the
		// view cannot apply it is that another replica wrote the order's
		// earlier events and this one has not heard yet. Load the order's
		// history and catch up on the spot.
		if cerr := s.catchUpLocked(ctx, e.StreamID); cerr != nil {
			s.log.Error("read model could not apply a stored event; the next sync will retry",
				"order_id", e.StreamID, "version", e.Version, "err", err, "catch_up_err", cerr)
		}
	}
	return nil
}

// view returns an order from the read model. If the view has never heard of it,
// another replica may have created it a moment ago, so look in the store before
// saying it does not exist.
func (s *Service) view(id string) (projection.View, error) {
	if v, ok := s.proj.Get(id); ok {
		return v, nil
	}
	s.commitMu.Lock()
	err := s.catchUpLocked(context.Background(), id)
	s.commitMu.Unlock()
	if err == nil {
		if v, ok := s.proj.Get(id); ok {
			return v, nil
		}
	}
	return projection.View{}, ErrNotFound
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
