package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const defaultPayloadAuditTable = "payload_audit_logs"

type PayloadAuditCHRepo struct {
	conn  clickhouse.Conn
	table string
}

func NewPayloadAuditCHRepo(conn clickhouse.Conn) *PayloadAuditCHRepo {
	return &PayloadAuditCHRepo{conn: conn, table: defaultPayloadAuditTable}
}

// NewPayloadAuditCHRepoWithTable is for tests so each test can isolate to its own table.
func NewPayloadAuditCHRepoWithTable(conn clickhouse.Conn, table string) *PayloadAuditCHRepo {
	return &PayloadAuditCHRepo{conn: conn, table: table}
}

// BatchInsert delegates to BatchInsertWithToken with an empty token (no dedup).
func (r *PayloadAuditCHRepo) BatchInsert(ctx context.Context, events []*PayloadAuditEvent) error {
	return r.BatchInsertWithToken(ctx, events, "")
}

func (r *PayloadAuditCHRepo) BatchInsertWithToken(ctx context.Context, events []*PayloadAuditEvent, token string) error {
	if len(events) == 0 {
		return nil
	}
	if token != "" {
		ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
			"insert_deduplication_token": token,
		}))
	}
	batch, err := r.conn.PrepareBatch(ctx, fmt.Sprintf("INSERT INTO %s", r.table))
	if err != nil {
		return fmt.Errorf("payload_audit ch prepare: %w", err)
	}
	for _, e := range events {
		userID := int64(0)
		if e.UserID != nil {
			userID = *e.UserID
		}
		apiKeyID := int64(0)
		if e.APIKeyID != nil {
			apiKeyID = *e.APIKeyID
		}
		groupID := int64(0)
		if e.GroupID != nil {
			groupID = *e.GroupID
		}
		if err := batch.Append(
			e.ID,
			e.CreatedAt,
			e.RequestID,
			userID, e.UserEmail,
			apiKeyID, e.APIKeyName,
			groupID, e.GroupName,
			parseIPv6(e.ClientIP),
			e.Endpoint, e.Provider, e.Model, e.UpstreamModel,
			e.Stream,
			uint16(e.StatusCode),
			uint32(e.DurationMs),
			e.InputExcerpt, e.OutputExcerpt,
			e.InputBody, e.OutputBody,
			e.InputFormat, e.OutputFormat,
			uint32(e.InputBytes), uint32(e.OutputBytes),
			e.InputTruncated, e.OutputTruncated, e.OutputOmitted,
			e.ErrorMessage,
		); err != nil {
			return fmt.Errorf("payload_audit ch append: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("payload_audit ch send: %w", err)
	}
	return nil
}

const payloadAuditListCols = `id, created_at, request_id, user_id, user_email, api_key_id, api_key_name,
group_id, group_name, IPv6NumToString(client_ip), endpoint, provider, model, upstream_model,
stream, status_code, duration_ms, input_excerpt, output_excerpt,
input_format, output_format, input_bytes, output_bytes,
input_truncated, output_truncated, output_omitted, error_message`

const payloadAuditFullCols = payloadAuditListCols + `, input_body, output_body`

func (r *PayloadAuditCHRepo) List(ctx context.Context, f PayloadAuditListFilter) ([]*PayloadAuditRow, *PayloadAuditCursor, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if f.KeywordILike != "" && f.To.Sub(f.From) > 7*24*time.Hour {
		return nil, nil, ErrPayloadAuditKeywordWindowTooLarge
	}
	toEffective := f.To
	if f.Cursor != nil {
		toEffective = f.Cursor.ToEffective
	} else {
		now := time.Now()
		if toEffective.IsZero() || toEffective.After(now) {
			toEffective = now
		}
	}

	cols := payloadAuditListCols
	if f.IncludeBody {
		cols = payloadAuditFullCols
	}

	where := []string{"created_at >= @from", "created_at <= @to"}
	args := []any{
		clickhouse.Named("from", f.From),
		clickhouse.Named("to", toEffective),
	}
	if f.UserID != nil && *f.UserID != 0 {
		where = append(where, "user_id = @user_id")
		args = append(args, clickhouse.Named("user_id", *f.UserID))
	}
	if f.GroupID != nil && *f.GroupID != 0 {
		where = append(where, "group_id = @group_id")
		args = append(args, clickhouse.Named("group_id", *f.GroupID))
	}
	if f.APIKeyID != nil && *f.APIKeyID != 0 {
		where = append(where, "api_key_id = @api_key_id")
		args = append(args, clickhouse.Named("api_key_id", *f.APIKeyID))
	}
	if f.Cursor != nil {
		where = append(where, "(created_at, id) < (@cur_created, @cur_id)")
		args = append(args,
			clickhouse.Named("cur_created", f.Cursor.LastCreated),
			clickhouse.Named("cur_id", f.Cursor.LastID),
		)
	}
	if f.KeywordILike != "" {
		where = append(where,
			"(positionCaseInsensitive(input_excerpt, @kw) > 0 OR positionCaseInsensitive(output_excerpt, @kw) > 0)")
		args = append(args, clickhouse.Named("kw", f.KeywordILike))
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY created_at DESC, id DESC LIMIT %d",
		cols, r.table, strings.Join(where, " AND "), limit,
	)
	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("payload_audit ch list: %w", err)
	}
	defer rows.Close()

	result := make([]*PayloadAuditRow, 0, limit)
	for rows.Next() {
		row, err := scanCHRow(rows, f.IncludeBody)
		if err != nil {
			return nil, nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *PayloadAuditCursor
	if len(result) == limit {
		last := result[len(result)-1]
		next = &PayloadAuditCursor{
			SchemaVer: payloadAuditCursorSchemaVer, ToEffective: toEffective,
			LastCreated: last.CreatedAt, LastID: last.ID,
		}
	}
	return result, next, nil
}

// scanCHRow converts a ClickHouse row (with or without body cols) into PayloadAuditRow.
// CH returns IPv6NumToString as String; nullable ints are zero-valued; we map 0 → nil for backward-compat fields.
func scanCHRow(rows interface {
	Scan(...any) error
}, includeBody bool) (*PayloadAuditRow, error) {
	var row PayloadAuditRow
	var userID, apiKeyID, groupID int64
	var statusCode uint16
	var durationMs, inputBytes, outputBytes uint32
	var clientIP string
	targets := []any{
		&row.ID, &row.CreatedAt, &row.RequestID,
		&userID, &row.UserEmail,
		&apiKeyID, &row.APIKeyName,
		&groupID, &row.GroupName,
		&clientIP,
		&row.Endpoint, &row.Provider, &row.Model, &row.UpstreamModel,
		&row.Stream, &statusCode, &durationMs,
		&row.InputExcerpt, &row.OutputExcerpt,
		&row.InputFormat, &row.OutputFormat,
		&inputBytes, &outputBytes,
		&row.InputTruncated, &row.OutputTruncated, &row.OutputOmitted,
		&row.ErrorMessage,
	}
	if includeBody {
		targets = append(targets, &row.InputBody, &row.OutputBody)
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, fmt.Errorf("payload_audit ch scan: %w", err)
	}
	row.ClientIP = clientIP
	row.StatusCode = int(statusCode)
	row.DurationMs = int(durationMs)
	row.InputBytes = int(inputBytes)
	row.OutputBytes = int(outputBytes)
	if userID != 0 {
		row.UserID = &userID
	}
	if apiKeyID != 0 {
		row.APIKeyID = &apiKeyID
	}
	if groupID != 0 {
		row.GroupID = &groupID
	}
	return &row, nil
}

var ErrCreatedAtRequired = errors.New("payload_audit ch: created_at is required for Get")

func (r *PayloadAuditCHRepo) Get(ctx context.Context, id int64, createdAt time.Time) (*PayloadAuditRow, error) {
	if createdAt.IsZero() {
		return nil, ErrCreatedAtRequired
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE id = ? AND toUnixTimestamp64Milli(created_at) = ? LIMIT 1",
		payloadAuditFullCols, r.table,
	)
	rows, err := r.conn.Query(ctx, query, id, createdAt.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("payload_audit ch get: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	return scanCHRow(rows, true)
}

func (r *PayloadAuditCHRepo) AlterTTL(ctx context.Context, retentionDays int) error {
	if retentionDays < 1 {
		retentionDays = 180
	}
	q := fmt.Sprintf("ALTER TABLE %s MODIFY TTL created_at + INTERVAL %d DAY", r.table, retentionDays)
	if err := r.conn.Exec(ctx, q); err != nil {
		return fmt.Errorf("payload_audit ch alter ttl: %w", err)
	}
	return nil
}

// DropExpiredMonthlyPartitions drops every fully-expired month partition (partition < toYYYYMM(cutoff))
// and returns the list of dropped partition ids.
func (r *PayloadAuditCHRepo) DropExpiredMonthlyPartitions(ctx context.Context, cutoff time.Time) ([]string, error) {
	cutoffMonth := uint32(cutoff.Year()*100 + int(cutoff.Month()))
	// Query distinct YYYYMM months from the table itself (avoids needing system.parts privilege).
	q := fmt.Sprintf(
		"SELECT DISTINCT toYYYYMM(created_at) AS m FROM %s WHERE toYYYYMM(created_at) < %d ORDER BY m",
		r.table, cutoffMonth,
	)
	rows, err := r.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("payload_audit ch list expired partitions: %w", err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var m uint32
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		parts = append(parts, fmt.Sprintf("%d", m))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	dropped := make([]string, 0, len(parts))
	for _, p := range parts {
		dq := fmt.Sprintf("ALTER TABLE %s DROP PARTITION %s", r.table, p)
		if err := r.conn.Exec(ctx, dq); err != nil {
			slog.Error("payload_audit ch drop partition fail", "partition", p, "err", err)
			continue
		}
		dropped = append(dropped, p)
	}
	return dropped, nil
}

// EnsureSchema delegates to the standalone EnsureSchema function using the
// repo's connection and the production table name.
func (r *PayloadAuditCHRepo) EnsureSchema(ctx context.Context, retentionDays int) error {
	if r.conn == nil {
		return nil
	}
	return EnsureSchema(ctx, r.conn, "payload_audit_logs", retentionDays)
}

func (r *PayloadAuditCHRepo) Ping(ctx context.Context) error {
	return r.conn.Ping(ctx)
}
