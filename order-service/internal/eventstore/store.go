// Package eventstore persists order events. It is the only place in the Order
// service that knows how events are stored, behind a three-method interface:
// swap the implementation and nothing else changes.
//
// Two implementations exist:
//
//   - File (this package, default): one append-only JSON-lines file. Free, no
//     server to run, and enough to make the event-sourcing story real.
//   - Cassandra (cassandra.go, build tag "cassandra"): the same contract on a
//     replicated database, selected with EVENT_STORE=cassandra.
package eventstore

import (
	"context"
	"errors"

	"gocart/order-service/internal/domain"
)

// ErrVersionConflict means another writer appended to the stream first. The
// caller should reload the stream, re-decide and try again.
var ErrVersionConflict = errors.New("eventstore: version conflict")

// Store is an append-only log of events, grouped into streams (one per order).
type Store interface {
	// Append adds e to its stream. e.Version must be exactly one more than the
	// stream's current length, otherwise it returns ErrVersionConflict. This
	// is optimistic concurrency: no locks are held between load and append.
	Append(ctx context.Context, e domain.Event) error

	// Load returns one stream in version order. An unknown stream is empty,
	// not an error.
	Load(ctx context.Context, streamID string) ([]domain.Event, error)

	// LoadAll returns every event. Events of one stream are in version order;
	// no order is promised between streams, because nothing here depends on one.
	LoadAll(ctx context.Context) ([]domain.Event, error)

	Close() error
}
