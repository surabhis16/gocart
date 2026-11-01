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

// build applies a sequence of commands, failing the test if any is refused.
func build(t *testing.T, steps ...string) Order {
	t.Helper()
	ev, err := Place("ord_1", "cust_1", []Item{{"prd_a", 2}, {"prd_b", 1}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Replay([]Event{ev})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		var e Event
		switch s {
		case "confirm":
			e, err = o.Confirm(priced(), "INR", t0)
		case "reject":
			e, err = o.Reject("insufficient_stock", "prd_a: wanted 2, have 1", t0)
		case "cancel":
			e, err = o.Cancel("changed my mind", t0)
		case "release":
			e, err = o.MarkStockReleased(t0)
		case "ship":
			e, err = o.Ship(t0)
		default:
			t.Fatalf("unknown step %q", s)
		}
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
	o := build(t, "confirm", "ship")
	if o.Status != StatusShipped || o.Version != 3 {
		t.Fatalf("status=%s version=%d", o.Status, o.Version)
	}
	if o.TotalMinor != 2*1000+550 || o.Currency != "INR" {
		t.Errorf("total=%d currency=%s", o.TotalMinor, o.Currency)
	}
	if o.CustomerID != "cust_1" || !o.PlacedAt.Equal(t0) {
		t.Errorf("identity lost: %+v", o)
	}
}

func TestCancelOwesAStockReleaseUntilItIsRecorded(t *testing.T) {
	o := build(t, "confirm", "cancel")
	if o.Status != StatusCancelled || !o.ReleasePending {
		t.Fatalf("after cancel: status=%s releasePending=%v", o.Status, o.ReleasePending)
	}
	o = build(t, "confirm", "cancel", "release")
	if o.ReleasePending {
		t.Error("release still pending after StockReleased")
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
}

// TestTransitionMatrix is the whole state machine in one table: for every
// state an order can be in, which commands are allowed.
func TestTransitionMatrix(t *testing.T) {
	cmds := []string{"confirm", "reject", "cancel", "release", "ship"}
	states := []struct {
		name  string
		steps []string
		allow map[string]bool
	}{
		{"placed", nil, map[string]bool{"confirm": true, "reject": true}},
		{"confirmed", []string{"confirm"}, map[string]bool{"cancel": true, "ship": true}},
		{"rejected", []string{"reject"}, map[string]bool{}},
		{"cancelled, release owed", []string{"confirm", "cancel"}, map[string]bool{"release": true}},
		{"cancelled, released", []string{"confirm", "cancel", "release"}, map[string]bool{}},
		{"shipped", []string{"confirm", "ship"}, map[string]bool{}},
	}
	for _, st := range states {
		for _, c := range cmds {
			o := build(t, st.steps...)
			var err error
			switch c {
			case "confirm":
				_, err = o.Confirm(priced(), "INR", t0)
			case "reject":
				_, err = o.Reject("x", "", t0)
			case "cancel":
				_, err = o.Cancel("", t0)
			case "release":
				_, err = o.MarkStockReleased(t0)
			case "ship":
				_, err = o.Ship(t0)
			}
			switch {
			case st.allow[c] && err != nil:
				t.Errorf("%s: %s refused: %v", st.name, c, err)
			case !st.allow[c] && !errors.Is(err, ErrInvalidTransition):
				t.Errorf("%s: %s err = %v, want ErrInvalidTransition", st.name, c, err)
			}
		}
	}
}

func TestEventsCarryStreamAndConsecutiveVersions(t *testing.T) {
	o := build(t, "confirm")
	e, _ := o.Ship(t0)
	if e.StreamID != "ord_1" || e.Version != 3 || e.Type != TypeOrderShipped {
		t.Errorf("%+v", e)
	}
}

func TestApplyRefusesDamagedHistory(t *testing.T) {
	placed, _ := Place("ord_1", "c", []Item{{"p", 1}}, t0)
	o, _ := Replay([]Event{placed})
	confirmed, _ := o.Confirm(priced(), "INR", t0)

	cases := map[string][]Event{
		"gap in versions":     {placed, func() Event { e := confirmed; e.Version = 3; return e }()},
		"duplicate event":     {placed, placed},
		"starts mid-stream":   {func() Event { e := confirmed; e.Version = 1; return e }()},
		"unknown type":        {placed, {StreamID: "ord_1", Version: 2, Type: "OrderTeleported", Data: []byte(`{}`), At: t0}},
		"wrong stream":        {placed, func() Event { e := confirmed; e.StreamID = "ord_2"; return e }()},
		"ship before confirm": {placed, {StreamID: "ord_1", Version: 2, Type: TypeOrderShipped, Data: []byte(`{}`), At: t0}},
		"release nobody owed": {placed, confirmed, {StreamID: "ord_1", Version: 3, Type: TypeStockReleased, Data: []byte(`{}`), At: t0}},
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
	}{
		"blank customer":      {"   ", ok},
		"long customer":       {strings.Repeat("c", MaxIDLen+1), ok},
		"control in customer": {"bob\nsmith", ok},
		"no items":            {"c", nil},
		"too many lines":      {"c", make([]Item, MaxLines+1)},
		"empty product id":    {"c", []Item{{"", 1}}},
		"zero quantity":       {"c", []Item{{"prd_a", 0}}},
		"huge quantity":       {"c", []Item{{"prd_a", MaxQuantity + 1}}},
		"duplicate product":   {"c", []Item{{"prd_a", 1}, {"prd_a", 2}}},
	}
	for name, tc := range cases {
		if _, err := Place("ord_1", tc.customer, tc.items, t0); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
	if _, err := Place("ord_1", " cust_1 ", ok, t0); err != nil {
		t.Errorf("valid order refused: %v", err)
	}
}

func TestCancelReasonIsBounded(t *testing.T) {
	o := build(t, "confirm")
	if _, err := o.Cancel(strings.Repeat("x", MaxReasonLen+1), t0); !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}
