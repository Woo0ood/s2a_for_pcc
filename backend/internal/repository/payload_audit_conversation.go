package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

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
