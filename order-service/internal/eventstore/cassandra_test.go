//go:build cassandra

package eventstore

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCassandraStoreMeetsTheContract runs the exact checks the file store
// passes (contract_test.go) against a real Cassandra. It needs one:
//
//	docker run -d --name cass -p 9042:9042 cassandra:5
//	CASSANDRA_TEST_HOSTS=127.0.0.1 go test -tags cassandra ./internal/eventstore
//
// It uses its own keyspace, gocart_test, and empties it between checks.
func TestCassandraStoreMeetsTheContract(t *testing.T) {
	hosts := os.Getenv("CASSANDRA_TEST_HOSTS")
	if hosts == "" {
		t.Skip("set CASSANDRA_TEST_HOSTS (for example 127.0.0.1) to run this against a real Cassandra")
	}
	port, _ := strconv.Atoi(os.Getenv("CASSANDRA_TEST_PORT"))

	runContract(t, func(t *testing.T) Store {
		s, err := OpenCassandra(context.Background(), CassandraConfig{
			Hosts: strings.Split(hosts, ","), Port: port, Keyspace: "gocart_test",
			ReplicationFactor: 1, InitSchema: true, ConnectTimeout: 2 * time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, tbl := range []string{"order_events", "order_changes"} {
    		if err := s.s.Query(`TRUNCATE ` + tbl).Exec(); err != nil {
        		t.Fatalf("truncate %s: %v", tbl, err)
    		}
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
