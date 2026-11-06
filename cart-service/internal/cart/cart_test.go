package cart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func line(id string, q int) Line { return Line{ProductID: id, Name: "name of " + id, Quantity: q} }

func TestAnUnknownCustomerHasAnEmptyCart(t *testing.T) {
	c := open(t).Get("cust_1")
	if len(c.Items) != 0 || c.Revision != 0 || c.CheckedOut() {
		t.Fatalf("%+v", c)
	}
	if c.Items == nil {
		t.Error("items must marshal as [], not null")
	}
}

func TestSetItemAddsAndReplacesQuantity(t *testing.T) {
	s := open(t)
	s.SetItem("c", line("prd_b", 2))
	s.SetItem("c", line("prd_a", 1))
	c, err := s.SetItem("c", line("prd_b", 5))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Items) != 2 || c.Items[0].ProductID != "prd_a" || c.Items[1].Quantity != 5 {
		t.Fatalf("lines must be sorted and the quantity replaced, not added: %+v", c.Items)
	}
}

func TestSettingTheSameQuantityTwiceChangesNothing(t *testing.T) {
	s := open(t)
	first, _ := s.SetItem("c", line("prd_a", 2))
	again, err := s.SetItem("c", line("prd_a", 2))
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != first.Revision || !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("an idempotent PUT must not move the revision: %d -> %d", first.Revision, again.Revision)
	}
}

func TestRevisionMovesOnEveryRealChange(t *testing.T) {
	s := open(t)
	revs := []int64{}
	step := func(c Cart, err error) {
		if err != nil {
			t.Fatal(err)
		}
		revs = append(revs, c.Revision)
	}
	step(s.SetItem("c", line("prd_a", 1)))
	step(s.SetItem("c", line("prd_a", 2)))
	step(s.SetItem("c", line("prd_b", 1)))
	step(s.RemoveItem("c", "prd_a"))
	step(s.Clear("c"))
	for i := 1; i < len(revs); i++ {
		if revs[i] <= revs[i-1] {
			t.Fatalf("revisions must strictly increase: %v", revs)
		}
	}
}

func TestRemovingAndClearingAreIdempotent(t *testing.T) {
	s := open(t)
	if _, err := s.RemoveItem("c", "prd_nope"); err != nil {
		t.Fatal(err)
	}
	c, _ := s.SetItem("c", line("prd_a", 1))
	a, _ := s.Clear("c")
	b, _ := s.Clear("c")
	if a.Revision <= c.Revision || b.Revision != a.Revision {
		t.Errorf("clear twice: %d, %d, %d", c.Revision, a.Revision, b.Revision)
	}
	if len(b.Items) != 0 {
		t.Error("cart not empty")
	}
}

func TestValidation(t *testing.T) {
	s := open(t)
	cases := []struct {
		name     string
		customer string
		line     Line
	}{
		{"zero quantity", "c", line("p", 0)},
		{"too many", "c", line("p", MaxQuantity+1)},
		{"no product", "c", line("", 1)},
		{"long product", "c", line(strings.Repeat("p", 65), 1)},
		{"bad customer", "has space", line("p", 1)},
		{"path-like customer", "../x", line("p", 1)},
		{"empty customer", "", line("p", 1)},
	}
	for _, c := range cases {
		if _, err := s.SetItem(c.customer, c.line); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	for i := 0; i < MaxLines; i++ {
		if _, err := s.SetItem("full", line("prd_"+string(rune('a'+i%26))+string(rune('a'+i/26)), 1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetItem("full", line("one_too_many", 1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("line %d: %v", MaxLines+1, err)
	}
	if _, err := s.SetItem("full", line("prd_aa", 7)); err != nil {
		t.Errorf("changing an existing line of a full cart must work: %v", err)
	}
}

func TestMarkOrderedFreezesTheCartAndTheNextChangeStartsOver(t *testing.T) {
	s := open(t)
	c, _ := s.SetItem("c", line("prd_a", 1))
	if err := s.MarkOrdered("c", c.Revision, "ord_1"); err != nil {
		t.Fatal(err)
	}
	got := s.Get("c")
	if !got.CheckedOut() || got.OrderID != "ord_1" || len(got.Items) != 1 || got.Revision != c.Revision {
		t.Fatalf("a checked-out cart keeps its lines and revision so a retry replays the same order: %+v", got)
	}
	next, err := s.SetItem("c", line("prd_b", 3))
	if err != nil {
		t.Fatal(err)
	}
	if next.CheckedOut() || len(next.Items) != 1 || next.Items[0].ProductID != "prd_b" || next.Revision <= c.Revision {
		t.Errorf("the next change must start a fresh cart, not edit the ordered one: %+v", next)
	}
}

func TestMarkOrderedIgnoresACartThatChangedMeanwhile(t *testing.T) {
	s := open(t)
	c, _ := s.SetItem("c", line("prd_a", 1))
	s.SetItem("c", line("prd_b", 1)) // the customer edits while checkout is in flight
	if err := s.MarkOrdered("c", c.Revision, "ord_1"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get("c"); got.CheckedOut() || len(got.Items) != 2 {
		t.Errorf("the edit must survive, and must not be labelled as part of order ord_1: %+v", got)
	}
}

func TestRenewKeepsLinesAndChangesTheKey(t *testing.T) {
	s := open(t)
	c, _ := s.SetItem("c", line("prd_a", 1))
	if err := s.Renew("c", c.Revision); err != nil {
		t.Fatal(err)
	}
	got := s.Get("c")
	if len(got.Items) != 1 || got.Revision != c.Revision+1 {
		t.Fatalf("%+v", got)
	}
	if CheckoutKey("c", c.Revision) == CheckoutKey("c", got.Revision) {
		t.Error("a renewed cart must produce a different Idempotency-Key")
	}
	if err := s.Renew("c", c.Revision); err != nil || s.Get("c").Revision != got.Revision {
		t.Error("renewing with a stale revision must do nothing")
	}
}

func TestCheckoutKeyIsStableAndSafe(t *testing.T) {
	if CheckoutKey("cust_1", 4) != CheckoutKey("cust_1", 4) {
		t.Fatal("not deterministic")
	}
	if CheckoutKey("cust_1", 4) == CheckoutKey("cust_2", 4) {
		t.Fatal("two customers must never share a key")
	}
	if k := CheckoutKey(strings.Repeat("c", 64), 1<<62); len(k) > 128 {
		t.Errorf("longest key is %d characters; the Order service allows 128", len(k))
	}
}

func TestCartsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	c, _ := s.SetItem("c", line("prd_a", 2))
	s.MarkOrdered("c", c.Revision, "ord_9")
	s.SetItem("d", line("prd_b", 1))

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Get("c"); got.OrderID != "ord_9" || got.Items[0].Quantity != 2 || got.Revision != c.Revision {
		t.Errorf("%+v", got)
	}
	if open, ordered := again.Counts(); open != 1 || ordered != 1 {
		t.Errorf("counts: %d open, %d ordered", open, ordered)
	}
}

func TestADamagedFileIsRefusedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "carts.json"), []byte(`{"carts": {`), 0o644)
	if _, err := Open(dir); err == nil {
		t.Fatal("opening a damaged file must fail, or the next write would destroy it")
	}
}

func TestAFailedSaveLeavesMemoryAsItWas(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	s.SetItem("c", line("prd_a", 1))
	os.RemoveAll(dir) // the disk goes away
	if _, err := s.SetItem("c", line("prd_a", 9)); err == nil {
		t.Fatal("expected the save to fail")
	}
	if got := s.Get("c"); got.Items[0].Quantity != 1 {
		t.Errorf("memory must not claim a change the disk refused: %+v", got)
	}
	if _, err := s.SetItem("new", line("prd_a", 1)); err == nil {
		t.Fatal("expected the save to fail")
	}
	if len(s.Get("new").Items) != 0 {
		t.Error("a cart whose first save failed must not exist")
	}
}

func TestConcurrentEditsKeepEveryLine(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetItem("c", line("prd_"+string(rune('a'+i%26))+string(rune('a'+i/26)), 1))
		}()
	}
	wg.Wait()
	c := s.Get("c")
	if len(c.Items) != 40 || c.Revision != 40 {
		t.Errorf("%d lines at revision %d", len(c.Items), c.Revision)
	}
}
