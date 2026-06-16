//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// StreamConversation
// ─────────────────────────────────────────────────────────────────────────────

func TestStreamConversation(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })

	if err := EnsureSchema(context.Background(), conn, table, 30); err != nil {
		t.Fatal(err)
	}
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	base := time.Now().UTC().Truncate(time.Millisecond)
	convKey := "stream-conv-abc"
	otherKey := "stream-conv-other"

	events := []*PayloadAuditEvent{
		{
			ID: 11001, CreatedAt: base.Add(-2 * time.Second),
			RequestID: "req-s1001", ConversationKey: convKey,
			ResponseID: "resp-s1001",
			InputBody:  "stream-turn-1-input", OutputBody: "stream-turn-1-output",
		},
		{
			ID: 11002, CreatedAt: base.Add(-1 * time.Second),
			RequestID: "req-s1002", ConversationKey: convKey,
			ResponseID: "resp-s1002", PreviousResponseID: "resp-s1001",
			InputBody: "stream-turn-2-input", OutputBody: "stream-turn-2-output",
		},
		{
			ID: 11003, CreatedAt: base,
			RequestID: "req-s1003", ConversationKey: convKey,
			ResponseID: "resp-s1003", PreviousResponseID: "resp-s1002",
			InputBody: "stream-turn-3-input", OutputBody: "stream-turn-3-output",
		},
		// Different conversation key — must NOT appear in results.
		{
			ID: 11004, CreatedAt: base,
			RequestID: "req-s1004", ConversationKey: otherKey,
			InputBody: "other-input", OutputBody: "other-output",
		},
	}
	if err := repo.BatchInsertWithToken(context.Background(), events, "stream-conv-test"); err != nil {
		t.Fatal(err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	var collected []*PayloadAuditRow
	err := repo.StreamConversation(context.Background(), convKey, from, to, 0, func(row *PayloadAuditRow) error {
		collected = append(collected, row)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(collected) != 3 {
		t.Fatalf("expected 3 rows for convKey, got %d", len(collected))
	}

	// Verify ascending order.
	for i := 1; i < len(collected); i++ {
		if collected[i].CreatedAt.Before(collected[i-1].CreatedAt) {
			t.Fatalf("rows not ascending at index %d", i)
		}
	}

	// Verify IDs in order.
	wantIDs := []int64{11001, 11002, 11003}
	for i, r := range collected {
		if r.ID != wantIDs[i] {
			t.Fatalf("collected[%d].ID = %d, want %d", i, r.ID, wantIDs[i])
		}
	}

	// Verify bodies populated (full columns).
	for _, r := range collected {
		if r.InputBody == "" || r.OutputBody == "" {
			t.Fatalf("expected bodies populated, got input=%q output=%q", r.InputBody, r.OutputBody)
		}
	}
}

func TestStreamConversation_EmptyKey(t *testing.T) {
	conn := testCHConn(t)
	repo := NewPayloadAuditCHRepoWithTable(conn, "irrelevant")
	err := repo.StreamConversation(context.Background(), "", time.Now().Add(-time.Hour), time.Now(), 0, func(*PayloadAuditRow) error {
		t.Fatal("fn should not be called for empty convKey")
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error for empty convKey, got %v", err)
	}
}

func TestStreamConversation_WithLimit(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	base := time.Now().UTC().Truncate(time.Millisecond)
	convKey := "stream-limit-conv"
	var events []*PayloadAuditEvent
	for i := 0; i < 5; i++ {
		events = append(events, &PayloadAuditEvent{
			ID: int64(12000 + i), CreatedAt: base.Add(-time.Duration(5-i) * time.Second),
			ConversationKey: convKey,
		})
	}
	_ = repo.BatchInsertWithToken(context.Background(), events, "stream-limit-test")

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	var count int
	_ = repo.StreamConversation(context.Background(), convKey, from, to, 3, func(*PayloadAuditRow) error {
		count++
		return nil
	})
	if count != 3 {
		t.Fatalf("expected limit=3 to yield 3 rows, got %d", count)
	}
}

func TestStreamConversation_FnErrorStopsIteration(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	base := time.Now().UTC().Truncate(time.Millisecond)
	convKey := "stream-fn-err"
	var events []*PayloadAuditEvent
	for i := 0; i < 5; i++ {
		events = append(events, &PayloadAuditEvent{
			ID: int64(13000 + i), CreatedAt: base.Add(time.Duration(i) * time.Second),
			ConversationKey: convKey,
		})
	}
	_ = repo.BatchInsertWithToken(context.Background(), events, "stream-fn-err-test")

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	errSentinel := &struct{ error }{error: nil}
	_ = errSentinel
	stopErr := context.DeadlineExceeded // any non-nil error

	count := 0
	err := repo.StreamConversation(context.Background(), convKey, from, to, 0, func(*PayloadAuditRow) error {
		count++
		if count == 2 {
			return stopErr
		}
		return nil
	})
	if err != stopErr {
		t.Fatalf("expected stopErr to be propagated, got %v", err)
	}
	if count != 2 {
		t.Fatalf("expected fn to be called exactly 2 times before stopping, got %d", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ConversationMeta
// ─────────────────────────────────────────────────────────────────────────────

func TestConversationMeta(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })

	if err := EnsureSchema(context.Background(), conn, table, 30); err != nil {
		t.Fatal(err)
	}
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	base := time.Now().UTC().Truncate(time.Millisecond)
	convKey := "meta-conv-abc"

	events := []*PayloadAuditEvent{
		{
			ID: 14001, CreatedAt: base.Add(-2 * time.Second),
			ConversationKey: convKey, ResponseID: "meta-resp-1",
		},
		{
			ID: 14002, CreatedAt: base.Add(-1 * time.Second),
			ConversationKey: convKey, ResponseID: "meta-resp-2",
		},
		{
			ID: 14003, CreatedAt: base,
			ConversationKey: convKey, ResponseID: "meta-resp-3",
		},
		// Another conv — must not pollute results.
		{
			ID: 14004, CreatedAt: base,
			ConversationKey: "other-meta-conv", ResponseID: "other-resp",
		},
	}
	if err := repo.BatchInsertWithToken(context.Background(), events, "meta-test"); err != nil {
		t.Fatal(err)
	}

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	count, timeFrom, timeTo, responseIDs, err := repo.ConversationMeta(context.Background(), convKey, from, to)
	if err != nil {
		t.Fatal(err)
	}

	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	// timeFrom should match the earliest row.
	if !timeFrom.Equal(base.Add(-2 * time.Second)) {
		t.Errorf("timeFrom = %v, want %v", timeFrom, base.Add(-2*time.Second))
	}
	// timeTo should match the latest row.
	if !timeTo.Equal(base) {
		t.Errorf("timeTo = %v, want %v", timeTo, base)
	}

	// All three response IDs returned.
	wantIDs := []string{"meta-resp-1", "meta-resp-2", "meta-resp-3"}
	for _, id := range wantIDs {
		if !responseIDs[id] {
			t.Errorf("responseIDs missing %q; got: %v", id, responseIDs)
		}
	}
	// Other conv's response ID must not be present.
	if responseIDs["other-resp"] {
		t.Error("responseIDs must not contain other conversation's response_id")
	}
}

func TestConversationMeta_EmptyKey(t *testing.T) {
	conn := testCHConn(t)
	repo := NewPayloadAuditCHRepoWithTable(conn, "irrelevant")
	count, tf, tt, rids, err := repo.ConversationMeta(context.Background(), "", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("expected nil error for empty convKey, got %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0 for empty convKey, got %d", count)
	}
	if !tf.IsZero() || !tt.IsZero() {
		t.Errorf("expected zero times for empty convKey, got from=%v to=%v", tf, tt)
	}
	if len(rids) != 0 {
		t.Errorf("expected empty responseIDs for empty convKey, got %v", rids)
	}
}

func TestConversationMeta_NoRows(t *testing.T) {
	conn := testCHConn(t)
	table := tempTableName(t)
	t.Cleanup(func() { _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	_ = EnsureSchema(context.Background(), conn, table, 30)
	repo := NewPayloadAuditCHRepoWithTable(conn, table)

	count, _, _, rids, err := repo.ConversationMeta(context.Background(), "nonexistent-conv",
		time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("expected nil error for missing conv, got %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0 for nonexistent conv, got %d", count)
	}
	if len(rids) != 0 {
		t.Errorf("expected empty responseIDs for nonexistent conv, got %v", rids)
	}
}
