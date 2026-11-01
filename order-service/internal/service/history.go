package service

import (
	"context"

	"gocart/order-service/internal/domain"
)

// History returns an order's raw events, oldest first: the facts the current
// state was derived from. It reads the event store, not the read model.
func (s *Service) History(ctx context.Context, id string) ([]domain.Event, error) {
	events, err := s.store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, ErrNotFound
	}
	return events, nil
}
