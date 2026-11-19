// Package wishlist holds shoppers' saved-for-later items. A wishlist is a list
// of (product, quantity) items for one customer, kept in a single JSON file,
// rewritten atomically.
//
// A wishlist never stores product names, prices or stock levels. Names, prices
// and stock belong to the Product service.
package wishlist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Limits for wishlists.
const (
	MaxLines    = 100
	MaxQuantity = 100
)

var (
	// ErrInvalid wraps every validation failure.
	ErrInvalid = errors.New("invalid wishlist request")
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Item is one product saved in a customer's wishlist.
type Item struct {
	ProductID string    `json:"product_id"`
	Quantity  int       `json:"quantity"`
	AddedAt   time.Time `json:"added_at"`
}

// Wishlist is a customer's wishlist as the API presents it.
type Wishlist struct {
	CustomerID string    `json:"customer_id"`
	Items      []Item    `json:"items"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

type record struct {
	Items     []Item    `json:"items"`
	UpdatedAt time.Time `json:"updated_at"`
}

type snapshot struct {
	Wishlists map[string]record `json:"wishlists"`
}

// Store keeps wishlists in memory and in one file.
type Store struct {
	mu        sync.RWMutex
	path      string
	wishlists map[string]record
	now       func() time.Time
}

// Open loads the wishlists in dir, creating dir if needed. An empty dir gives
// a memory-only store.
func Open(dir string) (*Store, error) {
	s := &Store{wishlists: map[string]record{}, now: time.Now}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s.path = filepath.Join(dir, "wishlists.json")
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", s.path, err)
	}
	if snap.Wishlists != nil {
		if err := validateSnapshot(snap.Wishlists, s.path); err != nil {
			return nil, err
		}
		s.wishlists = snap.Wishlists
	}
	return s, nil
}

func validateSnapshot(wishlists map[string]record, path string) error {
	for custID, rec := range wishlists {
		if !ValidCustomerID(custID) {
			return fmt.Errorf("%s is damaged: invalid customer id %q", path, custID)
		}
		if len(rec.Items) > MaxLines {
			return fmt.Errorf("%s is damaged: customer %q has %d items, exceeding max %d", path, custID, len(rec.Items), MaxLines)
		}
		seen := make(map[string]bool, len(rec.Items))
		for _, it := range rec.Items {
			if !ValidProductID(it.ProductID) {
				return fmt.Errorf("%s is damaged: customer %q has invalid product id %q", path, custID, it.ProductID)
			}
			if seen[it.ProductID] {
				return fmt.Errorf("%s is damaged: customer %q has duplicate product %q", path, custID, it.ProductID)
			}
			seen[it.ProductID] = true
			if it.Quantity < 1 || it.Quantity > MaxQuantity {
				return fmt.Errorf("%s is damaged: customer %q item %q has invalid quantity %d", path, custID, it.ProductID, it.Quantity)
			}
			if it.AddedAt.IsZero() {
				return fmt.Errorf("%s is damaged: customer %q item %q has missing added_at timestamp", path, custID, it.ProductID)
			}
		}
	}
	return nil
}

// ValidCustomerID reports whether id is acceptable in a URL path.
func ValidCustomerID(id string) bool { return idPattern.MatchString(id) }

// ValidProductID reports whether id is acceptable for a product.
func ValidProductID(id string) bool { return idPattern.MatchString(id) }

func view(customer string, r record) Wishlist {
	items := make([]Item, len(r.Items))
	copy(items, r.Items)
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
	return Wishlist{CustomerID: customer, Items: items, UpdatedAt: r.UpdatedAt}
}

// Get returns a customer's wishlist. A customer who has none gets an empty one.
func (s *Store) Get(customer string) Wishlist {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return view(customer, s.wishlists[customer])
}

// Has reports whether the wishlist already contains the product.
func (s *Store) Has(customer, productID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.wishlists[customer].Items {
		if it.ProductID == productID {
			return true
		}
	}
	return false
}

// Item returns the existing item for the customer and product, if present.
func (s *Store) Item(customer, productID string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, it := range s.wishlists[customer].Items {
		if it.ProductID == productID {
			return it, true
		}
	}
	return Item{}, false
}

// commit installs next for the customer and saves. If saving fails the wishlist
// is rolled back to previous state in memory.
func (s *Store) commit(customer string, next record) error {
	prev, had := s.wishlists[customer]
	next.UpdatedAt = s.now().UTC()
	s.wishlists[customer] = next
	if err := s.save(); err != nil {
		if had {
			s.wishlists[customer] = prev
		} else {
			delete(s.wishlists, customer)
		}
		return err
	}
	return nil
}

// SetItem sets the quantity of one product in the wishlist, adding it if
// needed. Setting the same quantity changes nothing, so the call is idempotent.
func (s *Store) SetItem(customer string, item Item) (Wishlist, error) {
	if !ValidCustomerID(customer) {
		return Wishlist{}, invalid("customer id must be 1-64 characters from A-Z a-z 0-9 . _ -")
	}
	if !ValidProductID(item.ProductID) {
		return Wishlist{}, invalid("product id must be 1-64 characters from A-Z a-z 0-9 . _ -")
	}
	if item.Quantity < 1 || item.Quantity > MaxQuantity {
		return Wishlist{}, invalid("quantity must be between 1 and %d", MaxQuantity)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.wishlists[customer]
	next := record{
		Items:     append([]Item(nil), cur.Items...),
		UpdatedAt: cur.UpdatedAt,
	}
	found := false
	for i, it := range next.Items {
		if it.ProductID == item.ProductID {
			found = true
			if it.Quantity == item.Quantity {
				return view(customer, cur), nil
			}
			next.Items[i].Quantity = item.Quantity
			break
		}
	}
	if !found {
		if len(next.Items) >= MaxLines {
			return Wishlist{}, invalid("a wishlist holds at most %d different products", MaxLines)
		}
		if item.AddedAt.IsZero() {
			item.AddedAt = s.now().UTC()
		}
		next.Items = append(next.Items, item)
	}
	if err := s.commit(customer, next); err != nil {
		return Wishlist{}, err
	}
	return view(customer, s.wishlists[customer]), nil
}

// RemoveItem takes a product out of the wishlist. Removing one that is not
// there succeeds and changes nothing.
func (s *Store) RemoveItem(customer, productID string) (Wishlist, error) {
	if !ValidCustomerID(customer) {
		return Wishlist{}, invalid("customer id must be 1-64 characters from A-Z a-z 0-9 . _ -")
	}
	if !ValidProductID(productID) {
		return Wishlist{}, invalid("product id must be 1-64 characters from A-Z a-z 0-9 . _ -")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.wishlists[customer]
	kept := make([]Item, 0, len(cur.Items))
	for _, it := range cur.Items {
		if it.ProductID != productID {
			kept = append(kept, it)
		}
	}
	if len(kept) == len(cur.Items) {
		return view(customer, cur), nil
	}
	next := record{Items: kept}
	if err := s.commit(customer, next); err != nil {
		return Wishlist{}, err
	}
	return view(customer, s.wishlists[customer]), nil
}

// Clear empties the wishlist. Clearing an empty wishlist succeeds and changes nothing.
func (s *Store) Clear(customer string) (Wishlist, error) {
	if !ValidCustomerID(customer) {
		return Wishlist{}, invalid("customer id must be 1-64 characters from A-Z a-z 0-9 . _ -")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.wishlists[customer]
	if !ok || len(cur.Items) == 0 {
		return view(customer, cur), nil
	}
	if err := s.commit(customer, record{}); err != nil {
		return Wishlist{}, err
	}
	return view(customer, s.wishlists[customer]), nil
}

// Counts returns the number of non-empty wishlists stored.
func (s *Store) Counts() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, r := range s.wishlists {
		if len(r.Items) > 0 {
			n++
		}
	}
	return n
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(snapshot{Wishlists: s.wishlists})
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "wishlists-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error { _ = tmp.Close(); _ = os.Remove(name); return err }
	if _, err := tmp.Write(b); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		_ = os.Remove(name)
		return err
	}
	// The file has been atomically replaced on disk. Directory fsync ensures the
	// directory entry is flushed to durable storage. Any error here must not roll
	// back in-memory state, as disk and memory would then disagree.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
