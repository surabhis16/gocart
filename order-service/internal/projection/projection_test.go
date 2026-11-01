package projection

import (
	"fmt"
	"testing"
	"time"

	"gocart/order-service/internal/domain"
)

var base = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

// history builds the events for one order by running the real domain commands.
func history(t *testing.T, id, customer string, at time.Time, steps ...string) []domain.Event {
	t.Helper()
	first, err := domain.Place(id, customer, []domain.Item{{ProductID: "prd_a", Quantity: 1}}, at)
	if err != nil {
		t.Fatal(err)
	}
	events := []domain.Event{first}
	o, _ := domain.Replay(events)
	for _, s := range steps {
		var e domain.Event
		switch s {
		case "confirm":
			e, err = o.Confirm([]domain.PricedItem{{ProductID: "prd_a", Name: "A", Quantity: 1, UnitPriceMinor: 500}}, "INR", at)
		case "reject":
			e, err = o.Reject("insufficient_stock", "", at)
		case "cancel":
			e, err = o.Cancel("", at)
		case "release":
			e, err = o.MarkStockReleased(at)
		case "ship":
			e, err = o.Ship(at)
		}
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		_ = o.Apply(e)
		events = append(events, e)
	}
	return events
}

func feed(t *testing.T, p *Projection, evs ...[]domain.Event) {
	t.Helper()
	for _, es := range evs {
		for _, e := range es {
			if err := p.Apply(e); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestGetReflectsLatestState(t *testing.T) {
	p := New()
	feed(t, p, history(t, "ord_1", "c1", base, "confirm", "ship"))
	v, ok := p.Get("ord_1")
	if !ok || v.Status != domain.StatusShipped || v.TotalMinor != 500 || v.Version != 3 || v.CustomerID != "c1" {
		t.Errorf("view = %+v ok=%v", v, ok)
	}
	if _, ok := p.Get("ord_nope"); ok {
		t.Error("found an order that does not exist")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	p := New()
	evs := history(t, "ord_1", "c1", base, "confirm")
	feed(t, p, evs, evs, evs) // the same events delivered three times
	if p.Len() != 1 {
		t.Errorf("Len = %d", p.Len())
	}
	if got := p.List(Filter{CustomerID: "c1"}); len(got) != 1 {
		t.Errorf("by-customer index duplicated the order: %d entries", len(got))
	}
	if v, _ := p.Get("ord_1"); v.Version != 2 {
		t.Errorf("version = %d", v.Version)
	}
}

func TestApplyRefusesAGap(t *testing.T) {
	p := New()
	evs := history(t, "ord_1", "c1", base, "confirm", "ship")
	if err := p.Apply(evs[0]); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(evs[2]); err == nil {
		t.Error("Apply accepted version 3 straight after version 1")
	}
}

func TestListIsNewestFirstAndFilters(t *testing.T) {
	p := New()
	feed(t, p,
		history(t, "ord_1", "c1", base, "confirm"),
		history(t, "ord_2", "c2", base.Add(time.Minute), "reject"),
		history(t, "ord_3", "c1", base.Add(2*time.Minute)),
		history(t, "ord_4", "c1", base.Add(3*time.Minute), "confirm", "ship"),
	)
	ids := func(vs []View) []string {
		var out []string
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	eq(ids(p.List(Filter{})), "ord_4", "ord_3", "ord_2", "ord_1")
	eq(ids(p.List(Filter{CustomerID: "c1"})), "ord_4", "ord_3", "ord_1")
	eq(ids(p.List(Filter{CustomerID: "c1", Status: domain.StatusConfirmed})), "ord_1")
	eq(ids(p.List(Filter{Status: domain.StatusRejected})), "ord_2")
	eq(ids(p.List(Filter{Limit: 2})), "ord_4", "ord_3")
	eq(ids(p.List(Filter{CustomerID: "nobody"})))
}

func TestPendingFindsUnfinishedWorkOldestFirst(t *testing.T) {
	p := New()
	feed(t, p,
		history(t, "ord_old", "c", base),                                                     // still placed
		history(t, "ord_new", "c", base.Add(9*time.Minute)),                                  // still placed, newer
		history(t, "ord_done", "c", base, "confirm"),                                         // nothing owed
		history(t, "ord_owed", "c", base.Add(time.Minute), "confirm", "cancel"),              // release owed
		history(t, "ord_paid", "c", base.Add(2*time.Minute), "confirm", "cancel", "release"), // settled
	)

	// Ten minutes in, both placed orders are well past the grace period.
	placed, release := p.Pending(base.Add(10*time.Minute), 5*time.Second)
	if got := fmt.Sprint(placed); got != "[ord_old ord_new]" {
		t.Errorf("placed = %s, want [ord_old ord_new] (oldest first)", got)
	}
	if got := fmt.Sprint(release); got != "[ord_owed]" {
		t.Errorf("release = %s, want [ord_owed]", got)
	}

	// One second after ord_new was placed it is still inside its grace period,
	// so it is left to the request that created it; ord_old is not.
	placed, _ = p.Pending(base.Add(9*time.Minute+time.Second), 5*time.Second)
	if got := fmt.Sprint(placed); got != "[ord_old]" {
		t.Errorf("placed = %s, want [ord_old]", got)
	}
}

func TestViewIsACopy(t *testing.T) {
	p := New()
	feed(t, p, history(t, "ord_1", "c1", base, "confirm"))
	v, _ := p.Get("ord_1")
	v.Items[0].Quantity = 999
	again, _ := p.Get("ord_1")
	if again.Items[0].Quantity != 1 {
		t.Error("mutating a returned view changed the projection")
	}
}
