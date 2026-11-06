//go:build cassandra

package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"gocart/order-service/internal/domain"
)

// CassandraConfig says where the event log lives.
type CassandraConfig struct {
	Hosts             []string      // contact points: hostnames or IPs, without ports
	Port              int           // 0 means the driver default, 9042
	Keyspace          string        // created if InitSchema is set
	ReplicationFactor int           // used only when creating the keyspace
	InitSchema        bool          // CREATE KEYSPACE/TABLE IF NOT EXISTS on startup
	ConnectTimeout    time.Duration // how long to keep retrying while Cassandra boots
}

var keyspaceName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,47}$`)

// Cassandra is the event log on Apache Cassandra.
//
// One partition per order (stream_id), one row per event (clustering column
// version). The version being part of the primary key is what makes
// optimistic concurrency work: `INSERT ... IF NOT EXISTS` is a lightweight
// transaction, so of any number of writers racing to append the same version
// of the same order, exactly one is applied and the rest are told so.
//
// Append is not a single atomic step across "previous version exists" and
// "insert". It does not need to be: events are never deleted, so once version
// n-1 exists it always will, and the insert's own uniqueness check settles
// every race.
type Cassandra struct {
	s *gocql.Session
}

// OpenCassandra connects, retrying until cfg.ConnectTimeout because Cassandra
// takes a minute or more to start and the Order service usually starts first.
func OpenCassandra(ctx context.Context, cfg CassandraConfig) (*Cassandra, error) {
	if len(cfg.Hosts) == 0 {
		return nil, errors.New("cassandra: no hosts configured")
	}
	if !keyspaceName.MatchString(cfg.Keyspace) {
		return nil, fmt.Errorf("cassandra: keyspace %q is not a plain identifier", cfg.Keyspace)
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 2 * time.Minute
	}
	if cfg.ReplicationFactor < 1 {
		cfg.ReplicationFactor = 1
	}

	if cfg.InitSchema {
		boot, err := dialWithRetry(ctx, cfg, "")
		if err != nil {
			return nil, err
		}
		stmts := []string{
			fmt.Sprintf(`CREATE KEYSPACE IF NOT EXISTS %s WITH replication = {'class': 'SimpleStrategy', 'replication_factor': %d}`,
				cfg.Keyspace, cfg.ReplicationFactor),
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.order_events (
				stream_id text,
				version   int,
				type      text,
				data      text,
				at        timestamp,
				PRIMARY KEY (stream_id, version)
			) WITH CLUSTERING ORDER BY (version ASC)`, cfg.Keyspace),
			// A change feed: one row per append, bucketed by hour so no
			// partition grows without bound, ordered by time within the bucket.
			// Rows expire after a day; a replica that has been away longer than
			// that does a full reload instead of reading the feed.
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.order_changes (
				bucket    text,
				id        timeuuid,
				stream_id text,
				PRIMARY KEY (bucket, id)
			) WITH CLUSTERING ORDER BY (id ASC) AND default_time_to_live = 86400`, cfg.Keyspace),
		}
		for _, stmt := range stmts {
			if err := boot.Query(stmt).WithContext(ctx).Exec(); err != nil {
				boot.Close()
				return nil, fmt.Errorf("cassandra: create schema: %w", err)
			}
		}
		boot.Close()
	}

	s, err := dialWithRetry(ctx, cfg, cfg.Keyspace)
	if err != nil {
		return nil, err
	}
	return &Cassandra{s: s}, nil
}

func dialWithRetry(ctx context.Context, cfg CassandraConfig, keyspace string) (*gocql.Session, error) {
	deadline := time.Now().Add(cfg.ConnectTimeout)
	for {
		cluster := gocql.NewCluster(cfg.Hosts...)
		if cfg.Port != 0 {
			cluster.Port = cfg.Port
		}
		cluster.Keyspace = keyspace
		cluster.Consistency = gocql.Quorum
		cluster.SerialConsistency = gocql.Serial
		cluster.Timeout = 5 * time.Second
		cluster.ConnectTimeout = 5 * time.Second

		s, err := cluster.CreateSession()
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, fmt.Errorf("cassandra: could not connect to %v within %s: %w", cfg.Hosts, cfg.ConnectTimeout, err)
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Append implements Store.
func (c *Cassandra) Append(ctx context.Context, e domain.Event) error {
	if e.StreamID == "" || e.Version < 1 {
		return errors.New("eventstore: event needs a stream id and a version of at least 1")
	}
	if e.Version > 1 {
		var prev int
		err := c.s.Query(`SELECT version FROM order_events WHERE stream_id = ? AND version = ?`,
			e.StreamID, e.Version-1).WithContext(ctx).Scan(&prev)
		if errors.Is(err, gocql.ErrNotFound) {
			return fmt.Errorf("%w: stream %s has no version %d, tried to write version %d",
				ErrVersionConflict, e.StreamID, e.Version-1, e.Version)
		}
		if err != nil {
			return fmt.Errorf("cassandra: check previous version: %w", err)
		}
	}
	existing := map[string]interface{}{}
	applied, err := c.s.Query(
		`INSERT INTO order_events (stream_id, version, type, data, at) VALUES (?, ?, ?, ?, ?) IF NOT EXISTS`,
		e.StreamID, e.Version, e.Type, string(e.Data), e.At,
	).WithContext(ctx).MapScanCAS(existing)
	if err != nil {
		return fmt.Errorf("cassandra: append event: %w", err)
	}
	if !applied {
		return fmt.Errorf("%w: stream %s already has version %d", ErrVersionConflict, e.StreamID, e.Version)
	}
	// Record the change for other replicas. The event is already durable, so a
	// failure here must not fail the append; the worst case is that another
	// replica hears about this event from its next full reload instead of from
	// the feed, a few minutes later. now() is the Cassandra node's clock.
	_ = c.s.Query(`INSERT INTO order_changes (bucket, id, stream_id) VALUES (?, now(), ?)`,
		feedBucket(time.Now()), e.StreamID).WithContext(ctx).Exec()
	return nil
}

// feedBucket names the hour a change belongs to.
func feedBucket(t time.Time) string { return t.UTC().Format("2006-01-02T15") }

// ChangedSince implements Store. It reads every hourly bucket from since to
// now, newest rows only (minTimeuuid turns a timestamp into the smallest
// timeuuid at that instant). Streams can appear more than once.
func (c *Cassandra) ChangedSince(ctx context.Context, since time.Time) ([]string, error) {
	now := time.Now().UTC()
	since = since.UTC()
	if since.After(now) {
		return nil, nil
	}
	var out []string
	for b := since.Truncate(time.Hour); !b.After(now); b = b.Add(time.Hour) {
		it := c.s.Query(`SELECT stream_id FROM order_changes WHERE bucket = ? AND id >= minTimeuuid(?)`,
			feedBucket(b), since).WithContext(ctx).Iter()
		var id string
		for it.Scan(&id) {
			out = append(out, id)
		}
		if err := it.Close(); err != nil {
			return nil, fmt.Errorf("cassandra: read change feed: %w", err)
		}
	}
	return out, nil
}

// Load implements Store.
func (c *Cassandra) Load(ctx context.Context, streamID string) ([]domain.Event, error) {
	it := c.s.Query(`SELECT version, type, data, at FROM order_events WHERE stream_id = ?`, streamID).
		WithContext(ctx).Iter()
	var (
		out  []domain.Event
		ver  int
		typ  string
		data string
		at   time.Time
	)
	for it.Scan(&ver, &typ, &data, &at) {
		out = append(out, domain.Event{
			StreamID: streamID, Version: ver, Type: typ, Data: json.RawMessage(data), At: at.UTC(),
		})
	}
	if err := it.Close(); err != nil {
		return nil, fmt.Errorf("cassandra: load stream %s: %w", streamID, err)
	}
	return out, nil
}

// LoadAll implements Store. It is a full table scan, which is right for the
// once-at-startup rebuild of the read model and wrong for anything on a hot
// path. Rows come back grouped by partition and ordered by version within it,
// so each order's events are in order.
func (c *Cassandra) LoadAll(ctx context.Context) ([]domain.Event, error) {
	it := c.s.Query(`SELECT stream_id, version, type, data, at FROM order_events`).WithContext(ctx).Iter()
	var (
		out    []domain.Event
		stream string
		ver    int
		typ    string
		data   string
		at     time.Time
	)
	for it.Scan(&stream, &ver, &typ, &data, &at) {
		out = append(out, domain.Event{
			StreamID: stream, Version: ver, Type: typ, Data: json.RawMessage(data), At: at.UTC(),
		})
	}
	if err := it.Close(); err != nil {
		return nil, fmt.Errorf("cassandra: load all events: %w", err)
	}
	return out, nil
}

// Close implements Store.
func (c *Cassandra) Close() error {
	c.s.Close()
	return nil
}
