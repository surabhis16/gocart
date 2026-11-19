package wishlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWishlistBasicLifecycle(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Empty wishlist retrieval
	wl := s.Get("cust_ada")
	if wl.CustomerID != "cust_ada" || len(wl.Items) != 0 {
		t.Fatalf("expected empty wishlist, got %+v", wl)
	}
	if s.Has("cust_ada", "prd_lamp") {
		t.Errorf("expected Has to report false for empty wishlist")
	}

	// 2. Add first item
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }

	wl, err = s.SetItem("cust_ada", Item{ProductID: "prd_lamp", Quantity: 1})
	if err != nil {
		t.Fatalf("set item failed: %v", err)
	}
	if len(wl.Items) != 1 || wl.Items[0].ProductID != "prd_lamp" || wl.Items[0].Quantity != 1 {
		t.Fatalf("unexpected items in wishlist: %+v", wl.Items)
	}
	if !wl.Items[0].AddedAt.Equal(t0) {
		t.Errorf("expected AddedAt %v, got %v", t0, wl.Items[0].AddedAt)
	}
	if !s.Has("cust_ada", "prd_lamp") {
		t.Errorf("expected Has to report true")
	}

	item, ok := s.Item("cust_ada", "prd_lamp")
	if !ok || item.ProductID != "prd_lamp" || item.Quantity != 1 {
		t.Errorf("expected item to be found with product id and quantity 1, got %v, %v", ok, item)
	}

	// 3. Repeated addition with identical quantity changes nothing (idempotent)
	wlSame, err := s.SetItem("cust_ada", Item{ProductID: "prd_lamp", Quantity: 1})
	if err != nil {
		t.Fatalf("idempotent set failed: %v", err)
	}
	if len(wlSame.Items) != 1 || wlSame.Items[0].Quantity != 1 {
		t.Fatalf("idempotent set corrupted items: %+v", wlSame.Items)
	}

	// 4. Update quantity (absolute SET)
	t1 := time.Date(2026, 11, 10, 13, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t1 }

	wl, err = s.SetItem("cust_ada", Item{ProductID: "prd_lamp", Quantity: 5})
	if err != nil {
		t.Fatalf("update quantity failed: %v", err)
	}
	if len(wl.Items) != 1 || wl.Items[0].Quantity != 5 {
		t.Fatalf("expected quantity 5, got %+v", wl.Items)
	}
	// AddedAt should be preserved from initial creation
	if !wl.Items[0].AddedAt.Equal(t0) {
		t.Errorf("expected AddedAt %v preserved, got %v", t0, wl.Items[0].AddedAt)
	}

	// 5. Add second item
	wl, err = s.SetItem("cust_ada", Item{ProductID: "prd_keyboard", Quantity: 2})
	if err != nil {
		t.Fatalf("add second item failed: %v", err)
	}
	if len(wl.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(wl.Items))
	}
	// Items must be sorted by ProductID
	if wl.Items[0].ProductID != "prd_keyboard" || wl.Items[1].ProductID != "prd_lamp" {
		t.Errorf("items not sorted by product id: %+v", wl.Items)
	}

	// 6. Remove single item
	wl, err = s.RemoveItem("cust_ada", "prd_lamp")
	if err != nil {
		t.Fatalf("remove item failed: %v", err)
	}
	if len(wl.Items) != 1 || wl.Items[0].ProductID != "prd_keyboard" {
		t.Fatalf("expected only keyboard remaining, got %+v", wl.Items)
	}

	// Idempotent remove of absent item
	wl, err = s.RemoveItem("cust_ada", "prd_absent")
	if err != nil {
		t.Fatalf("remove absent item failed: %v", err)
	}
	if len(wl.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(wl.Items))
	}

	// 7. Clear wishlist
	wl, err = s.Clear("cust_ada")
	if err != nil {
		t.Fatalf("clear wishlist failed: %v", err)
	}
	if len(wl.Items) != 0 {
		t.Fatalf("expected 0 items after clear, got %d", len(wl.Items))
	}

	// Idempotent clear of empty wishlist
	wl, err = s.Clear("cust_ada")
	if err != nil {
		t.Fatalf("second clear failed: %v", err)
	}
	if len(wl.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(wl.Items))
	}
}

func TestCustomerIsolation(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetItem("cust_user1", Item{ProductID: "prd_a", Quantity: 2}); err != nil {
		t.Fatalf("set item user1: %v", err)
	}
	if _, err := s.SetItem("cust_user2", Item{ProductID: "prd_b", Quantity: 3}); err != nil {
		t.Fatalf("set item user2: %v", err)
	}

	wl1 := s.Get("cust_user1")
	wl2 := s.Get("cust_user2")

	if len(wl1.Items) != 1 || wl1.Items[0].ProductID != "prd_a" {
		t.Errorf("user1 wishlist corrupted: %+v", wl1.Items)
	}
	if len(wl2.Items) != 1 || wl2.Items[0].ProductID != "prd_b" {
		t.Errorf("user2 wishlist corrupted: %+v", wl2.Items)
	}

	// User1 clearing does not affect User2
	if _, err := s.Clear("cust_user1"); err != nil {
		t.Fatalf("clear user1: %v", err)
	}
	if len(s.Get("cust_user1").Items) != 0 {
		t.Errorf("user1 wishlist not cleared")
	}
	if len(s.Get("cust_user2").Items) != 1 {
		t.Errorf("user2 wishlist was affected by user1 clear")
	}
}

func TestValidationErrors(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}

	// Invalid customer IDs
	for _, badCust := range []string{"", "has spaces", "too_long_" + string(make([]byte, 60)), "bad/slashes"} {
		if _, err := s.SetItem(badCust, Item{ProductID: "prd_1", Quantity: 1}); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected ErrInvalid for bad customer id %q, got %v", badCust, err)
		}
		if _, err := s.RemoveItem(badCust, "prd_1"); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected ErrInvalid for bad customer id %q on remove, got %v", badCust, err)
		}
		if _, err := s.Clear(badCust); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected ErrInvalid for bad customer id %q on clear, got %v", badCust, err)
		}
	}

	// Invalid product IDs in SetItem and RemoveItem
	for _, badProd := range []string{"", "has spaces", string(make([]byte, 65)), "bad/slash"} {
		if _, err := s.SetItem("cust_test", Item{ProductID: badProd, Quantity: 1}); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected ErrInvalid for bad product id %q in SetItem, got %v", badProd, err)
		}
		if _, err := s.RemoveItem("cust_test", badProd); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected ErrInvalid for bad product id %q in RemoveItem, got %v", badProd, err)
		}
	}

	// Invalid quantities
	for _, badQty := range []int{-5, 0, 101, 1000} {
		if _, err := s.SetItem("cust_test", Item{ProductID: "prd_ok", Quantity: badQty}); !errors.Is(err, ErrInvalid) {
			t.Errorf("expected ErrInvalid for bad quantity %d, got %v", badQty, err)
		}
	}
}

func TestItemLimitsAndDuplicatePrevention(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}

	cust := "cust_collector"
	// Fill up to MaxLines (100)
	for i := 0; i < MaxLines; i++ {
		id := fmt.Sprintf("prd_%04d", i)
		if _, err := s.SetItem(cust, Item{ProductID: id, Quantity: 1}); err != nil {
			t.Fatalf("failed to add item %d: %v", i, err)
		}
	}

	wl := s.Get(cust)
	if len(wl.Items) != MaxLines {
		t.Fatalf("expected %d items, got %d", MaxLines, len(wl.Items))
	}

	// Exceeding MaxLines must fail
	_, err = s.SetItem(cust, Item{ProductID: "prd_overflow", Quantity: 1})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("expected ErrInvalid on 101st item, got %v", err)
	}

	// Updating an EXISTING item among the 100 must succeed (duplicate prevention)
	wl, err = s.SetItem(cust, Item{ProductID: "prd_0000", Quantity: 10})
	if err != nil {
		t.Fatalf("updating existing item failed: %v", err)
	}
	if len(wl.Items) != MaxLines {
		t.Errorf("item count changed after updating existing item: %d", len(wl.Items))
	}
}

func TestUpdateItemPreservesAddedAt(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }

	_, err = s.SetItem("cust_ada", Item{ProductID: "prd_lamp", Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}

	// Advance time by 2 hours
	t1 := t0.Add(2 * time.Hour)
	s.now = func() time.Time { return t1 }

	wl, err := s.SetItem("cust_ada", Item{ProductID: "prd_lamp", Quantity: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(wl.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(wl.Items))
	}
	if !wl.Items[0].AddedAt.Equal(t0) {
		t.Errorf("expected AddedAt %v to remain untouched, got %v", t0, wl.Items[0].AddedAt)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	s1.now = func() time.Time { return t0 }

	_, err = s1.SetItem("cust_ada", Item{ProductID: "prd_lamp", Quantity: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s1.SetItem("cust_charles", Item{ProductID: "prd_book", Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}

	// Verify file was written
	if _, err := os.Stat(filepath.Join(dir, "wishlists.json")); err != nil {
		t.Fatalf("wishlists.json not found on disk: %v", err)
	}

	// Reopen store from same directory
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}

	ada := s2.Get("cust_ada")
	if len(ada.Items) != 1 || ada.Items[0].ProductID != "prd_lamp" || ada.Items[0].Quantity != 3 {
		t.Fatalf("cust_ada data not restored properly: %+v", ada)
	}

	charles := s2.Get("cust_charles")
	if len(charles.Items) != 1 || charles.Items[0].ProductID != "prd_book" || charles.Items[0].Quantity != 1 {
		t.Fatalf("cust_charles data not restored properly: %+v", charles)
	}
}

func TestPersistedSnapshotContainsNoProductNamesOrPrices(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.SetItem("cust_ada", Item{ProductID: "prd_lamp", Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(dir, "wishlists.json")
	b, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read wishlists.json: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("failed to unmarshal wishlists.json: %v", err)
	}

	wishlists, ok := raw["wishlists"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'wishlists' key in snapshot: %v", raw)
	}
	adaRec, ok := wishlists["cust_ada"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'cust_ada' in wishlists snapshot: %v", wishlists)
	}
	items, ok := adaRec["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected 1 item in cust_ada, got %v", adaRec["items"])
	}
	itemMap, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item is not a map: %v", items[0])
	}

	// Verify only product_id, quantity, and added_at exist
	for k := range itemMap {
		if k != "product_id" && k != "quantity" && k != "added_at" {
			t.Errorf("unexpected field %q found in persisted wishlist item (forbidden to store product metadata)", k)
		}
	}
	if _, hasName := itemMap["name"]; hasName {
		t.Errorf("persisted item MUST NOT contain 'name'")
	}
	if _, hasPrice := itemMap["price"]; hasPrice {
		t.Errorf("persisted item MUST NOT contain 'price'")
	}
}

func TestDamagedFileReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wishlists.json")
	if err := os.WriteFile(path, []byte("{corrupt-json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(dir)
	if err == nil {
		t.Error("expected error when opening damaged wishlists.json, got nil")
	}
}

func TestSemanticValidationOnOpen(t *testing.T) {
	validTimestamp := "2026-11-10T12:00:00Z"

	cases := []struct {
		name string
		json string
	}{
		{
			name: "invalid customer ID",
			json: fmt.Sprintf(`{"wishlists":{"bad customer":{"items":[{"product_id":"prd_1","quantity":1,"added_at":%q}]}}}`, validTimestamp),
		},
		{
			name: "invalid product ID",
			json: fmt.Sprintf(`{"wishlists":{"cust_1":{"items":[{"product_id":"bad product","quantity":1,"added_at":%q}]}}}`, validTimestamp),
		},
		{
			name: "duplicate product ID",
			json: fmt.Sprintf(`{"wishlists":{"cust_1":{"items":[{"product_id":"prd_1","quantity":1,"added_at":%q},{"product_id":"prd_1","quantity":2,"added_at":%q}]}}}`, validTimestamp, validTimestamp),
		},
		{
			name: "invalid quantity zero",
			json: fmt.Sprintf(`{"wishlists":{"cust_1":{"items":[{"product_id":"prd_1","quantity":0,"added_at":%q}]}}}`, validTimestamp),
		},
		{
			name: "invalid quantity over 100",
			json: fmt.Sprintf(`{"wishlists":{"cust_1":{"items":[{"product_id":"prd_1","quantity":101,"added_at":%q}]}}}`, validTimestamp),
		},
		{
			name: "missing added_at timestamp",
			json: `{"wishlists":{"cust_1":{"items":[{"product_id":"prd_1","quantity":1}]}}}`,
		},
		{
			name: "more than 100 items",
			json: func() string {
				items := ""
				for i := 0; i <= 100; i++ {
					if i > 0 {
						items += ","
					}
					items += fmt.Sprintf(`{"product_id":"prd_%03d","quantity":1,"added_at":%q}`, i, validTimestamp)
				}
				return fmt.Sprintf(`{"wishlists":{"cust_1":{"items":[%s]}}}`, items)
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wishlists.json")
			if err := os.WriteFile(path, []byte(tc.json), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Open(dir)
			if err == nil {
				t.Errorf("expected Open to fail for %s, got nil", tc.name)
			}
		})
	}
}

func TestFailedPersistenceRollback(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Initial valid write
	_, err = s.SetItem("cust_safe", Item{ProductID: "prd_1", Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}

	// The disk directory goes away
	os.RemoveAll(dir)

	// Attempt mutation that will fail to save
	_, err = s.SetItem("cust_safe", Item{ProductID: "prd_2", Quantity: 2})
	if err == nil {
		t.Fatal("expected save to fail when directory is gone")
	}

	// Verify in-memory state was rolled back
	wl := s.Get("cust_safe")
	if len(wl.Items) != 1 || wl.Items[0].ProductID != "prd_1" {
		t.Fatalf("expected rollback to 1 item (prd_1), got %+v", wl.Items)
	}

	// Adding a brand new customer when save fails leaves no trace
	if _, err := s.SetItem("cust_new", Item{ProductID: "prd_1", Quantity: 1}); err == nil {
		t.Fatal("expected save to fail for new customer")
	}
	if len(s.Get("cust_new").Items) != 0 {
		t.Error("a wishlist whose first save failed must not exist in memory")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	customers := 20
	opsPerCustomer := 50
	errCh := make(chan error, customers*opsPerCustomer*2)

	for i := 0; i < customers; i++ {
		wg.Add(1)
		cust := fmt.Sprintf("cust_%d", i)
		go func(c string) {
			defer wg.Done()
			for j := 0; j < opsPerCustomer; j++ {
				prod := fmt.Sprintf("prd_%d", j%5)
				if _, err := s.SetItem(c, Item{ProductID: prod, Quantity: (j % 10) + 1}); err != nil {
					errCh <- fmt.Errorf("customer %s set %s: %w", c, prod, err)
				}
				_ = s.Get(c)
				_ = s.Has(c, prod)
				if j%15 == 0 {
					if _, err := s.RemoveItem(c, prod); err != nil {
						errCh <- fmt.Errorf("customer %s remove %s: %w", c, prod, err)
					}
				}
			}
		}(cust)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent operation error: %v", err)
	}

	for i := 0; i < customers; i++ {
		cust := fmt.Sprintf("cust_%d", i)
		wl := s.Get(cust)
		if len(wl.Items) > 5 {
			t.Errorf("customer %s has unexpected item count %d", cust, len(wl.Items))
		}
	}
}

func TestConcurrentOperationsWithPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	customers := 8
	ops := 25
	errCh := make(chan error, customers*ops*2)

	for i := 0; i < customers; i++ {
		wg.Add(1)
		cust := fmt.Sprintf("cust_%d", i)
		go func(c string) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				prod := fmt.Sprintf("prd_%d", j%4)
				if _, err := s.SetItem(c, Item{ProductID: prod, Quantity: (j % 5) + 1}); err != nil {
					errCh <- fmt.Errorf("concurrent persistent set %s/%s: %w", c, prod, err)
				}
				_ = s.Get(c)
				if j%8 == 0 {
					if _, err := s.RemoveItem(c, prod); err != nil {
						errCh <- fmt.Errorf("concurrent persistent remove %s/%s: %w", c, prod, err)
					}
				}
			}
		}(cust)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent persistence error: %v", err)
	}

	// Verify persistence file survived and is valid
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("failed to reopen store after concurrent persistent writes: %v", err)
	}
	for i := 0; i < customers; i++ {
		cust := fmt.Sprintf("cust_%d", i)
		wl := reopened.Get(cust)
		if len(wl.Items) > 4 {
			t.Errorf("customer %s has unexpected item count %d", cust, len(wl.Items))
		}
		for _, it := range wl.Items {
			if it.Quantity < 1 || it.Quantity > 5 {
				t.Errorf("invalid quantity %d for customer %s item %s", it.Quantity, cust, it.ProductID)
			}
			if it.AddedAt.IsZero() {
				t.Errorf("missing AddedAt timestamp for customer %s item %s", cust, it.ProductID)
			}
		}
	}
}
