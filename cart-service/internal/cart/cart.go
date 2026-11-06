// Package cart holds shoppers' carts. A cart is a list of (product, quantity)
// lines for one customer, kept in a single JSON file, rewritten atomically.
//
// Two decisions shape it:
//
//   - A cart never stores a price. Prices belong to the Product service, and a
//     copy kept here would go stale and then disagree with what the Order
//     service actually charges. The cart says what you want; the order says
//     what it costs.
//
//   - Every cart has a revision that changes whenever its contents do. Checkout
//     derives its Idempotency-Key from it, which is what makes checkout safe to
//     retry: the same cart in the same state always means the same order.
package cart

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

// Limits. They match what the Order service will accept, so a cart that
// passes here cannot be refused later for its shape.
const (
	MaxLines    = 50
	MaxQuantity = 100
)

var (
	// ErrInvalid wraps every validation failure.
	ErrInvalid = errors.New("invalid cart request")
	idPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Line is one product in a cart. Name is a convenience copy taken when the
// line was added, so the cart can be shown without calling the Product service.
type Line struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	Quantity  int    `json:"quantity"`
}

// Cart is a customer's cart as the API shows it.
type Cart struct {
	CustomerID string `json:"customer_id"`
	Items      []Line `json:"items"`
	Revision   int64  `json:"revision"`
	// OrderID is set once the cart has been checked out. The cart then keeps
	// the lines that were bought, so a retried checkout finds the same order;
	// the next change to the cart starts a fresh one.
	OrderID   string    `json:"order_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// CheckedOut reports whether the cart's contents have become an order.
func (c Cart) CheckedOut() bool { return c.OrderID != "" }

type record struct {
	Items     []Line    `json:"items"`
	Revision  int64     `json:"revision"`
	OrderID   string    `json:"order_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type snapshot struct {
	Carts map[string]record `json:"carts"`
}

// Store keeps carts in memory and in one file.
type Store struct {
	mu    sync.Mutex
	path  string
	carts map[string]record
	now   func() time.Time
}

// Open loads the carts in dir, creating dir if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "carts.json"), carts: map[string]record{}, now: time.Now}
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
	if snap.Carts != nil {
		s.carts = snap.Carts
	}
	return s, nil
}

// ValidCustomerID reports whether id is acceptable in a URL path and in an
// Idempotency-Key.
func ValidCustomerID(id string) bool { return idPattern.MatchString(id) }

func view(customer string, r record) Cart {
	items := append([]Line{}, r.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
	return Cart{CustomerID: customer, Items: items, Revision: r.Revision, OrderID: r.OrderID, UpdatedAt: r.UpdatedAt}
}

// Get returns a customer's cart. A customer who has none has an empty one.
func (s *Store) Get(customer string) Cart {
	s.mu.Lock()
	defer s.mu.Unlock()
	return view(customer, s.carts[customer])
}

// Has reports whether the cart already contains the product, which lets the
// caller skip the Product service lookup for a quantity change.
func (s *Store) Has(customer, productID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.carts[customer].Items {
		if l.ProductID == productID {
			return true
		}
	}
	return false
}

// fresh is a cart's state after something other than checkout changed it: a
// checked-out cart is cleared first, so changes never edit an order that exists.
func fresh(r record) record {
	if r.OrderID != "" {
		return record{Revision: r.Revision + 1}
	}
	return r
}

// commit installs next for the customer and saves. If saving fails the cart is
// put back, so memory never claims more than the disk holds.
func (s *Store) commit(customer string, next record) error {
	prev, had := s.carts[customer]
	next.UpdatedAt = s.now().UTC()
	s.carts[customer] = next
	if err := s.save(); err != nil {
		if had {
			s.carts[customer] = prev
		} else {
			delete(s.carts, customer)
		}
		return err
	}
	return nil
}

// SetItem sets the quantity of one product, adding the line if needed. Setting
// the quantity a line already has changes nothing, so the call is idempotent.
func (s *Store) SetItem(customer string, line Line) (Cart, error) {
	if !idPattern.MatchString(customer) {
		return Cart{}, invalid("customer id must be 1-64 characters from A-Z a-z 0-9 . _ -")
	}
	if line.ProductID == "" || len(line.ProductID) > 64 {
		return Cart{}, invalid("product id must be 1-64 characters")
	}
	if line.Quantity < 1 || line.Quantity > MaxQuantity {
		return Cart{}, invalid("quantity must be between 1 and %d", MaxQuantity)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.carts[customer]
	next := fresh(cur)
	next.Items = append([]Line(nil), next.Items...)
	found := false
	for i, l := range next.Items {
		if l.ProductID == line.ProductID {
			found = true
			if l.Quantity == line.Quantity && cur.OrderID == "" {
				return view(customer, cur), nil
			}
			next.Items[i].Quantity = line.Quantity
		}
	}
	if !found {
		if len(next.Items) >= MaxLines {
			return Cart{}, invalid("a cart holds at most %d different products", MaxLines)
		}
		next.Items = append(next.Items, line)
	}
	next.Revision++
	if err := s.commit(customer, next); err != nil {
		return Cart{}, err
	}
	return view(customer, s.carts[customer]), nil
}

// RemoveItem takes a product out. Removing one that is not there succeeds.
func (s *Store) RemoveItem(customer, productID string) (Cart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.carts[customer]
	if cur.OrderID == "" {
		kept := make([]Line, 0, len(cur.Items))
		for _, l := range cur.Items {
			if l.ProductID != productID {
				kept = append(kept, l)
			}
		}
		if len(kept) == len(cur.Items) {
			return view(customer, cur), nil
		}
		next := cur
		next.Items = kept
		next.Revision++
		if err := s.commit(customer, next); err != nil {
			return Cart{}, err
		}
		return view(customer, s.carts[customer]), nil
	}
	// A checked-out cart is frozen history; removing from it starts over.
	return s.clearLocked(customer)
}

// Clear empties the cart. Clearing an empty cart succeeds and changes nothing.
func (s *Store) Clear(customer string) (Cart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clearLocked(customer)
}

func (s *Store) clearLocked(customer string) (Cart, error) {
	cur, ok := s.carts[customer]
	if !ok || (len(cur.Items) == 0 && cur.OrderID == "") {
		return view(customer, cur), nil
	}
	if err := s.commit(customer, record{Revision: cur.Revision + 1}); err != nil {
		return Cart{}, err
	}
	return view(customer, s.carts[customer]), nil
}

// MarkOrdered records that the cart at the given revision became an order. It
// does nothing if the cart changed in the meantime: the customer edited it
// while checkout was in flight, and those edits are not part of the order.
func (s *Store) MarkOrdered(customer string, revision int64, orderID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.carts[customer]
	if cur.Revision != revision || cur.OrderID == orderID {
		return nil
	}
	next := cur
	next.OrderID = orderID
	return s.commit(customer, next)
}

// Renew moves a cart to a new revision without changing its lines, if it is
// still at the given one. Checkout calls it after a definite refusal: the same
// Idempotency-Key would replay the refusal forever, so the next attempt needs
// a new one.
func (s *Store) Renew(customer string, revision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.carts[customer]
	if cur.Revision != revision {
		return nil
	}
	next := cur
	next.Revision++
	return s.commit(customer, next)
}

// Counts returns how many carts are open (have lines, not yet ordered) and
// how many are checked out.
func (s *Store) Counts() (open, ordered int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.carts {
		switch {
		case r.OrderID != "":
			ordered++
		case len(r.Items) > 0:
			open++
		}
	}
	return open, ordered
}

// CheckoutKey is the Idempotency-Key checkout sends for a cart in a given
// state. It is a pure function of the customer and revision, so a retried
// checkout sends the same key and cannot create a second order.
func CheckoutKey(customer string, revision int64) string {
	return fmt.Sprintf("cart:%s:%d", customer, revision)
}

func (s *Store) save() error {
	b, err := json.Marshal(snapshot{Carts: s.carts})
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "carts-*.tmp")
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
	if d, err := os.Open(dir); err == nil { // make the rename durable; best effort
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
