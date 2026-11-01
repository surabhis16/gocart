package eventstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gocart/order-service/internal/domain"
)

func ev(stream string, version int) domain.Event {
	return domain.Event{
		StreamID: stream, Version: version, Type: domain.TypeOrderShipped,
		Data: []byte(`{}`), At: time.Date(2026, 10, 30, 9, 0, version, 0, time.UTC),
	}
}

// runContract is the behaviour every Store implementation must provide. The
// file store runs it on every test run; the Cassandra store runs the very same
// checks when a Cassandra is available, which is how "same contract" is proven
// rather than claimed.
func runContract(t *testing.T, open func(t *testing.T) Store) {
	ctx := context.Background()

	t.Run("append then load keeps order", func(t *testing.T) {
		s := open(t)
		for v := 1; v <= 3; v++ {
			if err := s.Append(ctx, ev("ord_a", v)); err != nil {
				t.Fatalf("append v%d: %v", v, err)
			}
		}
		got, err := s.Load(ctx, "ord_a")
		if err != nil || len(got) != 3 {
			t.Fatalf("Load: %d events, err %v", len(got), err)
		}
		for i, e := range got {
			if e.Version != i+1 || e.StreamID != "ord_a" || e.Type != domain.TypeOrderShipped || string(e.Data) != "{}" {
				t.Errorf("event %d round-tripped wrong: %+v", i, e)
			}
		}
	})

	t.Run("unknown stream is empty not an error", func(t *testing.T) {
		got, err := open(t).Load(ctx, "ord_missing")
		if err != nil || len(got) != 0 {
			t.Errorf("got %d events, err %v", len(got), err)
		}
	})

	t.Run("version must follow the stream", func(t *testing.T) {
		s := open(t)
		if err := s.Append(ctx, ev("ord_a", 2)); !errors.Is(err, ErrVersionConflict) {
			t.Errorf("gap at start: err = %v, want ErrVersionConflict", err)
		}
		if err := s.Append(ctx, ev("ord_a", 1)); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, ev("ord_a", 1)); !errors.Is(err, ErrVersionConflict) {
			t.Errorf("duplicate version: err = %v, want ErrVersionConflict", err)
		}
		if err := s.Append(ctx, ev("ord_a", 3)); !errors.Is(err, ErrVersionConflict) {
			t.Errorf("skipped version: err = %v, want ErrVersionConflict", err)
		}
		if got, _ := s.Load(ctx, "ord_a"); len(got) != 1 {
			t.Errorf("rejected appends changed the stream: %d events", len(got))
		}
	})

	t.Run("exactly one of many racing writers wins", func(t *testing.T) {
		s := open(t)
		var wins, conflicts atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch err := s.Append(ctx, ev("ord_race", 1)); {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, ErrVersionConflict):
					conflicts.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 || conflicts.Load() != 15 {
			t.Errorf("wins=%d conflicts=%d, want 1 and 15", wins.Load(), conflicts.Load())
		}
	})

	t.Run("load all returns every stream, each in order", func(t *testing.T) {
		s := open(t)
		for _, id := range []string{"ord_x", "ord_y", "ord_z"} {
			for v := 1; v <= 2; v++ {
				if err := s.Append(ctx, ev(id, v)); err != nil {
					t.Fatal(err)
				}
			}
		}
		all, err := s.LoadAll(ctx)
		if err != nil || len(all) != 6 {
			t.Fatalf("LoadAll: %d events, err %v", len(all), err)
		}
		last := map[string]int{}
		for _, e := range all {
			if e.Version != last[e.StreamID]+1 {
				t.Errorf("stream %s out of order at %s", e.StreamID, fmt.Sprint(e.Version))
			}
			last[e.StreamID] = e.Version
		}
	})

	t.Run("cancelled context is respected", func(t *testing.T) {
		s := open(t)
		c, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.Append(c, ev("ord_a", 1)); err == nil {
			t.Error("Append ignored a cancelled context")
		}
		if got, _ := s.Load(ctx, "ord_a"); len(got) != 0 {
			t.Error("cancelled Append still wrote")
		}
	})
}
