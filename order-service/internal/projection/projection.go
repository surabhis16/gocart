// Package projection is the Order service's read model (the "Q" in CQRS).
//
// Commands never read from here: they replay an order's events from the store,
// so a decision is always made on the truth. Queries never replay anything:
// they read this in-memory view, which is rebuilt from the full event log at
// startup and kept current by applying each new event as it is stored.
//
// The view is shaped for the API (flat, with a by-customer index) rather than
// for decisions, which is the point of separating the two sides.
package projection

import (
	"sort"
	"sync"
	"time"

	"gocart/order-service/internal/domain"
)

// View is an order as the API presents it.
type View struct {
	ID                  string              `json:"id"`
	CustomerID          string              `json:"customer_id"`
	Status              domain.Status       `json:"status"`
	Items               []domain.PricedItem `json:"items"`
	TotalMinor          int64               `json:"total_minor,omitempty"`
	Currency            string              `json:"currency,omitempty"`
	RejectReason        string              `json:"reject_reason,omitempty"`
	RejectDetail        string              `json:"reject_detail,omitempty"`
	StockReleasePending bool                `json:"stock_release_pending,omitempty"`
	PlacedAt            time.Time           `json:"placed_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
	Version             int                 `json:"version"`
}

// Filter narrows a listing. Zero values mean "no filter" (Limit 0 means 50).
type Filter struct {
	CustomerID string
	Status     domain.Status
	Limit      int
}

// Projection is safe for concurrent use.
type Projection struct {
	mu         sync.RWMutex
	orders     map[string]domain.Order
	byCustomer map[string][]string
}

// New returns an empty projection.
func New() *Projection {
	return &Projection{orders: map[string]domain.Order{}, byCustomer: map[string][]string{}}
}

// Apply folds one event into the view. Applying an event that has already been
// applied is a no-op, so feeding the same event twice is harmless.
func (p *Projection) Apply(e domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o := p.orders[e.StreamID]
	if e.Version <= o.Version {
		return nil
	}
	if err := o.Apply(e); err != nil {
		return err
	}
	if e.Type == domain.TypeOrderPlaced {
		p.byCustomer[o.CustomerID] = append(p.byCustomer[o.CustomerID], o.ID)
	}
	p.orders[e.StreamID] = o
	return nil
}

// Rebuild applies events in the order given. Events of one stream must be in
// version order; nothing else is assumed.
func (p *Projection) Rebuild(events []domain.Event) error {
	for _, e := range events {
		if err := p.Apply(e); err != nil {
			return err
		}
	}
	return nil
}

// Get returns one order.
func (p *Projection) Get(id string) (View, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	o, ok := p.orders[id]
	if !ok {
		return View{}, false
	}
	return viewOf(o), true
}

// List returns matching orders, newest first.
func (p *Projection) List(f Filter) []View {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	var candidates []domain.Order
	if f.CustomerID != "" {
		for _, id := range p.byCustomer[f.CustomerID] {
			candidates = append(candidates, p.orders[id])
		}
	} else {
		candidates = make([]domain.Order, 0, len(p.orders))
		for _, o := range p.orders {
			candidates = append(candidates, o)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].PlacedAt.Equal(candidates[j].PlacedAt) {
			return candidates[i].PlacedAt.After(candidates[j].PlacedAt)
		}
		return candidates[i].ID > candidates[j].ID
	})

	out := make([]View, 0, min(limit, len(candidates)))
	for _, o := range candidates {
		if f.Status != "" && o.Status != f.Status {
			continue
		}
		out = append(out, viewOf(o))
		if len(out) == limit {
			break
		}
	}
	return out
}

// Pending lists orders that something still has to finish: placed orders that
// have waited at least grace for the Product service, and cancelled orders
// whose stock has not been released yet. Oldest first.
func (p *Projection) Pending(now time.Time, grace time.Duration) (placed, release []string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var pl, rl []domain.Order
	for _, o := range p.orders {
		switch {
		case o.Status == domain.StatusPlaced && now.Sub(o.UpdatedAt) >= grace:
			pl = append(pl, o)
		case o.ReleasePending:
			rl = append(rl, o)
		}
	}
	return idsOldestFirst(pl), idsOldestFirst(rl)
}

// Len is the number of orders in the view.
func (p *Projection) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.orders)
}

func idsOldestFirst(orders []domain.Order) []string {
	sort.Slice(orders, func(i, j int) bool {
		if !orders[i].UpdatedAt.Equal(orders[j].UpdatedAt) {
			return orders[i].UpdatedAt.Before(orders[j].UpdatedAt)
		}
		return orders[i].ID < orders[j].ID
	})
	ids := make([]string, len(orders))
	for i, o := range orders {
		ids[i] = o.ID
	}
	return ids
}

func viewOf(o domain.Order) View {
	return View{
		ID: o.ID, CustomerID: o.CustomerID, Status: o.Status,
		Items:      append([]domain.PricedItem(nil), o.Items...),
		TotalMinor: o.TotalMinor, Currency: o.Currency,
		RejectReason: o.RejectReason, RejectDetail: o.RejectDetail,
		StockReleasePending: o.ReleasePending,
		PlacedAt:            o.PlacedAt, UpdatedAt: o.UpdatedAt, Version: o.Version,
	}
}
