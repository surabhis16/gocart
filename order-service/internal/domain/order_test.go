package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

func priced() []PricedItem {
	return []PricedItem{
		{ProductID: "prd_a", Name: "A", Quantity: 2, UnitPriceMinor: 1000},
		{ProductID: "prd_b", Name: "B", Quantity: 1, UnitPriceMinor: 550},
	}
}

func placedEvent(t *testing.T) Event {
	t.Helper()
	ev, err := Place("ord_1", "cust_1", []Item{{"prd_a", 2}, {"prd_b", 1}}, "tok_visa", t0)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// command runs one named command against o.
func command(o Order, name string) (Event, error) {
	switch name {
	case "reserve":
		return o.ReserveStock(priced(), "INR", t0)
	case "reject":
		return o.Reject("insufficient_stock", "prd_a: wanted 2, have 1", t0)
	case "capture":
		return o.CapturePayment("pay_123", t0)
	case "decline":
		return o.DeclinePayment("card_declined", t0)
	case "expire":
		return o.Expire(t0)
	case "cancel":
		return o.Cancel("changed my mind", t0)
	case "release":
		return o.MarkStockReleased(t0)
	case "refund":
		return o.MarkPaymentRefunded(t0)
	case "ship":
		return o.Ship(t0)
	case "deliver":
		return o.Deliver(t0)
	}
	panic("unknown command " + name)
}

// build applies a sequence of commands, failing the test if any is refused.
func build(t *testing.T, steps ...string) Order {
	t.Helper()
	o, err := Replay([]Event{placedEvent(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		e, err := command(o, s)
		if err != nil {
			t.Fatalf("step %q refused: %v", s, err)
		}
		if err := o.Apply(e); err != nil {
			t.Fatalf("step %q: Apply: %v", s, err)
		}
	}
	return o
}

func TestHappyPathState(t *testing.T) {
	o := build(t, "reserve", "capture", "ship")
	if o.Status != StatusShipped || o.Version != 4 {
		t.Fatalf("status=%s version=%d", o.Status, o.Version)
	}
	if o.TotalMinor != 2*1000+550 || o.Currency != "INR" {
		t.Errorf("total=%d currency=%s", o.TotalMinor, o.Currency)
	}
	if o.PaymentReference != "pay_123" || o.PaymentToken != "tok_visa" {
		t.Errorf("payment details lost: %+v", o)
	}
	if o.CustomerID != "cust_1" || !o.PlacedAt.Equal(t0) {
		t.Errorf("identity lost: %+v", o)
	}
}

func TestReservingStockMovesToAwaitingPaymentNotConfirmed(t *testing.T) {
	o := build(t, "reserve")
	if o.Status != StatusAwaitingPayment {
		t.Fatalf("status = %s", o.Status)
	}
	if o.TotalMinor != 2550 {
		t.Errorf("total = %d, want the amount to charge worked out at reservation", o.TotalMinor)
	}
}

func TestCancelOwesStockAndMoney(t *testing.T) {
	o := build(t, "reserve", "capture", "cancel")
	if o.Status != StatusCancelled || !o.ReleasePending || !o.RefundPending {
		t.Fatalf("after cancel: %+v", o)
	}
	o = build(t, "reserve", "capture", "cancel", "release")
	if o.ReleasePending || !o.RefundPending {
		t.Errorf("after release: release=%v refund=%v", o.ReleasePending, o.RefundPending)
	}
	o = build(t, "reserve", "capture", "cancel", "release", "refund")
	if o.ReleasePending || o.RefundPending {
		t.Error("debts remain after both were paid")
	}
	// The two debts are independent: either can be settled first.
	o = build(t, "reserve", "capture", "cancel", "refund", "release")
	if o.ReleasePending || o.RefundPending {
		t.Error("order of settlement mattered")
	}
}

func TestDeclinedOwesStockButNoRefund(t *testing.T) {
	o := build(t, "reserve", "decline")
	if o.Status != StatusPaymentFailed || o.DeclineCode != "card_declined" {
		t.Fatalf("%+v", o)
	}
	if !o.ReleasePending {
		t.Error("a declined order must owe its stock back")
	}
	if o.RefundPending {
		t.Error("nothing was charged, so nothing is owed back")
	}
}

func TestExpiryOwesStockAndAVoid(t *testing.T) {
	o := build(t, "reserve", "expire")
	if o.Status != StatusExpired || !o.ReleasePending || !o.RefundPending {
		t.Fatalf("%+v", o)
	}
}

func TestRejectedKeepsItsReason(t *testing.T) {
	o := build(t, "reject")
	if o.Status != StatusRejected || o.RejectReason != "insufficient_stock" || !strings.Contains(o.RejectDetail, "prd_a") {
		t.Errorf("%+v", o)
	}
	if o.TotalMinor != 0 {
		t.Errorf("a rejected order has no total, got %d", o.TotalMinor)
	}
	if o.ReleasePending || o.RefundPending {
		t.Error("a rejected order never held stock or money")
	}
}

// TestTransitionMatrix is the whole state machine in one table: for every
// state an order can be in, which commands are allowed.
func TestTransitionMatrix(t *testing.T) {
	cmds := []string{"reserve", "reject", "capture", "decline", "expire", "cancel", "release", "refund", "ship", "deliver"}
	states := []struct {
		name  string
		steps []string
		allow []string
	}{
		{"placed", nil, []string{"reserve", "reject"}},
		{"awaiting payment", []string{"reserve"}, []string{"capture", "decline", "expire"}},
		{"confirmed", []string{"reserve", "capture"}, []string{"cancel", "ship"}},
		{"rejected", []string{"reject"}, nil},
		{"payment failed, release owed", []string{"reserve", "decline"}, []string{"release"}},
		{"payment failed, released", []string{"reserve", "decline", "release"}, nil},
		{"expired, both owed", []string{"reserve", "expire"}, []string{"release", "refund"}},
		{"expired, release done", []string{"reserve", "expire", "release"}, []string{"refund"}},
		{"expired, refund done", []string{"reserve", "expire", "refund"}, []string{"release"}},
		{"expired, all settled", []string{"reserve", "expire", "release", "refund"}, nil},
		{"cancelled, both owed", []string{"reserve", "capture", "cancel"}, []string{"release", "refund"}},
		{"cancelled, all settled", []string{"reserve", "capture", "cancel", "release", "refund"}, nil},
		{"shipped", []string{"reserve", "capture", "ship"}, []string{"deliver"}},
		{"delivered", []string{"reserve", "capture", "ship", "deliver"}, nil},
	}
	for _, st := range states {
		allowed := map[string]bool{}
		for _, a := range st.allow {
			allowed[a] = true
		}
		for _, c := range cmds {
			_, err := command(build(t, st.steps...), c)
			switch {
			case allowed[c] && err != nil:
				t.Errorf("%s: %s refused: %v", st.name, c, err)
			case !allowed[c] && !errors.Is(err, ErrInvalidTransition):
				t.Errorf("%s: %s err = %v, want ErrInvalidTransition", st.name, c, err)
			}
		}
	}
}

func TestDeliveredIsTheEndOfTheLine(t *testing.T) {
	o := build(t, "reserve", "capture", "ship", "deliver")
	if o.Status != StatusDelivered || o.Version != 5 {
		t.Fatalf("status=%s version=%d", o.Status, o.Version)
	}
	if o.ReleasePending || o.RefundPending {
		t.Error("delivery must not owe anything")
	}
	for _, c := range []string{"cancel", "ship", "deliver", "refund", "release"} {
		if _, err := command(o, c); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s after delivery: err = %v", c, err)
		}
	}
}

func TestEventsCarryStreamAndConsecutiveVersions(t *testing.T) {
	o := build(t, "reserve", "capture")
	e, _ := o.Ship(t0)
	if e.StreamID != "ord_1" || e.Version != 4 || e.Type != TypeOrderShipped {
		t.Errorf("%+v", e)
	}
}

// TestVersion01LogsStillReplay is the "managing change" case that matters most
// in an event-sourced system. Version 0.1 wrote OrderConfirmed straight after
// OrderPlaced, with no payment, and no token on OrderPlaced. Those events are
// stored facts; they cannot be edited, so the new code must keep reading them.
func TestVersion01LogsStillReplay(t *testing.T) {
	old := []Event{
		{StreamID: "ord_old", Version: 1, Type: TypeOrderPlaced, At: t0,
			Data: []byte(`{"customer_id":"cust_1","items":[{"product_id":"prd_a","quantity":2}]}`)},
		{StreamID: "ord_old", Version: 2, Type: TypeOrderConfirmed, At: t0,
			Data: []byte(`{"items":[{"product_id":"prd_a","name":"A","quantity":2,"unit_price_minor":1000}],"total_minor":2000,"currency":"INR"}`)},
	}
	o, err := Replay(old)
	if err != nil {
		t.Fatalf("a version 0.1 log no longer replays: %v", err)
	}
	if o.Status != StatusConfirmed || o.TotalMinor != 2000 || !o.NoPaymentStep {
		t.Fatalf("%+v", o)
	}

	// It was never charged, so cancelling it owes stock but no refund.
	e, err := o.Cancel("", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Apply(e); err != nil {
		t.Fatal(err)
	}
	if !o.ReleasePending || o.RefundPending {
		t.Errorf("legacy order cancel: release=%v refund=%v, want true/false", o.ReleasePending, o.RefundPending)
	}
}

func TestApplyRefusesDamagedHistory(t *testing.T) {
	placed := placedEvent(t)
	o, _ := Replay([]Event{placed})
	reserved, _ := o.ReserveStock(priced(), "INR", t0)
	at := func(e Event, v int) Event { e.Version = v; return e }
	raw := func(v int, typ string) Event {
		return Event{StreamID: "ord_1", Version: v, Type: typ, Data: []byte(`{}`), At: t0}
	}

	cases := map[string][]Event{
		"gap in versions":       {placed, at(reserved, 3)},
		"duplicate event":       {placed, placed},
		"starts mid-stream":     {at(reserved, 1)},
		"unknown type":          {placed, raw(2, "OrderTeleported")},
		"wrong stream":          {placed, func() Event { e := reserved; e.StreamID = "ord_2"; return e }()},
		"ship before payment":   {placed, reserved, raw(3, TypeOrderShipped)},
		"capture before stock":  {placed, raw(2, TypePaymentCaptured)},
		"release nobody owed":   {placed, reserved, raw(3, TypeStockReleased)},
		"refund nobody owed":    {placed, reserved, raw(3, TypePaymentRefunded)},
		"expire twice":          {placed, reserved, raw(3, TypeReservationExpired), raw(4, TypeReservationExpired)},
		"decline after capture": {placed, reserved, {StreamID: "ord_1", Version: 3, Type: TypePaymentCaptured, Data: []byte(`{"reference":"p"}`), At: t0}, raw(4, TypePaymentDeclined)},
	}
	for name, evs := range cases {
		if _, err := Replay(evs); err == nil {
			t.Errorf("%s: Replay accepted damaged history", name)
		}
	}
}

func TestPlaceValidation(t *testing.T) {
	ok := []Item{{"prd_a", 1}}
	cases := map[string]struct {
		customer string
		items    []Item
		token    string
	}{
		"blank customer":      {"   ", ok, "tok_visa"},
		"long customer":       {strings.Repeat("c", MaxIDLen+1), ok, "tok_visa"},
		"control in customer": {"bob\nsmith", ok, "tok_visa"},
		"no items":            {"c", nil, "tok_visa"},
		"too many lines":      {"c", make([]Item, MaxLines+1), "tok_visa"},
		"empty product id":    {"c", []Item{{"", 1}}, "tok_visa"},
		"zero quantity":       {"c", []Item{{"prd_a", 0}}, "tok_visa"},
		"huge quantity":       {"c", []Item{{"prd_a", MaxQuantity + 1}}, "tok_visa"},
		"duplicate product":   {"c", []Item{{"prd_a", 1}, {"prd_a", 2}}, "tok_visa"},
		"missing token":       {"c", ok, ""},
		"card number":         {"c", ok, "4242424242424242"},
		"token with spaces":   {"c", ok, "tok_a b"},
		"uppercase token":     {"c", ok, "tok_VISA"},
	}
	for name, tc := range cases {
		if _, err := Place("ord_1", tc.customer, tc.items, tc.token, t0); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
	if _, err := Place("ord_1", " cust_1 ", ok, "tok_visa", t0); err != nil {
		t.Errorf("valid order refused: %v", err)
	}
}

func TestCardNumberIsNeverWrittenToTheEvent(t *testing.T) {
	e, err := Place("ord_1", "c", []Item{{"prd_a", 1}}, "4242424242424242", t0)
	if err == nil || strings.Contains(string(e.Data), "4242") {
		t.Fatalf("err=%v data=%s", err, e.Data)
	}
}

func TestCancelReasonIsBounded(t *testing.T) {
	o := build(t, "reserve", "capture")
	if _, err := o.Cancel(strings.Repeat("x", MaxReasonLen+1), t0); !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}
