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
//	placed ──► awaiting_payment ──► confirmed ──► shipped
//	   │              │    │             │
//	   │              │    │             └──► cancelled
//	   │              │    └──► expired
//	   │              └──► payment_failed
//	   └──► rejected
//
// Three of those end states leave something owed to another service, and the
// order says so until it has been paid back: cancelled, payment_failed and
// expired all owe a stock release; cancelled and expired also owe the payment
// service a void or refund. Those debts are the saga's compensations.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	StatusPlaced          Status = "placed"           // accepted; waiting for the Product service to reserve stock
	StatusAwaitingPayment Status = "awaiting_payment" // stock is held and priced; waiting for the Payment service
	StatusConfirmed       Status = "confirmed"        // stock held and paid for
	StatusRejected        Status = "rejected"         // the Product service refused (no stock, unknown product)
	StatusPaymentFailed   Status = "payment_failed"   // the card was declined; the stock goes back
	StatusExpired         Status = "expired"          // nobody paid in time; the stock goes back
	StatusCancelled       Status = "cancelled"
	StatusShipped         Status = "shipped"
)

// AllStatuses lists every status, for validating filters.
var AllStatuses = []Status{
	StatusPlaced, StatusAwaitingPayment, StatusConfirmed, StatusRejected,
	StatusPaymentFailed, StatusExpired, StatusCancelled, StatusShipped,
}

// Event types. Names are past tense: they record facts, not intentions.
const (
	TypeOrderPlaced        = "OrderPlaced"
	TypeStockReserved      = "StockReserved"
	TypeOrderRejected      = "OrderRejected"
	TypePaymentCaptured    = "PaymentCaptured"
	TypePaymentDeclined    = "PaymentDeclined"
	TypeReservationExpired = "ReservationExpired"
	TypeOrderCancelled     = "OrderCancelled"
	TypeStockReleased      = "StockReleased"
	TypePaymentRefunded    = "PaymentRefunded"
	TypeOrderShipped       = "OrderShipped"

	// TypeOrderConfirmed is the version 0.1 way of confirming an order, written
	// when there was no payment step: stock reserved, done. New code never
	// writes it, but logs written by 0.1 contain it, and a stored event is a
	// fact that cannot be rewritten, so replay must keep understanding it.
	TypeOrderConfirmed = "OrderConfirmed"
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
		// PaymentToken is an opaque reference to a payment method held by the
		// provider, never a card number. It is kept so a payment that could not
		// be completed can be retried without asking the customer again. It is
		// absent from events written before payments existed.
		PaymentToken string `json:"payment_token,omitempty"`
	}
	StockReserved struct {
		Items      []PricedItem `json:"items"`
		TotalMinor int64        `json:"total_minor"`
		Currency   string       `json:"currency"`
	}
	OrderConfirmed struct { // legacy, see TypeOrderConfirmed
		Items      []PricedItem `json:"items"`
		TotalMinor int64        `json:"total_minor"`
		Currency   string       `json:"currency"`
	}
	OrderRejected struct {
		Reason  string `json:"reason"`
		Message string `json:"message,omitempty"`
	}
	PaymentCaptured struct {
		Reference string `json:"reference"`
	}
	PaymentDeclined struct {
		Code string `json:"code"`
	}
	ReservationExpired struct{}
	OrderCancelled     struct {
		Reason string `json:"reason,omitempty"`
	}
	StockReleased   struct{}
	PaymentRefunded struct{}
	OrderShipped    struct{}
)

// Order is the aggregate: the state obtained by replaying an order's events.
type Order struct {
	ID         string
	Version    int // number of events applied
	CustomerID string
	Status     Status

	Items        []PricedItem // requested items until stock is reserved, then priced
	TotalMinor   int64
	Currency     string
	RejectReason string
	RejectDetail string

	PaymentToken     string // see OrderPlaced; never exposed by the API
	PaymentReference string // the provider's id, once captured
	DeclineCode      string
	// NoPaymentStep is true for an order confirmed by a version 0.1 event: it
	// was never charged, so cancelling it has nothing to refund.
	NoPaymentStep bool

	// ReleasePending is true from the moment an order ends without its stock
	// being used (cancelled, payment_failed, expired) until StockReleased: the
	// Product service has not yet been told to put the stock back, and
	// something must still do that.
	ReleasePending bool
	// RefundPending is the same for money: the Payment service has not yet
	// been told to void or refund this order's charge.
	RefundPending bool

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
		o.PaymentToken = d.PaymentToken
		o.Items = make([]PricedItem, len(d.Items))
		for i, it := range d.Items {
			o.Items[i] = PricedItem{ProductID: it.ProductID, Quantity: it.Quantity}
		}

	case TypeStockReserved:
		if err := need(StatusPlaced); err != nil {
			return err
		}
		var d StockReserved
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("stream %s v%d: %w", e.StreamID, e.Version, err)
		}
		o.Status, o.Items, o.TotalMinor, o.Currency = StatusAwaitingPayment, d.Items, d.TotalMinor, d.Currency

	case TypeOrderConfirmed: // legacy: stock reserved and the order done, no payment step
		if err := need(StatusPlaced); err != nil {
			return err
		}
		var d OrderConfirmed
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("stream %s v%d: %w", e.StreamID, e.Version, err)
		}
		o.Status, o.Items, o.TotalMinor, o.Currency = StatusConfirmed, d.Items, d.TotalMinor, d.Currency
		o.NoPaymentStep = true

	case TypePaymentCaptured:
		if err := need(StatusAwaitingPayment); err != nil {
			return err
		}
		var d PaymentCaptured
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("stream %s v%d: %w", e.StreamID, e.Version, err)
		}
		o.Status, o.PaymentReference = StatusConfirmed, d.Reference

	case TypePaymentDeclined:
		if err := need(StatusAwaitingPayment); err != nil {
			return err
		}
		var d PaymentDeclined
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("stream %s v%d: %w", e.StreamID, e.Version, err)
		}
		o.Status, o.DeclineCode, o.ReleasePending = StatusPaymentFailed, d.Code, true

	case TypeReservationExpired:
		if err := need(StatusAwaitingPayment); err != nil {
			return err
		}
		// The charge may be in flight right now, so a void is owed too.
		o.Status, o.ReleasePending, o.RefundPending = StatusExpired, true, true

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
		o.RefundPending = !o.NoPaymentStep

	case TypeStockReleased:
		if !o.ReleasePending {
			return fmt.Errorf("stream %s v%d: StockReleased but no release was owed", e.StreamID, e.Version)
		}
		o.ReleasePending = false

	case TypePaymentRefunded:
		if !o.RefundPending {
			return fmt.Errorf("stream %s v%d: PaymentRefunded but no refund was owed", e.StreamID, e.Version)
		}
		o.RefundPending = false

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

// tokenPattern is the shape of a provider payment token. The Payment service
// enforces the same rule; it is repeated here on purpose, so that a card number
// is refused before this service writes it into an append-only log.
var tokenPattern = regexp.MustCompile(`^tok_[a-z0-9_]{1,40}$`)

// Place validates a new order and returns its first event.
func Place(id, customerID string, items []Item, paymentToken string, now time.Time) (Event, error) {
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
	if !tokenPattern.MatchString(paymentToken) {
		// An immutable log is a terrible place for a card number, so anything
		// that is not clearly a provider token is refused before it is stored.
		return Event{}, invalid("payment_token must be a provider token such as tok_visa, never a card number")
	}
	return newEvent(id, 1, TypeOrderPlaced, OrderPlaced{CustomerID: customerID, Items: items, PaymentToken: paymentToken}, now), nil
}

// ReserveStock records that the Product service holds stock for the order, at
// the prices it quoted, and works out the total to charge.
func (o Order) ReserveStock(priced []PricedItem, currency string, now time.Time) (Event, error) {
	if o.Status != StatusPlaced {
		return Event{}, badTransition("order %s is %s; stock can only be reserved for a placed order", o.ID, o.Status)
	}
	var total int64
	for _, it := range priced {
		total += int64(it.Quantity) * it.UnitPriceMinor
	}
	return o.next(TypeStockReserved, StockReserved{Items: priced, TotalMinor: total, Currency: currency}, now), nil
}

// CapturePayment records that the Payment service took the money.
func (o Order) CapturePayment(reference string, now time.Time) (Event, error) {
	if o.Status != StatusAwaitingPayment {
		return Event{}, badTransition("order %s is %s; only an order awaiting payment can be paid", o.ID, o.Status)
	}
	return o.next(TypePaymentCaptured, PaymentCaptured{Reference: reference}, now), nil
}

// DeclinePayment records a definite refusal by the card network. The stock is
// now owed back.
func (o Order) DeclinePayment(code string, now time.Time) (Event, error) {
	if o.Status != StatusAwaitingPayment {
		return Event{}, badTransition("order %s is %s; only an order awaiting payment can be declined", o.ID, o.Status)
	}
	return o.next(TypePaymentDeclined, PaymentDeclined{Code: code}, now), nil
}

// Expire gives up on an order nobody paid for. The stock is owed back, and so
// is a void of the charge, in case one is on its way.
func (o Order) Expire(now time.Time) (Event, error) {
	if o.Status != StatusAwaitingPayment {
		return Event{}, badTransition("order %s is %s; only an order awaiting payment can expire", o.ID, o.Status)
	}
	return o.next(TypeReservationExpired, ReservationExpired{}, now), nil
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

// MarkPaymentRefunded records that the Payment service has voided the charge.
func (o Order) MarkPaymentRefunded(now time.Time) (Event, error) {
	if !o.RefundPending {
		return Event{}, badTransition("order %s is not waiting for a refund", o.ID)
	}
	return o.next(TypePaymentRefunded, PaymentRefunded{}, now), nil
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
