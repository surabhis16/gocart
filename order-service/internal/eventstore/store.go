// Package eventstore persists order events. It is the only place in the Order
// service that knows how events are stored, behind a three-method interface:
// swap the implementation and nothing else changes.
//
// Besides appending and loading, a store can say which streams changed
// recently (ChangedSince), which is how several replicas of the Order service
// keep their read models current from one shared log.
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
	"time"

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

	// ChangedSince returns the ids of streams that have had an event appended
	// at or after since. It lets one replica of the Order service learn what
	// another replica wrote, without reading the whole log.
	//
	// It is a hint, not a guarantee: it may name streams that have not changed
	// (the caller just reloads them, which is harmless) and, if a writer died
	// between appending and recording the change, it can miss one. The caller
	// is expected to run a full LoadAll now and then as the backstop, and to
	// pass a since that is earlier than it strictly needs, to absorb clock skew.
	ChangedSince(ctx context.Context, since time.Time) ([]string, error)

	Close() error
}
