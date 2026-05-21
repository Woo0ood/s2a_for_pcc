//go:build integration

package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

func testCHConn(t *testing.T) clickhouse.Conn {
	t.Helper()
	dsn := os.Getenv("TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("TEST_CLICKHOUSE_DSN not set")
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return conn
}

func tempTableName(t *testing.T) string {
	t.Helper()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("payload_audit_logs_test_%d_%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func TestEnsureSchemaCreates(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() {
		_ = conn.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	})

	if err := EnsureSchema(context.Background(), conn, table, 90); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := conn.QueryRow(context.Background(),
		"SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = ?", table,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !chContains(got, "INTERVAL 90 DAY") && !chContains(got, "toIntervalDay(90)") {
		t.Fatalf("ttl not 90 days: %s", got)
	}
}

func TestEnsureSchemaAltersTTL(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", table)) })

	_ = EnsureSchema(context.Background(), conn, table, 30)
	if err := EnsureSchema(context.Background(), conn, table, 120); err != nil {
		t.Fatal(err)
	}
	var got string
	_ = conn.QueryRow(context.Background(),
		"SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = ?", table,
	).Scan(&got)
	if !chContains(got, "INTERVAL 120 DAY") && !chContains(got, "toIntervalDay(120)") {
		t.Fatalf("ttl not altered to 120: %s", got)
	}
}

func chContains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (len(needle) == 0 || chIndexOf(haystack, needle) >= 0)
}
func chIndexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
