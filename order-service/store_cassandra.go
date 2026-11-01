//go:build cassandra

package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gocart/order-service/internal/eventstore"
)

// openCassandra builds the Cassandra-backed event store from the environment.
// Only compiled with -tags cassandra.
func openCassandra(log *slog.Logger) (eventstore.Store, error) {
	var hosts []string
	for _, h := range strings.Split(env("CASSANDRA_HOSTS", ""), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil, errors.New("EVENT_STORE=cassandra needs CASSANDRA_HOSTS, for example cassandra or 10.0.0.5,10.0.0.6")
	}
	port, err := strconv.Atoi(env("CASSANDRA_PORT", "9042"))
	if err != nil {
		return nil, errors.New("CASSANDRA_PORT must be a number")
	}
	rf, err := strconv.Atoi(env("CASSANDRA_REPLICATION_FACTOR", "1"))
	if err != nil || rf < 1 {
		return nil, errors.New("CASSANDRA_REPLICATION_FACTOR must be a whole number of at least 1")
	}
	timeout, err := time.ParseDuration(env("CASSANDRA_CONNECT_TIMEOUT", "3m"))
	if err != nil {
		return nil, errors.New("CASSANDRA_CONNECT_TIMEOUT must be a duration such as 3m")
	}
	initSchema, _ := strconv.ParseBool(env("CASSANDRA_INIT_SCHEMA", "true"))

	cfg := eventstore.CassandraConfig{
		Hosts: hosts, Port: port, Keyspace: env("CASSANDRA_KEYSPACE", "gocart"),
		ReplicationFactor: rf, InitSchema: initSchema, ConnectTimeout: timeout,
	}
	log.Info("connecting to cassandra", "hosts", hosts, "keyspace", cfg.Keyspace, "init_schema", initSchema)
	return eventstore.OpenCassandra(context.Background(), cfg)
}
