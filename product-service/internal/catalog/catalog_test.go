package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func seeded(t *testing.T) *Store {
	t.Helper()
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	return s
}

func stock(t *testing.T, s *Store, id string) int {
	t.Helper()
	p, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return p.Stock
}

func TestReserveDecrementsStockAndCapturesPrice(t *testing.T) {
	s := seeded(t)
	res, replayed, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 2}, {"prd_notebook", 5}})
	if err != nil || replayed {
		t.Fatalf("Reserve: replayed=%v err=%v", replayed, err)
	}
	if got := stock(t, s, "prd_keyboard"); got != 23 {
		t.Errorf("keyboard stock = %d, want 23", got)
	}
	if got := stock(t, s, "prd_notebook"); got != 195 {
		t.Errorf("notebook stock = %d, want 195", got)
	}
	if res.Items[0].UnitPriceMinor != 499900 || res.Items[0].Name == "" || res.Currency != Currency {
		t.Errorf("reservation did not capture price/name/currency: %+v", res)
	}
}

func TestReserveIsAllOrNothing(t *testing.T) {
	s := seeded(t)
	_, _, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 1}, {"prd_lamp", 4}}) // lamp has 3
	var ins *InsufficientStockError
	if !errors.As(err, &ins) {
		t.Fatalf("err = %v, want InsufficientStockError", err)
	}
	if len(ins.Shortages) != 1 || ins.Shortages[0] != (Shortage{"prd_lamp", 4, 3}) {
		t.Errorf("shortages = %+v", ins.Shortages)
	}
	if got := stock(t, s, "prd_keyboard"); got != 25 {
		t.Errorf("keyboard stock = %d after failed reservation, want untouched 25", got)
	}
}

func TestReserveUnknownProduct(t *testing.T) {
	s := seeded(t)
	_, _, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 1}, {"prd_nope", 1}})
	var unk *UnknownProductsError
	if !errors.As(err, &unk) || len(unk.IDs) != 1 || unk.IDs[0] != "prd_nope" {
		t.Fatalf("err = %v, want UnknownProductsError for prd_nope", err)
	}
	if got := stock(t, s, "prd_keyboard"); got != 25 {
		t.Errorf("stock changed on failed reservation: %d", got)
	}
}

func TestReserveIsIdempotentPerOrder(t *testing.T) {
	s := seeded(t)
	first, _, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 2}, {"prd_hub", 1}})
	if err != nil {
		t.Fatal(err)
	}
	// Same items in a different line order is still the same request.
	again, replayed, err := s.Reserve("ord_1", []Line{{"prd_hub", 1}, {"prd_keyboard", 2}})
	if err != nil || !replayed {
		t.Fatalf("replay: replayed=%v err=%v", replayed, err)
	}
	if again.ReservedAt != first.ReservedAt {
		t.Error("replay returned a different reservation")
	}
	if got := stock(t, s, "prd_keyboard"); got != 23 {
		t.Errorf("stock = %d after replay, want 23 (no double deduction)", got)
	}
	if _, _, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 3}}); !errors.Is(err, ErrReservationMismatch) {
		t.Errorf("changed items: err = %v, want ErrReservationMismatch", err)
	}
}

func TestReleaseRestoresStockAndIsRepeatable(t *testing.T) {
	s := seeded(t)
	if _, _, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 4}}); err != nil {
		t.Fatal(err)
	}
	released, err := s.Release("ord_1")
	if err != nil || !released {
		t.Fatalf("Release: released=%v err=%v", released, err)
	}
	if got := stock(t, s, "prd_keyboard"); got != 25 {
		t.Errorf("stock = %d after release, want 25", got)
	}
	released, err = s.Release("ord_1")
	if err != nil || released {
		t.Errorf("second Release: released=%v err=%v, want false,nil", released, err)
	}
	if got := stock(t, s, "prd_keyboard"); got != 25 {
		t.Errorf("stock = %d after second release, want 25", got)
	}
}

func TestValidation(t *testing.T) {
	s := seeded(t)
	bad := []struct {
		name  string
		lines []Line
		order string
	}{
		{"empty order id", []Line{{"prd_keyboard", 1}}, ""},
		{"bad order id", []Line{{"prd_keyboard", 1}}, "has space"},
		{"no lines", nil, "ord_1"},
		{"zero quantity", []Line{{"prd_keyboard", 0}}, "ord_1"},
		{"huge quantity", []Line{{"prd_keyboard", MaxQuantity + 1}}, "ord_1"},
		{"duplicate product", []Line{{"prd_keyboard", 1}, {"prd_keyboard", 1}}, "ord_1"},
		{"missing product id", []Line{{"", 1}}, "ord_1"},
	}
	for _, tc := range bad {
		if _, _, err := s.Reserve(tc.order, tc.lines); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", tc.name, err)
		}
	}
	for _, in := range []NewProduct{
		{Name: "", PriceMinor: 100},
		{Name: "x", PriceMinor: 0},
		{Name: "x", PriceMinor: MaxPriceMinor + 1},
		{Name: "x", PriceMinor: 100, Stock: -1},
	} {
		if _, err := s.Create(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Create(%+v): err = %v, want ErrInvalid", in, err)
		}
	}
	if _, err := s.SetStock("prd_keyboard", -1); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetStock(-1): err = %v", err)
	}
	if _, err := s.SetStock("prd_nope", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetStock(unknown): err = %v", err)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 2}}); err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(NewProduct{Name: "Cable", PriceMinor: 19900, Stock: 9})
	if err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := again.SeedIfEmpty(); n != 0 {
		t.Errorf("SeedIfEmpty re-seeded a non-empty store (%d)", n)
	}
	if got := stock(t, again, "prd_keyboard"); got != 23 {
		t.Errorf("stock after restart = %d, want 23", got)
	}
	if _, err := again.Get(created.ID); err != nil {
		t.Errorf("created product lost on restart: %v", err)
	}
	// The reservation must have survived too, or replays would double-deduct.
	if _, replayed, err := again.Reserve("ord_1", []Line{{"prd_keyboard", 2}}); err != nil || !replayed {
		t.Errorf("reservation lost on restart: replayed=%v err=%v", replayed, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestCorruptSnapshotIsRefusedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(`{"products": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a corrupt snapshot; starting empty would silently lose the catalogue")
	}
}

func TestFailedPersistLeavesMemoryUntouched(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedIfEmpty(); err != nil {
		t.Fatal(err)
	}
	// Make the data directory unusable so the next write must fail.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reserve("ord_1", []Line{{"prd_keyboard", 2}}); err == nil {
		t.Fatal("Reserve succeeded although the snapshot could not be written")
	}
	if got := stock(t, s, "prd_keyboard"); got != 25 {
		t.Errorf("stock = %d after failed persist, want 25: memory ran ahead of disk", got)
	}
}

func TestNeverOversellsUnderConcurrency(t *testing.T) {
	s := seeded(t)
	const buyers = 60 // prd_lamp has 3 in stock
	var wg sync.WaitGroup
	var won atomic.Int32
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "ord_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			if _, _, err := s.Reserve(id, []Line{{"prd_lamp", 1}}); err == nil {
				won.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if won.Load() != 3 {
		t.Errorf("%d reservations succeeded for 3 units of stock", won.Load())
	}
	if got := stock(t, s, "prd_lamp"); got != 0 {
		t.Errorf("final stock = %d, want 0", got)
	}
}
