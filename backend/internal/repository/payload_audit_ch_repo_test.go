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

func TestCHRepoListCursor(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	base := time.Now().UTC().Truncate(time.Millisecond)
	var events []*PayloadAuditEvent
	for i := 0; i < 25; i++ {
		events = append(events, &PayloadAuditEvent{
			ID: int64(10000 + i), CreatedAt: base.Add(-time.Duration(i) * time.Second),
			RequestID: "req",
		})
	}
	_ = repo.BatchInsertWithToken(context.Background(), events, "list-test")

	page1, cur, err := repo.List(context.Background(), PayloadAuditListFilter{
		From: base.Add(-time.Hour), To: base, Limit: 10, IncludeBody: false,
	})
	if err != nil || len(page1) != 10 || cur == nil {
		t.Fatalf("page1: len=%d cur=%v err=%v", len(page1), cur, err)
	}
	page2, _, _ := repo.List(context.Background(), PayloadAuditListFilter{
		From: base.Add(-time.Hour), To: base, Limit: 10, IncludeBody: false, Cursor: cur,
	})
	if len(page2) != 10 {
		t.Fatalf("page2 len=%d", len(page2))
	}
	if page1[9].ID == page2[0].ID {
		t.Fatal("cursor did not advance")
	}
}

func TestCHRepoListExcludesBodyByDefault(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)
	now := time.Now().UTC().Truncate(time.Millisecond)
	_ = repo.BatchInsertWithToken(context.Background(),
		[]*PayloadAuditEvent{{ID: 99, CreatedAt: now.Add(-time.Second), InputBody: "SECRET", OutputBody: "REPLY"}},
		"body-test")

	rows, _, _ := repo.List(context.Background(), PayloadAuditListFilter{
		From: now.Add(-time.Minute), To: now, Limit: 10,
	})
	if len(rows) != 1 || rows[0].InputBody != "" || rows[0].OutputBody != "" {
		t.Fatalf("body should be empty by default, got len=%d input=%q output=%q", len(rows), rows[0].InputBody, rows[0].OutputBody)
	}
}

func TestCHRepoListIncludeBody(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)
	now := time.Now().UTC().Truncate(time.Millisecond)
	_ = repo.BatchInsertWithToken(context.Background(),
		[]*PayloadAuditEvent{{ID: 99, CreatedAt: now.Add(-time.Second), InputBody: "SECRET", OutputBody: "REPLY"}},
		"body-test")

	rows, _, _ := repo.List(context.Background(), PayloadAuditListFilter{
		From: now.Add(-time.Minute), To: now, Limit: 10, IncludeBody: true,
	})
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].InputBody != "SECRET" || rows[0].OutputBody != "REPLY" {
		t.Fatal("body should be present when IncludeBody=true")
	}
}

func TestCHRepoListUserFilter(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)
	now := time.Now().UTC().Truncate(time.Millisecond)
	u1, u2 := int64(1), int64(2)
	_ = repo.BatchInsertWithToken(context.Background(), []*PayloadAuditEvent{
		{ID: 100, CreatedAt: now.Add(-time.Second), UserID: &u1},
		{ID: 101, CreatedAt: now.Add(-time.Second), UserID: &u2},
	}, "uf")

	rows, _, _ := repo.List(context.Background(), PayloadAuditListFilter{
		From: now.Add(-time.Minute), To: now, Limit: 10, UserID: &u1,
	})
	if len(rows) != 1 || *rows[0].UserID != 1 {
		t.Fatalf("expected only user 1, got %v", rows)
	}
}

func TestCHRepoListKeyword(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)
	now := time.Now().UTC().Truncate(time.Millisecond)
	_ = repo.BatchInsertWithToken(context.Background(), []*PayloadAuditEvent{
		{ID: 200, CreatedAt: now.Add(-time.Second), InputExcerpt: "hello WORLD"},
		{ID: 201, CreatedAt: now.Add(-time.Second), OutputExcerpt: "no match"},
	}, "kw")

	rows, _, _ := repo.List(context.Background(), PayloadAuditListFilter{
		From: now.Add(-time.Minute), To: now, Limit: 10, KeywordILike: "world",
	})
	if len(rows) != 1 || rows[0].ID != 200 {
		t.Fatalf("keyword match failed: %v", rows)
	}
}

func TestCHRepoGet(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)
	now := time.Now().UTC().Truncate(time.Millisecond)
	_ = repo.BatchInsertWithToken(context.Background(),
		[]*PayloadAuditEvent{{ID: 555, CreatedAt: now, RequestID: "r5", InputBody: "FULL"}}, "g")

	row, err := repo.Get(context.Background(), 555, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.InputBody != "FULL" {
		t.Fatalf("body missing: %q", row.InputBody)
	}
}

func TestCHRepoGetRequiresCreatedAt(t *testing.T) {
	conn := testCHConn(t)
	repo := NewPayloadAuditCHRepoWithTable(conn, "irrelevant")
	if _, err := repo.Get(context.Background(), 1, time.Time{}); err == nil {
		t.Fatal("want error when createdAt is zero")
	}
}

func TestCHRepoAlterTTL(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)
	if err := repo.AlterTTL(context.Background(), 90); err != nil {
		t.Fatal(err)
	}
	var ef string
	_ = conn.QueryRow(context.Background(),
		"SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = ?", table).Scan(&ef)
	// ClickHouse may report TTL as "INTERVAL 90 DAY" or "toIntervalDay(90)".
	if !contains(ef, "90") {
		t.Fatalf("want TTL with 90 days, got: %s", ef)
	}
}

func TestCHRepoDropExpiredPartitions(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	// Use a very long TTL so the old row is not auto-expired by MergeTree TTL.
	_ = EnsureSchema(context.Background(), conn, table, 3650)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	old := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	_ = repo.BatchInsertWithToken(context.Background(), []*PayloadAuditEvent{
		{ID: 1, CreatedAt: old},
		{ID: 2, CreatedAt: now},
	}, "drop-test")

	dropped, err := repo.DropExpiredMonthlyPartitions(context.Background(), now.AddDate(0, -1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) == 0 || dropped[0] != "202401" {
		t.Fatalf("expected partition 202401 dropped, got %v", dropped)
	}
	var count uint64
	_ = conn.QueryRow(context.Background(), "SELECT count() FROM "+table+" WHERE id = 1").Scan(&count)
	if count != 0 {
		t.Fatalf("old row not dropped")
	}
}
