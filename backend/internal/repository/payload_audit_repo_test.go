//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ensureTestUser inserts a user row and returns its auto-generated ID.
func ensureTestUser(t *testing.T, email string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(context.Background(),
		`INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		 VALUES ($1, 'hash', 'user', 'active', 0, 1)
		 RETURNING id`, email).Scan(&id))
	return id
}

// createCurrentMonthPartition creates a partition covering the current month.
func createCurrentMonthPartition(t *testing.T, repo *PayloadAuditRepo) {
	t.Helper()
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repo.CreatePartition(context.Background(), monthStart))
}

// makeEvent creates a test PayloadAuditEvent with sensible defaults.
func makeEvent(idx int, overrides ...func(*PayloadAuditEvent)) *PayloadAuditEvent {
	e := &PayloadAuditEvent{
		RequestID:    fmt.Sprintf("req-%04d", idx),
		UserEmail:    fmt.Sprintf("user%d@example.com", idx),
		APIKeyName:   "default",
		GroupName:    "test-group",
		ClientIP:     "127.0.0.1",
		Endpoint:     "/v1/chat/completions",
		Provider:     "openai",
		Model:        "gpt-4",
		UpstreamModel: "gpt-4-turbo",
		Stream:       idx%2 == 0,
		StatusCode:   200,
		DurationMs:   100 + idx,
		InputExcerpt:  fmt.Sprintf("input excerpt %d", idx),
		OutputExcerpt: fmt.Sprintf("output excerpt %d", idx),
		InputBody:    strings.Repeat("A", 5120),
		OutputBody:   strings.Repeat("B", 5120),
		InputFormat:  "json",
		OutputFormat: "text",
		InputBytes:   5120,
		OutputBytes:  5120,
		InputTruncated:  false,
		OutputTruncated: false,
		OutputOmitted:   false,
		ErrorMessage: "",
		CreatedAt:    time.Now().Add(-time.Duration(1000-idx) * time.Millisecond),
	}
	for _, fn := range overrides {
		fn(e)
	}
	return e
}

func TestPayloadAuditRepo_BatchInsert_Roundtrip(t *testing.T) {
	ctx := context.Background()
	repo := NewPayloadAuditRepo(integrationDB)
	createCurrentMonthPartition(t, repo)

	events := make([]*PayloadAuditEvent, 100)
	for i := range events {
		events[i] = makeEvent(i)
	}
	require.NoError(t, repo.BatchInsert(ctx, events))

	// Verify count
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM payload_audit_logs WHERE request_id LIKE 'req-%'").Scan(&count))
	require.GreaterOrEqual(t, count, 100)

	// Verify single row content
	row, err := repo.Get(ctx, 0, time.Time{}) // fallback: scan all partitions
	// Get by request_id instead since we don't know the id
	var id int64
	var createdAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT id, created_at FROM payload_audit_logs WHERE request_id = $1 LIMIT 1", "req-0050",
	).Scan(&id, &createdAt))

	row, err = repo.Get(ctx, id, createdAt)
	require.NoError(t, err)
	require.Equal(t, "req-0050", row.RequestID)
	require.Equal(t, "user50@example.com", row.UserEmail)
	require.Equal(t, "/v1/chat/completions", row.Endpoint)
	require.Equal(t, "openai", row.Provider)
	require.Equal(t, "gpt-4", row.Model)
	require.Equal(t, "gpt-4-turbo", row.UpstreamModel)
	require.Equal(t, true, row.Stream) // 50 is even
	require.Equal(t, 200, row.StatusCode)
}

func TestPayloadAuditRepo_List_ByUserAndTimeRange(t *testing.T) {
	ctx := context.Background()
	repo := NewPayloadAuditRepo(integrationDB)
	createCurrentMonthPartition(t, repo)

	uid42 := ensureTestUser(t, "payload-audit-user42@example.com")
	uid43 := ensureTestUser(t, "payload-audit-user43@example.com")
	now := time.Now()
	events := make([]*PayloadAuditEvent, 10)
	for i := 0; i < 5; i++ {
		events[i] = makeEvent(2000+i, func(e *PayloadAuditEvent) {
			e.UserID = &uid42
			e.RequestID = fmt.Sprintf("user42-req-%04d", i)
			e.CreatedAt = now.Add(-time.Duration(5-i) * time.Second)
		})
	}
	for i := 0; i < 5; i++ {
		events[5+i] = makeEvent(2100+i, func(e *PayloadAuditEvent) {
			e.UserID = &uid43
			e.RequestID = fmt.Sprintf("user43-req-%04d", i)
			e.CreatedAt = now.Add(-time.Duration(5-i) * time.Second)
		})
	}
	require.NoError(t, repo.BatchInsert(ctx, events))

	rows, _, err := repo.List(ctx, PayloadAuditListFilter{
		From:   now.Add(-1 * time.Hour),
		To:     now.Add(1 * time.Minute),
		UserID: &uid42,
		Limit:  100,
	})
	require.NoError(t, err)
	require.Len(t, rows, 5)
	for _, r := range rows {
		require.NotNil(t, r.UserID)
		require.Equal(t, uid42, *r.UserID)
	}
}

func TestPayloadAuditRepo_List_CursorMonotonic(t *testing.T) {
	ctx := context.Background()
	repo := NewPayloadAuditRepo(integrationDB)
	createCurrentMonthPartition(t, repo)

	now := time.Now()
	uniquePrefix := fmt.Sprintf("cursor-mono-%d", now.UnixNano())
	events := make([]*PayloadAuditEvent, 10)
	for i := range events {
		events[i] = makeEvent(3000+i, func(e *PayloadAuditEvent) {
			e.RequestID = fmt.Sprintf("%s-%04d", uniquePrefix, i)
			e.InputExcerpt = fmt.Sprintf("%s input %d", uniquePrefix, i)
			e.CreatedAt = now.Add(-time.Duration(10-i) * time.Second)
		})
	}
	require.NoError(t, repo.BatchInsert(ctx, events))

	var allRows []*PayloadAuditRow
	var cursor *PayloadAuditCursor
	for page := 0; page < 4; page++ {
		rows, nextCursor, err := repo.List(ctx, PayloadAuditListFilter{
			From:   now.Add(-1 * time.Hour),
			To:     now.Add(1 * time.Minute),
			Cursor: cursor,
			Limit:  3,
			KeywordILike: uniquePrefix,
		})
		require.NoError(t, err)
		allRows = append(allRows, rows...)
		cursor = nextCursor
		if nextCursor == nil {
			break
		}
	}
	require.Equal(t, 10, len(allRows), "expected 3+3+3+1=10 rows total")

	// Verify strict DESC ordering and no duplicates
	seen := map[int64]bool{}
	for i := 1; i < len(allRows); i++ {
		prev := allRows[i-1]
		curr := allRows[i]
		require.False(t, seen[curr.ID], "duplicate id %d", curr.ID)
		seen[curr.ID] = true
		require.True(t,
			prev.CreatedAt.After(curr.CreatedAt) ||
				(prev.CreatedAt.Equal(curr.CreatedAt) && prev.ID > curr.ID),
			"ordering violation at index %d", i)
	}
	seen[allRows[0].ID] = true
}

func TestPayloadAuditRepo_List_HighWaterMark(t *testing.T) {
	ctx := context.Background()
	repo := NewPayloadAuditRepo(integrationDB)
	createCurrentMonthPartition(t, repo)

	now := time.Now()
	uniquePrefix := fmt.Sprintf("hwm-%d", now.UnixNano())
	events := make([]*PayloadAuditEvent, 5)
	for i := range events {
		events[i] = makeEvent(4000+i, func(e *PayloadAuditEvent) {
			e.RequestID = fmt.Sprintf("%s-%04d", uniquePrefix, i)
			e.InputExcerpt = fmt.Sprintf("%s input %d", uniquePrefix, i)
			e.CreatedAt = now.Add(-time.Duration(5-i) * time.Second)
		})
	}
	require.NoError(t, repo.BatchInsert(ctx, events))

	// First page: limit=2
	rows1, cursor1, err := repo.List(ctx, PayloadAuditListFilter{
		From:         now.Add(-1 * time.Hour),
		To:           now.Add(1 * time.Minute),
		Limit:        2,
		KeywordILike: uniquePrefix,
	})
	require.NoError(t, err)
	require.Len(t, rows1, 2)
	require.NotNil(t, cursor1)

	// Insert a new event with future timestamp (after ToEffective was frozen)
	lateEvent := makeEvent(4999, func(e *PayloadAuditEvent) {
		e.RequestID = fmt.Sprintf("%s-late", uniquePrefix)
		e.InputExcerpt = fmt.Sprintf("%s input late", uniquePrefix)
		e.CreatedAt = now.Add(10 * time.Second)
	})
	require.NoError(t, repo.BatchInsert(ctx, []*PayloadAuditEvent{lateEvent}))

	// Second page with cursor: should only see remaining 3 from original set
	var remaining []*PayloadAuditRow
	cursor := cursor1
	for cursor != nil {
		rows, nextCursor, err := repo.List(ctx, PayloadAuditListFilter{
			From:         now.Add(-1 * time.Hour),
			To:           now.Add(1 * time.Minute),
			Cursor:       cursor,
			Limit:        10,
			KeywordILike: uniquePrefix,
		})
		require.NoError(t, err)
		remaining = append(remaining, rows...)
		cursor = nextCursor
	}
	require.Equal(t, 3, len(remaining), "should only return remaining 3 original rows, not the late one")

	// Verify none of the remaining rows is the late event
	for _, r := range remaining {
		require.NotEqual(t, fmt.Sprintf("%s-late", uniquePrefix), r.RequestID)
	}
}

func TestPayloadAuditRepo_List_KeywordWindowTooLarge(t *testing.T) {
	ctx := context.Background()
	repo := NewPayloadAuditRepo(integrationDB)

	now := time.Now()
	_, _, err := repo.List(ctx, PayloadAuditListFilter{
		From:         now.Add(-30 * 24 * time.Hour),
		To:           now,
		KeywordILike: "x",
		Limit:        10,
	})
	require.ErrorIs(t, err, ErrPayloadAuditKeywordWindowTooLarge)
}

func TestPayloadAuditRepo_PartitionLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := NewPayloadAuditRepo(integrationDB)

	// Create a partition for Jan 2025
	monthStart := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repo.CreatePartition(ctx, monthStart))

	// Should be ATTACHED
	state, err := repo.PartitionState(ctx, "payload_audit_logs_2025_01")
	require.NoError(t, err)
	require.Equal(t, PartitionStateAttached, state)

	// Detach concurrently
	require.NoError(t, repo.DetachPartitionConcurrently(ctx, "payload_audit_logs_2025_01"))

	// Poll until no longer ATTACHED (may go directly to DETACHED or through DETACH_PENDING)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, err = repo.PartitionState(ctx, "payload_audit_logs_2025_01")
		require.NoError(t, err)
		if state != PartitionStateAttached {
			break
		}
		time.Sleep(1 * time.Second)
	}
	require.NotEqual(t, PartitionStateAttached, state, "partition should no longer be ATTACHED after detach")

	// If DETACH_PENDING, finalize
	if state == PartitionStateDetachPending {
		require.NoError(t, repo.FinalizePartitionDetach(ctx, "payload_audit_logs_2025_01"))
		state, err = repo.PartitionState(ctx, "payload_audit_logs_2025_01")
		require.NoError(t, err)
	}
	require.Equal(t, PartitionStateDetached, state)

	// Drop
	require.NoError(t, repo.DropPartition(ctx, "payload_audit_logs_2025_01"))

	// Should be UNKNOWN
	state, err = repo.PartitionState(ctx, "payload_audit_logs_2025_01")
	require.NoError(t, err)
	require.Equal(t, PartitionStateUnknown, state)

	// ListPartitionsBefore should not return it
	partitions, err := repo.ListPartitionsBefore(ctx, time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	for _, p := range partitions {
		require.NotEqual(t, "payload_audit_logs_2025_01", p.Name)
	}
}
