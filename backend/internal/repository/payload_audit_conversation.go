package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// StreamConversation iterates conversation rows ascending WITHOUT collecting
// them into a slice. limit<=0 means no limit (worker path). fn is called for
// each scanned row; returning a non-nil error stops iteration immediately and
// that error is returned. The same WHERE/ORDER clause as ListConversation is
// used so both paths are consistent.
// Empty convKey returns nil without error (matches ListConversation).
func (r *PayloadAuditCHRepo) StreamConversation(ctx context.Context, convKey string, from, to time.Time, limit int, fn func(*PayloadAuditRow) error) error {
	if convKey == "" {
		return nil
	}
	var q string
	if limit > 0 {
		q = fmt.Sprintf(
			"SELECT %s FROM %s WHERE conversation_key = @ck AND created_at >= @from AND created_at <= @to ORDER BY created_at ASC, id ASC LIMIT %d",
			payloadAuditFullCols, r.table, limit,
		)
	} else {
		q = fmt.Sprintf(
			"SELECT %s FROM %s WHERE conversation_key = @ck AND created_at >= @from AND created_at <= @to ORDER BY created_at ASC, id ASC",
			payloadAuditFullCols, r.table,
		)
	}
	rows, err := r.conn.Query(ctx, q,
		clickhouse.Named("ck", convKey),
		clickhouse.Named("from", from), clickhouse.Named("to", to))
	if err != nil {
		return fmt.Errorf("payload_audit ch stream conversation: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanCHRow(rows, true)
		if err != nil {
			return err
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ConversationMeta returns cheap metadata for a conversation WITHOUT fetching
// input_body/output_body. It is used by the streaming export worker to
// pre-build the responseID set (for chain-gap detection) and to know the
// approximate time range before streaming begins.
//
// Returns count=0 with zero times and an empty map when no rows are found (not an error).
func (r *PayloadAuditCHRepo) ConversationMeta(ctx context.Context, convKey string, from, to time.Time) (count int, timeFrom, timeTo time.Time, responseIDs map[string]bool, err error) {
	responseIDs = make(map[string]bool)
	if convKey == "" {
		return 0, time.Time{}, time.Time{}, responseIDs, nil
	}

	// Fetch count + time range in one query.
	qMeta := fmt.Sprintf(
		"SELECT count(), min(created_at), max(created_at) FROM %s WHERE conversation_key = @ck AND created_at >= @from AND created_at <= @to",
		r.table,
	)
	var cnt uint64
	rowMeta := r.conn.QueryRow(ctx, qMeta,
		clickhouse.Named("ck", convKey),
		clickhouse.Named("from", from), clickhouse.Named("to", to))
	if scanErr := rowMeta.Scan(&cnt, &timeFrom, &timeTo); scanErr != nil {
		err = fmt.Errorf("payload_audit ch conversation meta: %w", scanErr)
		return
	}
	count = int(cnt)
	if count == 0 {
		return
	}

	// Fetch all response_ids (no body columns).
	qIDs := fmt.Sprintf(
		"SELECT response_id FROM %s WHERE conversation_key = @ck AND created_at >= @from AND created_at <= @to AND response_id != ''",
		r.table,
	)
	idRows, idErr := r.conn.Query(ctx, qIDs,
		clickhouse.Named("ck", convKey),
		clickhouse.Named("from", from), clickhouse.Named("to", to))
	if idErr != nil {
		err = fmt.Errorf("payload_audit ch conversation meta response_ids: %w", idErr)
		return
	}
	defer idRows.Close()
	for idRows.Next() {
		var rid string
		if scanErr := idRows.Scan(&rid); scanErr != nil {
			err = fmt.Errorf("payload_audit ch conversation meta scan response_id: %w", scanErr)
			return
		}
		responseIDs[rid] = true
	}
	err = idRows.Err()
	return
}

// ListByCacheKeyNeedle finds rows whose input_body contains `needle` (e.g.
// `prompt_cache_key":"<pck>`), scoped to a user + time window. Used to recover
// historical conversations whose conversation_key column is empty. The needle
// match runs server-side (CH scans only the user's window via the user_id index).
// Empty needle returns nil without error.
// limit <= 0 or > 2000 defaults to 500.
func (r *PayloadAuditCHRepo) ListByCacheKeyNeedle(ctx context.Context, userID *int64, needle string, from, to time.Time, limit int) ([]*PayloadAuditRow, error) {
	if needle == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	where := []string{"created_at >= @from", "created_at <= @to", "position(input_body, @needle) > 0"}
	args := []any{clickhouse.Named("from", from), clickhouse.Named("to", to), clickhouse.Named("needle", needle)}
	if userID != nil && *userID != 0 {
		where = append(where, "user_id = @uid")
		args = append(args, clickhouse.Named("uid", *userID))
	}
	// max_execution_time bounds the position(input_body, needle) scan: input_body
	// is a multi-GiB column, so an unbounded scan over a heavy user's window can run
	// for many seconds and hammer ClickHouse. On exceed, CH errors and the caller
	// degrades to a single-turn export instead of hanging.
	q := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY created_at ASC, id ASC LIMIT %d SETTINGS max_execution_time=25",
		payloadAuditFullCols, r.table, strings.Join(where, " AND "), limit,
	)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("payload_audit ch list by cachekey: %w", err)
	}
	defer rows.Close()
	var out []*PayloadAuditRow
	for rows.Next() {
		row, err := scanCHRow(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListConversation returns all rows of one conversation_key within [from,to],
// ascending by (created_at, id), with bodies, capped at limit MATCHED rows.
// Empty convKey returns nil without error.
// limit <= 0 or > 2000 defaults to 500.
func (r *PayloadAuditCHRepo) ListConversation(ctx context.Context, convKey string, from, to time.Time, limit int) ([]*PayloadAuditRow, error) {
	if convKey == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	q := fmt.Sprintf(
		"SELECT %s FROM %s WHERE conversation_key = @ck AND created_at >= @from AND created_at <= @to ORDER BY created_at ASC, id ASC LIMIT %d",
		payloadAuditFullCols, r.table, limit,
	)
	rows, err := r.conn.Query(ctx, q,
		clickhouse.Named("ck", convKey),
		clickhouse.Named("from", from), clickhouse.Named("to", to))
	if err != nil {
		return nil, fmt.Errorf("payload_audit ch list conversation: %w", err)
	}
	defer rows.Close()
	var out []*PayloadAuditRow
	for rows.Next() {
		row, err := scanCHRow(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
