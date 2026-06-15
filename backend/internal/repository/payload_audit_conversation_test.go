//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

func TestListConversation(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })

	if err := EnsureSchema(context.Background(), conn, table, 30); err != nil {
		t.Fatal(err)
	}
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	base := time.Now().UTC().Truncate(time.Millisecond)
	convKey := "conv-abc-123"
	otherKey := "conv-other-999"

	events := []*PayloadAuditEvent{
		{
			ID: 7001, CreatedAt: base.Add(-2 * time.Second),
			RequestID: "req-7001", ConversationKey: convKey,
			InputBody: "turn-1-input", OutputBody: "turn-1-output",
		},
		{
			ID: 7002, CreatedAt: base.Add(-1 * time.Second),
			RequestID: "req-7002", ConversationKey: convKey,
			InputBody: "turn-2-input", OutputBody: "turn-2-output",
		},
		{
			ID: 7003, CreatedAt: base,
			RequestID: "req-7003", ConversationKey: convKey,
			InputBody: "turn-3-input", OutputBody: "turn-3-output",
		},
		// Different conversation key — must NOT appear in results.
		{
			ID: 7004, CreatedAt: base,
			RequestID: "req-7004", ConversationKey: otherKey,
			InputBody: "other-input", OutputBody: "other-output",
		},
	}
	if err := repo.BatchInsertWithToken(context.Background(), events, "list-conv-test"); err != nil {
		t.Fatal(err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	rows, err := repo.ListConversation(context.Background(), convKey, from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows for convKey, got %d", len(rows))
	}

	// Verify ascending order by created_at.
	for i := 1; i < len(rows); i++ {
		if rows[i].CreatedAt.Before(rows[i-1].CreatedAt) {
			t.Fatalf("rows not ascending: rows[%d].CreatedAt=%v < rows[%d].CreatedAt=%v",
				i, rows[i].CreatedAt, i-1, rows[i-1].CreatedAt)
		}
	}

	// Verify bodies are present (full scan).
	for _, r := range rows {
		if r.InputBody == "" || r.OutputBody == "" {
			t.Fatalf("expected bodies to be populated, got input=%q output=%q", r.InputBody, r.OutputBody)
		}
	}

	// Verify IDs in order.
	wantIDs := []int64{7001, 7002, 7003}
	for i, r := range rows {
		if r.ID != wantIDs[i] {
			t.Fatalf("rows[%d].ID = %d, want %d", i, r.ID, wantIDs[i])
		}
	}
}

func TestListConversationEmptyKey(t *testing.T) {
	conn := testCHConn(t)
	repo := NewPayloadAuditCHRepoWithTable(conn, "irrelevant")
	rows, err := repo.ListConversation(context.Background(), "", time.Now().Add(-time.Hour), time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if rows != nil {
		t.Fatalf("expected nil for empty convKey, got %v", rows)
	}
}

func TestListConversationLimitDefault(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	base := time.Now().UTC().Truncate(time.Millisecond)
	convKey := "conv-limit-test"

	// Insert 3 rows; limit=99999 (> 2000) should default to 500 and still return all 3.
	events := []*PayloadAuditEvent{
		{ID: 8001, CreatedAt: base.Add(-2 * time.Second), ConversationKey: convKey},
		{ID: 8002, CreatedAt: base.Add(-1 * time.Second), ConversationKey: convKey},
		{ID: 8003, CreatedAt: base, ConversationKey: convKey},
	}
	_ = repo.BatchInsertWithToken(context.Background(), events, "list-conv-limit")

	rows, err := repo.ListConversation(context.Background(), convKey,
		base.Add(-time.Hour), base.Add(time.Hour), 99999)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
}
