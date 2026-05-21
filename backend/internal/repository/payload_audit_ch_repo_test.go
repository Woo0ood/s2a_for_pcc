//go:build integration

package repository

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestCHRepoBatchInsert(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })

	if err := EnsureSchema(context.Background(), conn, table, 30); err != nil {
		t.Fatal(err)
	}
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	now := time.Now().UTC().Truncate(time.Millisecond)
	user := int64(42)
	events := []*PayloadAuditEvent{
		{
			ID: 1001, CreatedAt: now, RequestID: "req-1",
			UserID: &user, UserEmail: "u@example.com",
			Endpoint: "/v1/chat", Provider: "openai", Model: "gpt-4",
			Stream: true, StatusCode: 200, DurationMs: 350,
			InputBody: "hello", OutputBody: "world",
			InputFormat: "json", OutputFormat: "text",
			InputBytes: 5, OutputBytes: 5,
			ClientIP: "127.0.0.1",
		},
	}
	if err := repo.BatchInsertWithToken(context.Background(), events, "test-token-1"); err != nil {
		t.Fatal(err)
	}
	var count uint64
	_ = conn.QueryRow(context.Background(), "SELECT count() FROM "+table).Scan(&count)
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestCHRepoBatchInsertDedup(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	events := []*PayloadAuditEvent{{ID: 2001, CreatedAt: time.Now().UTC()}}
	_ = repo.BatchInsertWithToken(context.Background(), events, "dedup-tok")
	_ = repo.BatchInsertWithToken(context.Background(), events, "dedup-tok")

	var count uint64
	_ = conn.QueryRow(context.Background(), "SELECT count() FROM "+table).Scan(&count)
	if count != 1 {
		t.Fatalf("dedup failed, count = %d", count)
	}
}

func TestCHRepoIPv6ConversionEmpty(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	_ = repo.BatchInsertWithToken(context.Background(),
		[]*PayloadAuditEvent{{ID: 3001, CreatedAt: time.Now().UTC(), ClientIP: ""}},
		"ip-empty")

	var ip net.IP
	_ = conn.QueryRow(context.Background(), "SELECT client_ip FROM "+table+" WHERE id = 3001").Scan(&ip)
	if !ip.Equal(net.IPv6zero) {
		t.Fatalf("want ::, got %v", ip)
	}
}
