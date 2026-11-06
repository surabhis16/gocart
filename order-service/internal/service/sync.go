package service

import (
	"context"
	"fmt"
	"time"
)

// Several replicas of the Order service can share one event store (Cassandra).
// Commands are safe regardless, because every command replays the order's
// events from the store and appends with optimistic concurrency. What each
// replica holds privately is its read model, and a replica only updates that
// from the commands it handled itself, so without help it would never learn of
// orders placed through another replica.
//
// Sync is that help. It asks the store which streams changed recently, reloads
// those, and applies what the view is missing. It is the event feed, polled.
// Applying an event the view already has is a no-op, so repeating or
// overlapping work is harmless, and that is what lets the design be this
// simple.

// syncOverlap makes each poll start a little before the last one ended, to
// absorb clock differences between this replica and whatever stamped the feed.
const syncOverlap = 10 * time.Second

// WithResync sets how often Sync also reloads the whole log. The change feed
// is a hint that can miss an event if a writer died at the wrong moment; the
// full reload is the backstop that bounds how long a miss can last.
func WithResync(d time.Duration) Option { return func(s *Service) { s.resyncEvery = d } }

// Sync brings this replica's read model up to date with the shared store.
func (s *Service) Sync(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	start := time.Now()
	if s.resyncEvery > 0 && start.Sub(s.lastFull) >= s.resyncEvery {
		return s.fullResync(ctx, start)
	}
	ids, err := s.store.ChangedSince(ctx, s.lastSync.Add(-syncOverlap))
	if err != nil {
		return fmt.Errorf("read change feed: %w", err)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		s.commitMu.Lock()
		err := s.catchUpLocked(ctx, id)
		s.commitMu.Unlock()
		if err != nil {
			return err // lastSync is not advanced, so the next poll tries again
		}
	}
	s.lastSync = start
	return nil
}

func (s *Service) fullResync(ctx context.Context, start time.Time) error {
	events, err := s.store.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("full resync: %w", err)
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	for _, e := range events {
		if err := s.proj.Apply(e); err != nil {
			return fmt.Errorf("full resync: apply %s v%d: %w", e.StreamID, e.Version, err)
		}
	}
	s.lastSync, s.lastFull = start, start
	s.log.Debug("full resync of the read model", "events", len(events), "orders", s.proj.Len())
	return nil
}

// catchUpLocked applies whatever events of one stream the view lacks. The
// caller holds commitMu, so no command can apply a later event of the same
// stream in the middle.
func (s *Service) catchUpLocked(ctx context.Context, id string) error {
	events, err := s.store.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("load %s: %w", id, err)
	}
	for _, e := range events {
		if err := s.proj.Apply(e); err != nil {
			return fmt.Errorf("apply %s v%d: %w", id, e.Version, err)
		}
	}
	return nil
}

// RunSync calls Sync every interval until ctx is cancelled.
func (s *Service) RunSync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("read model sync failed; will retry", "err", err)
			}
		}
	}
}
