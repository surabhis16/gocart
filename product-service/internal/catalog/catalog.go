// Package catalog is the Product bounded context: the product catalogue and
// the stock reservations that other services make against it.
//
// Persistence is deliberately boring. The whole state is one JSON snapshot that
// is replaced atomically (write temp file, fsync, rename) on every mutation.
// Every mutation is applied to a copy of the state and only swapped in after
// the snapshot is safely on disk, so a failed write can never leave memory and
// disk disagreeing. That is the right size for a catalogue of thousands of
// rows; the Order service, whose history matters, uses an event log instead.
package catalog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Currency is the only currency GoCart sells in. Amounts are integers in the
// smallest unit (paise), never floats.
const Currency = "INR"

// Limits applied at the boundary of the bounded context.
const (
	MaxNameLen        = 120
	MaxDescriptionLen = 1000
	MaxPriceMinor     = 1_000_000_000
	MaxStock          = 1_000_000
	MaxLines          = 50
	MaxQuantity       = 100
)

var (
	// ErrNotFound is returned when a product does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid is wrapped by every input validation failure.
	ErrInvalid = errors.New("invalid input")
	// ErrReservationMismatch means the order already holds a reservation for
	// different items. Retrying the same request is safe; changing it is not.
	ErrReservationMismatch = errors.New("order already holds a reservation for different items")
)

var orderIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Unwrap() error { return ErrInvalid }

func invalid(format string, args ...any) error {
	return &validationError{msg: fmt.Sprintf(format, args...)}
}

// Shortage describes one line of a reservation that could not be satisfied.
type Shortage struct {
	ProductID string `json:"product_id"`
	Requested int    `json:"requested"`
	Available int    `json:"available"`
}

// InsufficientStockError is returned when at least one line cannot be reserved.
// Nothing is reserved in that case: reservations are all-or-nothing.
type InsufficientStockError struct{ Shortages []Shortage }

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("insufficient stock for %d product(s)", len(e.Shortages))
}

// UnknownProductsError is returned when a reservation names products that do
// not exist.
type UnknownProductsError struct{ IDs []string }

func (e *UnknownProductsError) Error() string {
	return fmt.Sprintf("unknown product(s): %v", e.IDs)
}

// Product is a sellable item and its current stock.
type Product struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceMinor  int64  `json:"price_minor"`
	Currency    string `json:"currency"`
	Stock       int    `json:"stock"`
}

// NewProduct is the input for creating a product.
type NewProduct struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceMinor  int64  `json:"price_minor"`
	Stock       int    `json:"stock"`
}

// Line is one requested product and quantity.
type Line struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

// ReservedLine is a reserved line with the name and price captured at the
// moment of reservation, so the buyer is charged what they were quoted.
type ReservedLine struct {
	ProductID      string `json:"product_id"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
}

// Reservation is stock held for one order.
type Reservation struct {
	OrderID    string         `json:"order_id"`
	Currency   string         `json:"currency"`
	Items      []ReservedLine `json:"items"`
	ReservedAt time.Time      `json:"reserved_at"`
}

type snapshot struct {
	Products     map[string]Product     `json:"products"`
	Reservations map[string]Reservation `json:"reservations"`
}

func newSnapshot() snapshot {
	return snapshot{Products: map[string]Product{}, Reservations: map[string]Reservation{}}
}

// clone copies the maps. Reservation.Items is shared, which is safe because a
// reservation's items are never modified after creation.
func (s snapshot) clone() snapshot {
	c := snapshot{
		Products:     make(map[string]Product, len(s.Products)),
		Reservations: make(map[string]Reservation, len(s.Reservations)),
	}
	for k, v := range s.Products {
		c.Products[k] = v
	}
	for k, v := range s.Reservations {
		c.Reservations[k] = v
	}
	return c
}

// Store holds the catalogue. It is safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	snap snapshot
	path string // empty means memory only
	now  func() time.Time
}

// Open loads the catalogue from dir, creating it if needed. An empty dir gives
// a memory-only store, which is what the tests use.
func Open(dir string) (*Store, error) {
	s := &Store{snap: newSnapshot(), now: time.Now}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s.path = filepath.Join(dir, "catalog.json")
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read catalogue: %w", err)
	}
	if err := json.Unmarshal(b, &s.snap); err != nil {
		return nil, fmt.Errorf("catalogue %s is corrupt: %w", s.path, err)
	}
	if s.snap.Products == nil {
		s.snap.Products = map[string]Product{}
	}
	if s.snap.Reservations == nil {
		s.snap.Reservations = map[string]Reservation{}
	}
	return s, nil
}

// commit persists next and, only if that succeeds, makes it current.
// The caller must hold the write lock.
func (s *Store) commit(next snapshot) error {
	if s.path != "" {
		if err := writeAtomic(s.path, next); err != nil {
			return fmt.Errorf("persist catalogue: %w", err)
		}
	}
	s.snap = next
	return nil
}

func writeAtomic(path string, snap snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "catalog-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	// Make the rename itself durable. Not supported everywhere, so best effort.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// List returns every product ordered by ID.
func (s *Store) List() []Product {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Product, 0, len(s.snap.Products))
	for _, p := range s.snap.Products {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns one product.
func (s *Store) Get(id string) (Product, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.snap.Products[id]
	if !ok {
		return Product{}, ErrNotFound
	}
	return p, nil
}

// Create adds a product with a generated ID.
func (s *Store) Create(in NewProduct) (Product, error) {
	if err := validateNew(in); err != nil {
		return Product{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newID("prd")
	for _, taken := s.snap.Products[id]; taken; _, taken = s.snap.Products[id] {
		id = newID("prd")
	}
	p := Product{
		ID: id, Name: in.Name, Description: in.Description,
		PriceMinor: in.PriceMinor, Currency: Currency, Stock: in.Stock,
	}
	next := s.snap.clone()
	next.Products[id] = p
	if err := s.commit(next); err != nil {
		return Product{}, err
	}
	return p, nil
}

// SetStock sets the absolute stock level of a product (restocking).
func (s *Store) SetStock(id string, stock int) (Product, error) {
	if stock < 0 || stock > MaxStock {
		return Product{}, invalid("stock must be between 0 and %d", MaxStock)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.snap.Products[id]
	if !ok {
		return Product{}, ErrNotFound
	}
	p.Stock = stock
	next := s.snap.clone()
	next.Products[id] = p
	if err := s.commit(next); err != nil {
		return Product{}, err
	}
	return p, nil
}

// Reserve holds stock for an order, all lines or none.
//
// It is idempotent per order ID: repeating the same request returns the
// original reservation without touching stock (replayed is true). That is what
// makes it safe for the Order service to retry after a timeout, where it cannot
// know whether the first attempt landed.
func (s *Store) Reserve(orderID string, lines []Line) (res Reservation, replayed bool, err error) {
	if err := validateReserve(orderID, lines); err != nil {
		return Reservation{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.snap.Reservations[orderID]; ok {
		if sameItems(existing.Items, lines) {
			return existing, true, nil
		}
		return Reservation{}, false, ErrReservationMismatch
	}

	var unknown []string
	var short []Shortage
	for _, l := range lines {
		p, ok := s.snap.Products[l.ProductID]
		switch {
		case !ok:
			unknown = append(unknown, l.ProductID)
		case p.Stock < l.Quantity:
			short = append(short, Shortage{ProductID: l.ProductID, Requested: l.Quantity, Available: p.Stock})
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Reservation{}, false, &UnknownProductsError{IDs: unknown}
	}
	if len(short) > 0 {
		sort.Slice(short, func(i, j int) bool { return short[i].ProductID < short[j].ProductID })
		return Reservation{}, false, &InsufficientStockError{Shortages: short}
	}

	next := s.snap.clone()
	items := make([]ReservedLine, 0, len(lines))
	for _, l := range lines {
		p := next.Products[l.ProductID]
		p.Stock -= l.Quantity
		next.Products[l.ProductID] = p
		items = append(items, ReservedLine{
			ProductID: p.ID, Name: p.Name, Quantity: l.Quantity, UnitPriceMinor: p.PriceMinor,
		})
	}
	res = Reservation{OrderID: orderID, Currency: Currency, Items: items, ReservedAt: s.now().UTC()}
	next.Reservations[orderID] = res
	if err := s.commit(next); err != nil {
		return Reservation{}, false, err
	}
	return res, false, nil
}

// Release returns an order's reserved stock to the shelf. Releasing an order
// that holds nothing is not an error, so compensation can be retried freely.
func (s *Store) Release(orderID string) (released bool, err error) {
	if !orderIDPattern.MatchString(orderID) {
		return false, invalid("order_id must match %s", orderIDPattern)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.snap.Reservations[orderID]
	if !ok {
		return false, nil
	}
	next := s.snap.clone()
	for _, it := range res.Items {
		if p, ok := next.Products[it.ProductID]; ok {
			p.Stock += it.Quantity
			next.Products[it.ProductID] = p
		}
	}
	delete(next.Reservations, orderID)
	if err := s.commit(next); err != nil {
		return false, err
	}
	return true, nil
}

// SeedIfEmpty loads a small demo catalogue into an empty store. Two items are
// deliberately awkward (almost sold out, sold out) so that the failure paths of
// the Order service can be demonstrated without any setup.
func (s *Store) SeedIfEmpty() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snap.Products) > 0 {
		return 0, nil
	}
	demo := []Product{
		{ID: "prd_keyboard", Name: "Mechanical Keyboard (Tenkeyless)", Description: "Hot-swappable switches, USB-C.", PriceMinor: 499900, Stock: 25},
		{ID: "prd_hub", Name: "USB-C Hub, 7-in-1", Description: "HDMI, SD card reader, 3x USB-A, PD pass-through.", PriceMinor: 249900, Stock: 40},
		{ID: "prd_notebook", Name: "A5 Dotted Notebook", Description: "160 pages, lay-flat binding.", PriceMinor: 34900, Stock: 200},
		{ID: "prd_bottle", Name: "Insulated Steel Bottle, 750 ml", Description: "Keeps drinks cold for 24 hours.", PriceMinor: 79900, Stock: 60},
		{ID: "prd_lamp", Name: "LED Desk Lamp", Description: "Three colour temperatures. Only a few left.", PriceMinor: 129900, Stock: 3},
		{ID: "prd_deskmat", Name: "Limited Edition Desk Mat", Description: "Sold out.", PriceMinor: 189900, Stock: 0},
	}
	next := s.snap.clone()
	for _, p := range demo {
		p.Currency = Currency
		next.Products[p.ID] = p
	}
	if err := s.commit(next); err != nil {
		return 0, err
	}
	return len(demo), nil
}

func validateNew(in NewProduct) error {
	switch {
	case in.Name == "" || len(in.Name) > MaxNameLen:
		return invalid("name must be 1-%d characters", MaxNameLen)
	case len(in.Description) > MaxDescriptionLen:
		return invalid("description must be at most %d characters", MaxDescriptionLen)
	case in.PriceMinor < 1 || in.PriceMinor > MaxPriceMinor:
		return invalid("price_minor must be between 1 and %d", MaxPriceMinor)
	case in.Stock < 0 || in.Stock > MaxStock:
		return invalid("stock must be between 0 and %d", MaxStock)
	}
	return nil
}

func validateReserve(orderID string, lines []Line) error {
	if !orderIDPattern.MatchString(orderID) {
		return invalid("order_id must match %s", orderIDPattern)
	}
	if len(lines) == 0 || len(lines) > MaxLines {
		return invalid("items must contain 1-%d lines", MaxLines)
	}
	seen := make(map[string]bool, len(lines))
	for _, l := range lines {
		switch {
		case l.ProductID == "":
			return invalid("every item needs a product_id")
		case l.Quantity < 1 || l.Quantity > MaxQuantity:
			return invalid("quantity for %s must be between 1 and %d", l.ProductID, MaxQuantity)
		case seen[l.ProductID]:
			return invalid("product %s appears more than once; combine the quantities", l.ProductID)
		}
		seen[l.ProductID] = true
	}
	return nil
}

func sameItems(held []ReservedLine, want []Line) bool {
	if len(held) != len(want) {
		return false
	}
	qty := make(map[string]int, len(held))
	for _, h := range held {
		qty[h.ProductID] = h.Quantity
	}
	for _, w := range want {
		if q, ok := qty[w.ProductID]; !ok || q != w.Quantity {
			return false
		}
	}
	return true
}

func newID(prefix string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
