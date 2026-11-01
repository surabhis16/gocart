//go:build !cassandra

package main

import (
	"errors"
	"log/slog"

	"gocart/order-service/internal/eventstore"
)

// openCassandra is the stub used by the default build, which has no Cassandra
// driver in it. Building with `-tags cassandra` swaps in the real one.
func openCassandra(*slog.Logger) (eventstore.Store, error) {
	return nil, errors.New("EVENT_STORE=cassandra needs a binary built with -tags cassandra " +
		"(docker build --build-arg TAGS=cassandra ...); this one was not")
}
