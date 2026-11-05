package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gocart/payment-service/internal/gateway"
)

// countingGateway wraps the mock and counts how often the provider is asked.
type countingGateway struct {
	inner *gateway.Gateway
	calls atomic.Int64
	gate  chan struct{} // if set, Authorize blocks until it is closed
}

func (c *countingGateway) Authorize(orderID, token string) gateway.Outcome {
	c.calls.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	return c.inner.Authorize(orderID, token)
}

func open(t *testing.T) (*Store, *countingGateway, string) {
	t.Helper()
	dir := t.TempDir()
	gw := &countingGateway{inner: gateway.New(10 * time.Millisecond)}
	s, err := Open(dir, gw)
	if err != nil {
		t.Fatal(err)
	}
	return s, gw, dir
}

func TestApprovedChargeIsRecorded(t *testing.T) {
	s, _, _ := open(t)
	c, replayed, err := s.Charge("ord_1", 5000, "INR", "tok_visa")
	if err != nil || replayed {
		t.Fatalf("err=%v replayed=%v", err, replayed)
	}
	if c.Status != StatusApproved || c.Reference == "" || c.AmountMinor != 5000 {
		t.Fatalf("unexpected charge %+v", c)
	}
}

func TestRepeatedChargeDoesNotAskTheProviderAgain(t *testing.T) {
	s, gw, _ := open(t)
	first, _, _ := s.Charge("ord_1", 5000, "INR", "tok_visa")
	second, replayed, err := s.Charge("ord_1", 5000, "INR", "tok_visa")
	if err != nil || !replayed {
		t.Fatalf("err=%v replayed=%v", err, replayed)
	}
	if second.Reference != first.Reference {
		t.Fatalf("a repeat produced a new reference: %s vs %s", first.Reference, second.Reference)
	}
	if n := gw.calls.Load(); n != 1 {
		t.Fatalf("provider asked %d times, want 1", n)
	}
}

func TestDeclineIsDefiniteAndReplayed(t *testing.T) {
	cases := map[string]string{
		"tok_declined":           gateway.DeclineCard,
		"tok_insufficient_funds": gateway.DeclineInsufficientFunds,
		"tok_expired_card":       gateway.DeclineExpiredCard,
	}
	for token, code := range cases {
		t.Run(token, func(t *testing.T) {
			s, gw, _ := open(t)
			c, _, err := s.Charge("ord_1", 100, "INR", token)
			if err != nil {
				t.Fatal(err)
			}
			if c.Status != StatusDeclined || c.DeclineCode != code || c.Reference != "" {
				t.Fatalf("unexpected %+v", c)
			}
			// Even with a good card, the same order stays declined: a new
			// attempt is a new order, which is what makes the answer definite.
			again, replayed, err := s.Charge("ord_1", 100, "INR", "tok_visa")
			if err != nil || !replayed || again.Status != StatusDeclined {
				t.Fatalf("again=%+v replayed=%v err=%v", again, replayed, err)
			}
			if gw.calls.Load() != 1 {
				t.Fatalf("provider asked %d times", gw.calls.Load())
			}
		})
	}
}

func TestDifferentAmountForSameOrderIsAMismatch(t *testing.T) {
	s, _, _ := open(t)
	_, _, _ = s.Charge("ord_1", 5000, "INR", "tok_visa")
	for _, tc := range []struct {
		amount int64
		cur    string
	}{{5001, "INR"}, {5000, "USD"}} {
		if _, _, err := s.Charge("ord_1", tc.amount, tc.cur, "tok_visa"); !errors.Is(err, ErrMismatch) {
			t.Fatalf("%v: want ErrMismatch, got %v", tc, err)
		}
	}
}

func TestUnavailableIsNotRecordedAndCanBeRetried(t *testing.T) {
	s, gw, _ := open(t)
	_, _, err := s.Charge("ord_1", 100, "INR", "tok_flaky")
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("first attempt: want unavailable, got %v", err)
	}
	if _, err := s.Get("ord_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown outcome must not be recorded, Get returned %v", err)
	}
	c, replayed, err := s.Charge("ord_1", 100, "INR", "tok_flaky")
	if err != nil || replayed || c.Status != StatusApproved {
		t.Fatalf("retry: %+v replayed=%v err=%v", c, replayed, err)
	}
	if gw.calls.Load() != 2 {
		t.Fatalf("provider asked %d times, want 2", gw.calls.Load())
	}
}

func TestPermanentlyDownProviderNeverRecords(t *testing.T) {
	s, _, _ := open(t)
	for i := 0; i < 3; i++ {
		if _, _, err := s.Charge("ord_1", 100, "INR", "tok_down"); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if n := s.Counts()[StatusApproved] + s.Counts()[StatusDeclined]; n != 0 {
		t.Fatalf("%d charges recorded", n)
	}
}

func TestValidation(t *testing.T) {
	s, gw, _ := open(t)
	cases := []struct {
		name   string
		id     string
		amount int64
		cur    string
		token  string
	}{
		{"empty order", "", 100, "INR", "tok_visa"},
		{"bad order chars", "ord 1", 100, "INR", "tok_visa"},
		{"zero amount", "o", 0, "INR", "tok_visa"},
		{"negative amount", "o", -5, "INR", "tok_visa"},
		{"too large", "o", maxAmountMinor + 1, "INR", "tok_visa"},
		{"lowercase currency", "o", 100, "inr", "tok_visa"},
		{"two-letter currency", "o", 100, "IN", "tok_visa"},
		{"card number as token", "o", 100, "INR", "4242424242424242"},
		{"token without prefix", "o", 100, "INR", "visa"},
		{"token uppercase", "o", 100, "INR", "tok_VISA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.Charge(tc.id, tc.amount, tc.cur, tc.token)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
	if gw.calls.Load() != 0 {
		t.Fatal("the provider was asked about an invalid request")
	}
}

func TestCancelRefundsAnApprovedCharge(t *testing.T) {
	s, _, _ := open(t)
	approved, _, _ := s.Charge("ord_1", 5000, "INR", "tok_visa")
	c, changed, err := s.Cancel("ord_1")
	if err != nil || !changed {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	if c.Status != StatusVoided || c.VoidedAt == nil || c.Reference != approved.Reference {
		t.Fatalf("unexpected %+v", c)
	}
	// Idempotent.
	c2, changed, err := s.Cancel("ord_1")
	if err != nil || changed || c2.Status != StatusVoided {
		t.Fatalf("second cancel: %+v changed=%v err=%v", c2, changed, err)
	}
	// A voided real charge replays as voided, not as a fresh chance to pay.
	again, replayed, err := s.Charge("ord_1", 5000, "INR", "tok_visa")
	if err != nil || !replayed || again.Status != StatusVoided {
		t.Fatalf("charge after cancel: %+v replayed=%v err=%v", again, replayed, err)
	}
}

func TestCancelOfADeclinedChargeChangesNothing(t *testing.T) {
	s, _, _ := open(t)
	_, _, _ = s.Charge("ord_1", 100, "INR", "tok_declined")
	c, changed, err := s.Cancel("ord_1")
	if err != nil || changed || c.Status != StatusDeclined {
		t.Fatalf("%+v changed=%v err=%v", c, changed, err)
	}
}

func TestCancelBeforeAnyChargeLeavesATombstone(t *testing.T) {
	s, gw, _ := open(t)
	c, changed, err := s.Cancel("ord_1")
	if err != nil || !changed || c.Status != StatusVoided || c.Reference != "" {
		t.Fatalf("%+v changed=%v err=%v", c, changed, err)
	}
	// This is the point of the tombstone: a charge that was already on its
	// way when the order was cancelled must not succeed when it arrives.
	if _, _, err := s.Charge("ord_1", 5000, "INR", "tok_visa"); !errors.Is(err, ErrVoided) {
		t.Fatalf("want ErrVoided, got %v", err)
	}
	if gw.calls.Load() != 0 {
		t.Fatal("the provider was asked to charge a cancelled order")
	}
}

func TestCancelDuringAnInFlightChargeWaitsThenRefunds(t *testing.T) {
	s, gw, _ := open(t)
	gw.gate = make(chan struct{})

	chargeDone := make(chan Charge, 1)
	go func() {
		c, _, _ := s.Charge("ord_1", 5000, "INR", "tok_visa")
		chargeDone <- c
	}()
	for gw.calls.Load() == 0 { // wait until the provider call is in flight
		time.Sleep(time.Millisecond)
	}

	cancelDone := make(chan Charge, 1)
	go func() {
		c, _, _ := s.Cancel("ord_1")
		cancelDone <- c
	}()
	select {
	case <-cancelDone:
		t.Fatal("cancel returned while the charge was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(gw.gate)
	<-chargeDone
	c := <-cancelDone
	if c.Status != StatusVoided || c.Reference == "" {
		t.Fatalf("cancel should have refunded the charge that landed: %+v", c)
	}
}

func TestConcurrentDuplicateChargesProduceOneCharge(t *testing.T) {
	s, gw, _ := open(t)
	const n = 20
	refs := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := s.Charge("ord_1", 5000, "INR", "tok_visa")
			if err != nil {
				t.Error(err)
				return
			}
			refs <- c.Reference
		}()
	}
	wg.Wait()
	close(refs)
	seen := map[string]bool{}
	for r := range refs {
		seen[r] = true
	}
	if len(seen) != 1 {
		t.Fatalf("%d distinct references, want 1", len(seen))
	}
	if gw.calls.Load() != 1 {
		t.Fatalf("provider asked %d times, want 1", gw.calls.Load())
	}
}

func TestDifferentOrdersDoNotBlockEachOther(t *testing.T) {
	s, _, _ := open(t)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = s.Charge("slow_"+string(rune('a'+i)), 100, "INR", "tok_slow")
		}(i)
	}
	wg.Wait()
	// Five 10ms charges in parallel; serialised they would take 50ms+.
	if d := time.Since(start); d > 45*time.Millisecond {
		t.Fatalf("charges for different orders look serialised: %v", d)
	}
}

func TestLedgerSurvivesARestart(t *testing.T) {
	s, _, dir := open(t)
	a, _, _ := s.Charge("ord_a", 100, "INR", "tok_visa")
	_, _, _ = s.Charge("ord_b", 200, "INR", "tok_declined")
	_, _, _ = s.Cancel("ord_c")
	_, _, _ = s.Cancel("ord_a")

	gw := &countingGateway{inner: gateway.New(0)}
	s2, err := Open(dir, gw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get("ord_a")
	if err != nil || got.Status != StatusVoided || got.Reference != a.Reference {
		t.Fatalf("%+v %v", got, err)
	}
	if c, _ := s2.Get("ord_b"); c.Status != StatusDeclined {
		t.Fatalf("ord_b: %+v", c)
	}
	if _, _, err := s2.Charge("ord_c", 100, "INR", "tok_visa"); !errors.Is(err, ErrVoided) {
		t.Fatalf("the tombstone was lost across a restart: %v", err)
	}
	if gw.calls.Load() != 0 {
		t.Fatal("restart caused the provider to be asked again")
	}
}

func TestCorruptLedgerRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(`{"ord_1": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, gateway.New(0)); err == nil {
		t.Fatal("a corrupt ledger must not be silently treated as empty")
	}
}

func TestFailedPersistRollsBackMemory(t *testing.T) {
	s, _, dir := open(t)
	// Make the directory unwritable by replacing it with a file path component.
	s.path = filepath.Join(dir, "missing-subdir", "ledger.json")
	if _, _, err := s.Charge("ord_1", 100, "INR", "tok_visa"); err == nil {
		t.Fatal("expected a persist error")
	}
	if _, err := s.Get("ord_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("memory kept a charge the disk does not have: %v", err)
	}
}

func TestListIsNewestFirstAndCountsAddUp(t *testing.T) {
	s, _, _ := open(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	i := 0
	s.now = func() time.Time { i++; return t0.Add(time.Duration(i) * time.Minute) }
	_, _, _ = s.Charge("ord_1", 100, "INR", "tok_visa")
	_, _, _ = s.Charge("ord_2", 100, "INR", "tok_declined")
	_, _, _ = s.Cancel("ord_1")
	list := s.List()
	if len(list) != 2 || list[0].OrderID != "ord_2" {
		t.Fatalf("%+v", list)
	}
	c := s.Counts()
	if c[StatusVoided] != 1 || c[StatusDeclined] != 1 || c[StatusApproved] != 0 {
		t.Fatalf("%v", c)
	}
}
