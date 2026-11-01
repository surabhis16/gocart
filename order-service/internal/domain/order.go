// Package domain is the Order bounded context's write model.
//
// An order is never stored as a row that gets updated. It is stored as the
// list of things that happened to it (events), and its current state is
// whatever you get by replaying them. The decision methods below ("can this
// order be cancelled?") are pure functions of that state, so the whole state
// machine can be tested without a database, a network or a clock.
//
// Life cycle:
//
//	placed ──► confirmed ──► shipped
//	   │           │
//	   │           └──► cancelled   (stock release is owed until StockReleased)
//	   └──► rejected
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Limits applied when an order is placed.
const (
	MaxLines     = 50
	MaxQuantity  = 100
	MaxIDLen     = 64
	MaxReasonLen = 200
)

var (
	// ErrValidation is wrapped by every input validation failure.
	ErrValidation = errors.New("validation failed")
	// ErrInvalidTransition is returned when an order's state forbids a command.
	ErrInvalidTransition = errors.New("invalid state transition")
)

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Unwrap() error { return ErrValidation }

func invalid(format string, args ...any) error {
	return &validationError{msg: fmt.Sprintf(format, args...)}
}

type transitionError struct{ msg string }

func (e *transitionError) Error() string { return e.msg }
func (e *transitionError) Unwrap() error { return ErrInvalidTransition }

func badTransition(format string, args ...any) error {
	return &transitionError{msg: fmt.Sprintf(format, args...)}
}

// Status is where an order is in its life cycle.
type Status string

const (
	StatusPlaced    Status = "placed"    // accepted; waiting for the Product service to reserve stock
	StatusConfirmed Status = "confirmed" // stock is reserved and priced
	StatusRejected  Status = "rejected"  // the Product service refused (no stock, unknown product)
	StatusCancelled Status = "cancelled"
	StatusShipped   Status = "shipped"
)

// Event types. Names are past tense: they record facts, not intentions.
const (
	TypeOrderPlaced    = "OrderPlaced"
	TypeOrderConfirmed = "OrderConfirmed"
	TypeOrderRejected  = "OrderRejected"
	TypeOrderCancelled = "OrderCancelled"
	TypeStockReleased  = "StockReleased"
	TypeOrderShipped   = "OrderShipped"
)

// Event is the envelope every fact is stored in. Version is the event's
// 1-based position in its stream (one stream per order); the store uses it to
// detect two writers racing on the same order.
type Event struct {
	StreamID string          `json:"stream_id"`
	Version  int             `json:"version"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	At       time.Time       `json:"at"`
}

// Item is a requested product and quantity.
type Item struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

// PricedItem is an item with the name and price quoted when stock was
// reserved. Name and price are absent until then.
type PricedItem struct {
	ProductID      string `json:"product_id"`
	Name           string `json:"name,omitempty"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor,omitempty"`
}

// Event payloads.
type (
	OrderPlaced struct {
		CustomerID string `json:"customer_id"`
		Items      []Item `json:"items"`
	}
	OrderConfirmed struct {
		Items      []PricedItem `json:"items"`
		TotalMinor int64        `json:"total_minor"`
		Currency   string       `json:"currency"`
	}
	OrderRejected struct {
		Reason  string `json:"reason"`
		Message string `json:"message,omitempty"`
	}
	OrderCancelled struct {
		Reason string `json:"reason,omitempty"`
	}
	StockReleased struct{}
	OrderShipped  struct{}
)

// Order is the aggregate: the state obtained by replaying an order's events.
type Order struct {
	ID         string
	Version    int // number of events applied
	CustomerID string
	Status     Status

	Items        []PricedItem // requested items until confirmed, then priced
	TotalMinor   int64
	Currency     string
	RejectReason string
	RejectDetail string

	// ReleasePending is true from OrderCancelled until StockReleased: the order
	// is cancelled but the Product service has not yet been told to put the
	// stock back. Something must still do that.
	ReleasePending bool

	PlacedAt  time.Time
	UpdatedAt time.Time
}

// Replay rebuilds an order from its events, in order.
func Replay(events []Event) (Order, error) {
	var o Order
	for _, e := range events {
		if err := o.Apply(e); err != nil {
			return Order{}, err
		}
	}
	return o, nil
}

// Apply folds one event into the state. It rejects events that arrive out of
// order or that the current state could never have produced, because that
// means the stored history is damaged and guessing would make it worse.
func (o *Order) Apply(e Event) error {
	if e.Version != o.Version+1 {
		return fmt.Errorf("stream %s: expected event version %d, got %d", e.StreamID, o.Version+1, e.Version)
	}
	if o.Version > 0 && e.StreamID != o.ID {
		return fmt.Errorf("event for stream %s applied to order %s", e.StreamID, o.ID)
	}
	need := func(want Status) error {
		if o.Status != want {
			return fmt.Errorf("stream %s v%d: %s event while order is %q", e.StreamID, e.Version, e.Type, o.Status)
		}
		return nil
	}

	switch e.Type {
	case TypeOrderPlaced:
		if o.Version != 0 {
			return fmt.Errorf("stream %s v%d: OrderPlaced is not the first event", e.StreamID, e.Version)
		}
		var d OrderPlaced
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("stream %s v%d: %w", e.StreamID, e.Version, err)
		}
		o.ID, o.CustomerID, o.Status, o.PlacedAt = e.StreamID, d.CustomerID, StatusPlaced, e.At
		o.Items = make([]PricedItem, len(d.Items))
		for i, it := range d.Items {
			o.Items[i] = PricedItem{ProductID: it.ProductID, Quantity: it.Quantity}
		}

	case TypeOrderConfirmed:
		if err := need(StatusPlaced); err != nil {
			return err
		}
		var d OrderConfirmed
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("stream %s v%d: %w", e.StreamID, e.Version, err)
		}
		o.Status, o.Items, o.TotalMinor, o.Currency = StatusConfirmed, d.Items, d.TotalMinor, d.Currency

	case TypeOrderRejected:
		if err := need(StatusPlaced); err != nil {
			return err
		}
		var d OrderRejected
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("stream %s v%d: %w", e.StreamID, e.Version, err)
		}
		o.Status, o.RejectReason, o.RejectDetail = StatusRejected, d.Reason, d.Message

	case TypeOrderCancelled:
		if err := need(StatusConfirmed); err != nil {
			return err
		}
		o.Status, o.ReleasePending = StatusCancelled, true

	case TypeStockReleased:
		if err := need(StatusCancelled); err != nil {
			return err
		}
		if !o.ReleasePending {
			return fmt.Errorf("stream %s v%d: StockReleased but no release was owed", e.StreamID, e.Version)
		}
		o.ReleasePending = false

	case TypeOrderShipped:
		if err := need(StatusConfirmed); err != nil {
			return err
		}
		o.Status = StatusShipped

	default:
		// An unknown type means the log was written by a newer version of this
		// service. Skipping it silently would produce wrong state.
		return fmt.Errorf("stream %s v%d: unknown event type %q", e.StreamID, e.Version, e.Type)
	}

	o.Version = e.Version
	o.UpdatedAt = e.At
	return nil
}

// Place validates a new order and returns its first event.
func Place(id, customerID string, items []Item, now time.Time) (Event, error) {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" || len(customerID) > MaxIDLen || hasControl(customerID) {
		return Event{}, invalid("customer_id must be 1-%d printable characters", MaxIDLen)
	}
	if len(items) == 0 || len(items) > MaxLines {
		return Event{}, invalid("items must contain 1-%d lines", MaxLines)
	}
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		switch {
		case it.ProductID == "" || len(it.ProductID) > MaxIDLen || hasControl(it.ProductID):
			return Event{}, invalid("every item needs a product_id of 1-%d printable characters", MaxIDLen)
		case it.Quantity < 1 || it.Quantity > MaxQuantity:
			return Event{}, invalid("quantity for %s must be between 1 and %d", it.ProductID, MaxQuantity)
		case seen[it.ProductID]:
			return Event{}, invalid("product %s appears more than once; combine the quantities", it.ProductID)
		}
		seen[it.ProductID] = true
	}
	return newEvent(id, 1, TypeOrderPlaced, OrderPlaced{CustomerID: customerID, Items: items}, now), nil
}

// Confirm records that stock was reserved at the quoted prices.
func (o Order) Confirm(priced []PricedItem, currency string, now time.Time) (Event, error) {
	if o.Status != StatusPlaced {
		return Event{}, badTransition("order %s is %s; only a placed order can be confirmed", o.ID, o.Status)
	}
	var total int64
	for _, it := range priced {
		total += int64(it.Quantity) * it.UnitPriceMinor
	}
	return o.next(TypeOrderConfirmed, OrderConfirmed{Items: priced, TotalMinor: total, Currency: currency}, now), nil
}

// Reject records that the Product service refused the order.
func (o Order) Reject(reason, message string, now time.Time) (Event, error) {
	if o.Status != StatusPlaced {
		return Event{}, badTransition("order %s is %s; only a placed order can be rejected", o.ID, o.Status)
	}
	return o.next(TypeOrderRejected, OrderRejected{Reason: reason, Message: message}, now), nil
}

// Cancel cancels a confirmed order. A shipped order cannot be cancelled: the
// goods have left, so that is a return, which is a different process.
func (o Order) Cancel(reason string, now time.Time) (Event, error) {
	if len(reason) > MaxReasonLen {
		return Event{}, invalid("reason must be at most %d characters", MaxReasonLen)
	}
	if o.Status != StatusConfirmed {
		return Event{}, badTransition("order %s is %s; only a confirmed order can be cancelled", o.ID, o.Status)
	}
	return o.next(TypeOrderCancelled, OrderCancelled{Reason: reason}, now), nil
}

// MarkStockReleased records that the Product service has put the stock back.
func (o Order) MarkStockReleased(now time.Time) (Event, error) {
	if !o.ReleasePending {
		return Event{}, badTransition("order %s is not waiting for a stock release", o.ID)
	}
	return o.next(TypeStockReleased, StockReleased{}, now), nil
}

// Ship marks a confirmed order as shipped.
func (o Order) Ship(now time.Time) (Event, error) {
	if o.Status != StatusConfirmed {
		return Event{}, badTransition("order %s is %s; only a confirmed order can be shipped", o.ID, o.Status)
	}
	return o.next(TypeOrderShipped, OrderShipped{}, now), nil
}

func (o Order) next(typ string, payload any, now time.Time) Event {
	return newEvent(o.ID, o.Version+1, typ, payload, now)
}

func newEvent(stream string, version int, typ string, payload any, now time.Time) Event {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err) // payloads are plain structs of strings and numbers
	}
	return Event{StreamID: stream, Version: version, Type: typ, Data: data, At: now.UTC()}
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
