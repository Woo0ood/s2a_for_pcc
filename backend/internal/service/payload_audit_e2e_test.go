//go:build integration

package service_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestPayloadAuditE2E_SinkToClickHouse(t *testing.T) {
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

	table := fmt.Sprintf("payload_audit_logs_e2e_%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	if err := repository.EnsureSchema(context.Background(), conn, table, 30); err != nil {
		t.Fatal(err)
	}
	repo := repository.NewPayloadAuditCHRepoWithTable(conn, table)
	adapter := repository.NewPayloadAuditSinkAdapter(repo)

	sink := service.NewPayloadAuditSink(adapter, service.SinkConfig{
		WorkerCount:  1,
		BatchSize:    10,
		BatchFlushMs: 200,
		TokenFn: func(events []*service.PayloadAuditEvent) string {
			if len(events) == 0 {
				return ""
			}
			return fmt.Sprintf("e2e-%d", events[0].ID)
		},
	})
	ctx := context.Background()
	sink.Start(ctx)

	// enqueue 5 events
	for i := 0; i < 5; i++ {
		ok := sink.TryEnqueue(&service.PayloadAuditEvent{
			ID:         int64(time.Now().UnixNano()) + int64(i),
			CreatedAt:  time.Now().UTC(),
			RequestID:  fmt.Sprintf("e2e-req-%d", i),
			Endpoint:   "/v1/chat",
			Provider:   "openai",
			Model:      "gpt-4",
			Stream:     true,
			StatusCode: 200,
			DurationMs: 100 + i,
			InputBody:  "hello",
			OutputBody: "world",
		})
		if !ok {
			t.Fatalf("enqueue %d failed", i)
		}
	}

	// wait for flush
	time.Sleep(800 * time.Millisecond)
	rem := sink.Stop(ctx, 5*time.Second)
	if len(rem) != 0 {
		t.Fatalf("undrained: %d", len(rem))
	}

	var count uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("count=%d want 5", count)
	}
	t.Logf("e2e ok: %d rows in %s", count, table)
}
