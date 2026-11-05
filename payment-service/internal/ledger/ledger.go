// Package ledger is the Payment service's record of what it has charged.
//
// One rule shapes everything here: a charge is identified by the order it is
// for, and asking twice must never charge twice. The Order service retries
// when it cannot tell whether a request landed, so this service sees the same
// charge request repeatedly, and has to answer every repeat identically.
//
// Three outcomes are stored, because three are definite:
//
//	approved   the provider took the money
//	declined   the provider refused; asking again will not change that
//	voided     the charge was cancelled (refunded if it had been approved)
//
// A fourth outcome, "the provider did not answer", is deliberately NOT stored.
// Nothing is known, so nothing is recorded, and the caller may try again.
package ledger

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"gocart/payment-service/internal/gateway"
)

// Status is where a charge stands.
type Status string

const (
	StatusApproved Status = "approved"
	StatusDeclined Status = "declined"
	StatusVoided   Status = "voided"
)

// Errors a caller can test for with errors.Is.
var (
	ErrInvalid             = errors.New("invalid request")
	ErrNotFound            = errors.New("no charge for this order")
	ErrMismatch            = errors.New("a charge for this order exists with a different amount or currency")
	ErrVoided              = errors.New("this order's charge was cancelled; it can no longer be charged")
	ErrProviderUnavailable = errors.New("payment provider unavailable")
)

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }
func (e *validationError) Unwrap() error { return ErrInvalid }

func invalid(format string, args ...any) error {
	return &validationError{msg: fmt.Sprintf(format, args...)}
}

// Charge is one order's payment.
type Charge struct {
	OrderID     string     `json:"order_id"`
	Status      Status     `json:"status"`
	AmountMinor int64      `json:"amount_minor"`
	Currency    string     `json:"currency"`
	Reference   string     `json:"reference,omitempty"` // the provider-side id; empty for a tombstone
	DeclineCode string     `json:"decline_code,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	VoidedAt    *time.Time `json:"voided_at,omitempty"`
}

// Authorizer is the provider. The mock in package gateway satisfies it.
type Authorizer interface {
	Authorize(orderID, token string) gateway.Outcome
}

var (
	orderIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// maxAmountMinor keeps arithmetic far from overflow and rejects typos.
const maxAmountMinor = 100_000_000_00 // one crore rupees, in paise

// Store holds the ledger. The zero value is not usable; call Open.
type Store struct {
	auth Authorizer
	path string
	now  func() time.Time

	mu      sync.Mutex // guards charges and locks
	charges map[string]Charge
	locks   map[string]*sync.Mutex
}

// Open loads the ledger kept in dir, creating it if absent.
func Open(dir string, auth Authorizer) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{
		auth:    auth,
		path:    filepath.Join(dir, "ledger.json"),
		now:     func() time.Time { return time.Now().UTC() },
		charges: map[string]Charge{},
		locks:   map[string]*sync.Mutex{},
	}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	if err := json.Unmarshal(b, &s.charges); err != nil {
		// Refuse to start rather than quietly forget what was charged.
		return nil, fmt.Errorf("ledger file is corrupt: %w", err)
	}
	return s, nil
}

// orderLock returns the mutex that serialises everything for one order. The
// provider call happens while it is held, so a retry that arrives while the
// first attempt is still in flight waits and then sees its result, instead of
// racing it into a second charge.
func (s *Store) orderLock(orderID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[orderID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[orderID] = l
	}
	return l
}

func validateCharge(orderID string, amount int64, currency, token string) error {
	if !orderIDPattern.MatchString(orderID) {
		return invalid("order_id must be 1-64 characters from A-Z a-z 0-9 . _ : -")
	}
	if amount <= 0 {
		return invalid("amount_minor must be positive")
	}
	if amount > maxAmountMinor {
		return invalid("amount_minor exceeds the per-charge limit")
	}
	if !currencyPattern.MatchString(currency) {
		return invalid("currency must be a three-letter ISO 4217 code such as INR")
	}
	if !gateway.ValidToken(token) {
		return invalid("payment_token must be a provider token (tok_...), never a card number")
	}
	return nil
}

// Charge takes payment for an order. replayed is true when the answer is a
// previously recorded one and the provider was not asked again.
func (s *Store) Charge(orderID string, amountMinor int64, currency, token string) (c Charge, replayed bool, err error) {
	if err := validateCharge(orderID, amountMinor, currency, token); err != nil {
		return Charge{}, false, err
	}
	l := s.orderLock(orderID)
	l.Lock()
	defer l.Unlock()

	s.mu.Lock()
	existing, found := s.charges[orderID]
	s.mu.Unlock()
	if found {
		if existing.Status == StatusVoided && existing.Reference == "" {
			return Charge{}, false, ErrVoided // tombstone: cancelled before it was ever charged
		}
		if existing.AmountMinor != amountMinor || existing.Currency != currency {
			return Charge{}, false, ErrMismatch
		}
		return existing, true, nil
	}

	out := s.auth.Authorize(orderID, token)
	if out.Unavailable {
		return Charge{}, false, ErrProviderUnavailable
	}
	rec := Charge{
		OrderID:     orderID,
		AmountMinor: amountMinor,
		Currency:    currency,
		CreatedAt:   s.now(),
	}
	if out.Approved {
		rec.Status = StatusApproved
		rec.Reference = newReference()
	} else {
		rec.Status = StatusDeclined
		rec.DeclineCode = out.DeclineCode
	}
	if err := s.put(rec); err != nil {
		// The provider may have taken the money but we could not record it.
		// A real provider deduplicates on its own idempotency key; the mock
		// is deterministic, so the retry reaches the same answer.
		return Charge{}, false, err
	}
	return rec, false, nil
}

// Cancel voids an order's charge. An approved charge becomes voided (a
// refund). With no charge at all it records a tombstone, so a charge request
// that is still on its way can never succeed afterwards. changed is false when
// the call had no effect (already voided, or declined, so nothing to undo).
func (s *Store) Cancel(orderID string) (c Charge, changed bool, err error) {
	if !orderIDPattern.MatchString(orderID) {
		return Charge{}, false, invalid("order_id must be 1-64 characters from A-Z a-z 0-9 . _ : -")
	}
	l := s.orderLock(orderID)
	l.Lock()
	defer l.Unlock()

	s.mu.Lock()
	existing, found := s.charges[orderID]
	s.mu.Unlock()

	now := s.now()
	switch {
	case !found:
		rec := Charge{OrderID: orderID, Status: StatusVoided, Currency: "", CreatedAt: now, VoidedAt: &now}
		if err := s.put(rec); err != nil {
			return Charge{}, false, err
		}
		return rec, true, nil
	case existing.Status == StatusApproved:
		existing.Status = StatusVoided
		existing.VoidedAt = &now
		if err := s.put(existing); err != nil {
			return Charge{}, false, err
		}
		return existing, true, nil
	default:
		return existing, false, nil
	}
}

// Get returns an order's charge.
func (s *Store) Get(orderID string) (Charge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.charges[orderID]
	if !ok {
		return Charge{}, ErrNotFound
	}
	return c, nil
}

// Counts returns how many charges are in each status, for /metrics.
func (s *Store) Counts() map[Status]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[Status]int{StatusApproved: 0, StatusDeclined: 0, StatusVoided: 0}
	for _, c := range s.charges {
		out[c.Status]++
	}
	return out
}

// List returns every charge, newest first.
func (s *Store) List() []Charge {
	s.mu.Lock()
	out := make([]Charge, 0, len(s.charges))
	for _, c := range s.charges {
		out = append(out, c)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].OrderID < out[j].OrderID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// put records c and persists the ledger. If persisting fails the change is
// rolled back, so memory never claims something the disk does not.
func (s *Store) put(c Charge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.charges[c.OrderID]
	s.charges[c.OrderID] = c
	if err := writeAtomic(s.path, s.charges); err != nil {
		if had {
			s.charges[c.OrderID] = prev
		} else {
			delete(s.charges, c.OrderID)
		}
		return fmt.Errorf("persist ledger: %w", err)
	}
	return nil
}

func writeAtomic(path string, charges map[string]Charge) error {
	b, err := json.Marshal(charges)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "ledger-*.tmp")
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
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func newReference() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the OS random source failing is not recoverable
	}
	return "pay_" + hex.EncodeToString(b[:])
}
