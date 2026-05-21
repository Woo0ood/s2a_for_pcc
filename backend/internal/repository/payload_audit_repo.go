package repository

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PayloadAuditEvent represents the writable fields of a payload_audit_logs row.
type PayloadAuditEvent struct {
	ID                                               int64
	RequestID                                        string
	UserID, APIKeyID, GroupID                         *int64
	UserEmail, APIKeyName, GroupName, ClientIP        string
	Endpoint, Provider, Model, UpstreamModel         string
	Stream                                           bool
	StatusCode, DurationMs                           int
	InputExcerpt, OutputExcerpt                      string
	InputBody, OutputBody                            string
	InputFormat, OutputFormat                        string
	InputBytes, OutputBytes                          int
	InputTruncated, OutputTruncated, OutputOmitted   bool
	ErrorMessage                                     string
	CreatedAt                                        time.Time
}

// PayloadAuditRow is a read-back row including the generated id.
type PayloadAuditRow struct {
	ID int64
	PayloadAuditEvent
}

// PayloadAuditListFilter controls pagination and filtering for List queries.
type PayloadAuditListFilter struct {
	From, To              time.Time
	UserID, GroupID, APIKeyID *int64
	Cursor                *PayloadAuditCursor
	Limit                 int
	KeywordILike          string
	IncludeBody           bool
}

// PayloadAuditCursor is a keyset-pagination cursor serialised as base64(JSON).
type PayloadAuditCursor struct {
	SchemaVer   int       `json:"v"`
	ToEffective time.Time `json:"to"`
	LastCreated time.Time `json:"lc"`
	LastID      int64     `json:"li"`
}

const payloadAuditCursorSchemaVer = 1

// EncodeCursor serialises a cursor to a base64-encoded JSON string.
func EncodeCursor(c *PayloadAuditCursor) (string, error) {
	if c == nil {
		return "", nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode payload audit cursor: %w", err)
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// DecodeCursor deserialises a base64-encoded JSON cursor.
func DecodeCursor(s string) (*PayloadAuditCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode payload audit cursor base64: %w", err)
	}
	var c PayloadAuditCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode payload audit cursor json: %w", err)
	}
	if c.SchemaVer != payloadAuditCursorSchemaVer {
		return nil, fmt.Errorf("cursor schema mismatch: got %d, want %d", c.SchemaVer, payloadAuditCursorSchemaVer)
	}
	return &c, nil
}

// PayloadAuditPartition holds metadata about a single partition.
type PayloadAuditPartition struct {
	Name string
	End  time.Time
}

const (
	PartitionStateAttached      = "ATTACHED"
	PartitionStateDetachPending = "DETACH_PENDING"
	PartitionStateDetached      = "DETACHED"
	PartitionStateUnknown       = "UNKNOWN"
)

var (
	ErrPayloadAuditKeywordWindowTooLarge = errors.New("payload audit: keyword search requires time window <= 7 days")

	partitionNameRe = regexp.MustCompile(`^payload_audit_logs_\d{4}_\d{2}$`)
)

// PayloadAuditRepo provides raw-SQL access to the payload_audit_logs partitioned table.
type PayloadAuditRepo struct{ db *sql.DB }

func NewPayloadAuditRepo(db *sql.DB) *PayloadAuditRepo { return &PayloadAuditRepo{db: db} }

// BatchInsertWithToken inserts events, ignoring the dedup token (PG does not
// support ClickHouse-style insert dedup). This stub satisfies the interface;
// the PG repo will be removed in Task 13.
func (r *PayloadAuditRepo) BatchInsertWithToken(ctx context.Context, events []*PayloadAuditEvent, _ string) error {
	return r.BatchInsert(ctx, events)
}

// BatchInsert inserts events in a single multi-value INSERT within a transaction.
func (r *PayloadAuditRepo) BatchInsert(ctx context.Context, events []*PayloadAuditEvent) error {
	if len(events) == 0 {
		return nil
	}

	const colCount = 28
	var sb strings.Builder
	sb.WriteString(`INSERT INTO payload_audit_logs
    (created_at, request_id, user_id, user_email, api_key_id, api_key_name,
     group_id, group_name, client_ip, endpoint, provider, model, upstream_model,
     stream, status_code, duration_ms, input_excerpt, output_excerpt,
     input_body, output_body, input_format, output_format,
     input_bytes, output_bytes, input_truncated, output_truncated, output_omitted,
     error_message)
VALUES `)

	args := make([]any, 0, len(events)*colCount)
	for i, e := range events {
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Now()
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * colCount
		sb.WriteString("(")
		for j := 0; j < colCount; j++ {
			if j > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("$")
			sb.WriteString(strconv.Itoa(base + j + 1))
		}
		sb.WriteString(")")
		args = append(args,
			e.CreatedAt,
			e.RequestID,
			nullableInt64Ptr(e.UserID),
			e.UserEmail,
			nullableInt64Ptr(e.APIKeyID),
			e.APIKeyName,
			nullableInt64Ptr(e.GroupID),
			e.GroupName,
			e.ClientIP,
			e.Endpoint,
			e.Provider,
			e.Model,
			e.UpstreamModel,
			e.Stream,
			e.StatusCode,
			e.DurationMs,
			e.InputExcerpt,
			e.OutputExcerpt,
			e.InputBody,
			e.OutputBody,
			e.InputFormat,
			e.OutputFormat,
			e.InputBytes,
			e.OutputBytes,
			e.InputTruncated,
			e.OutputTruncated,
			e.OutputOmitted,
			e.ErrorMessage,
		)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("payload audit batch insert begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("payload audit batch insert exec: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("payload audit batch insert commit: %w", err)
	}
	return nil
}

const payloadAuditSelectCols = `id, created_at, request_id, user_id, user_email, api_key_id, api_key_name,
     group_id, group_name, client_ip, endpoint, provider, model, upstream_model,
     stream, status_code, duration_ms, input_excerpt, output_excerpt,
     input_body, output_body, input_format, output_format,
     input_bytes, output_bytes, input_truncated, output_truncated, output_omitted,
     error_message`

// List returns a cursor-paginated slice of rows ordered by (created_at DESC, id DESC).
func (r *PayloadAuditRepo) List(ctx context.Context, f PayloadAuditListFilter) ([]*PayloadAuditRow, *PayloadAuditCursor, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	// keyword search window guard
	if f.KeywordILike != "" {
		if f.To.Sub(f.From) > 7*24*time.Hour {
			return nil, nil, ErrPayloadAuditKeywordWindowTooLarge
		}
	}

	// Determine effective upper bound
	toEffective := f.To
	if f.Cursor != nil {
		toEffective = f.Cursor.ToEffective
	} else {
		now := time.Now()
		if toEffective.IsZero() || toEffective.After(now) {
			toEffective = now
		}
	}

	clauses := []string{"created_at >= $1", "created_at <= $2"}
	args := []any{f.From, toEffective}
	nextArg := 3

	if f.UserID != nil {
		clauses = append(clauses, fmt.Sprintf("user_id = $%d", nextArg))
		args = append(args, *f.UserID)
		nextArg++
	}
	if f.GroupID != nil {
		clauses = append(clauses, fmt.Sprintf("group_id = $%d", nextArg))
		args = append(args, *f.GroupID)
		nextArg++
	}
	if f.APIKeyID != nil {
		clauses = append(clauses, fmt.Sprintf("api_key_id = $%d", nextArg))
		args = append(args, *f.APIKeyID)
		nextArg++
	}
	if f.Cursor != nil {
		clauses = append(clauses, fmt.Sprintf("(created_at, id) < ($%d, $%d)", nextArg, nextArg+1))
		args = append(args, f.Cursor.LastCreated, f.Cursor.LastID)
		nextArg += 2
	}
	if f.KeywordILike != "" {
		like := "%" + f.KeywordILike + "%"
		clauses = append(clauses, fmt.Sprintf("(input_excerpt ILIKE $%d OR output_excerpt ILIKE $%d)", nextArg, nextArg+1))
		args = append(args, like, like)
		nextArg += 2
	}

	args = append(args, limit)
	query := fmt.Sprintf(`SELECT %s FROM payload_audit_logs WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d`,
		payloadAuditSelectCols, strings.Join(clauses, " AND "), nextArg)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("payload audit list query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]*PayloadAuditRow, 0, limit)
	for rows.Next() {
		row, err := scanPayloadAuditRow(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("payload audit list scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("payload audit list iterate: %w", err)
	}

	var nextCursor *PayloadAuditCursor
	if len(result) == limit {
		last := result[len(result)-1]
		nextCursor = &PayloadAuditCursor{
			SchemaVer:   payloadAuditCursorSchemaVer,
			ToEffective: toEffective,
			LastCreated: last.CreatedAt,
			LastID:      last.ID,
		}
	}

	return result, nextCursor, nil
}

// Get retrieves a single row by id. If createdAt is non-zero, it is used as a
// partition hint to enable index usage; otherwise all partitions are scanned.
func (r *PayloadAuditRepo) Get(ctx context.Context, id int64, createdAt time.Time) (*PayloadAuditRow, error) {
	var query string
	var args []any
	if !createdAt.IsZero() {
		query = fmt.Sprintf("SELECT %s FROM payload_audit_logs WHERE id = $1 AND created_at = $2 LIMIT 1", payloadAuditSelectCols)
		args = []any{id, createdAt}
	} else {
		query = fmt.Sprintf("SELECT %s FROM payload_audit_logs WHERE id = $1 LIMIT 1", payloadAuditSelectCols)
		args = []any{id}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("payload audit get query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("payload audit get iterate: %w", err)
		}
		return nil, sql.ErrNoRows
	}
	row, err := scanPayloadAuditRow(rows)
	if err != nil {
		return nil, fmt.Errorf("payload audit get scan: %w", err)
	}
	return row, nil
}

// ListPartitionsBefore returns monthly partitions whose end time is <= cutoff.
func (r *PayloadAuditRepo) ListPartitionsBefore(ctx context.Context, cutoff time.Time) ([]PayloadAuditPartition, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT partition_name, partition_end FROM payload_audit_partitions_before($1)", cutoff)
	if err != nil {
		return nil, fmt.Errorf("payload audit list partitions before: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []PayloadAuditPartition
	for rows.Next() {
		var p PayloadAuditPartition
		if err := rows.Scan(&p.Name, &p.End); err != nil {
			return nil, fmt.Errorf("payload audit list partitions scan: %w", err)
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payload audit list partitions iterate: %w", err)
	}
	return result, nil
}

// PartitionState returns the state of a named partition.
func (r *PayloadAuditRepo) PartitionState(ctx context.Context, name string) (string, error) {
	if !partitionNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid partition name: %s", name)
	}

	// Check if table exists at all
	var tableExists bool
	if err := r.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind IN ('r','p'))", name,
	).Scan(&tableExists); err != nil {
		return "", fmt.Errorf("payload audit partition state check table: %w", err)
	}
	if !tableExists {
		return PartitionStateUnknown, nil
	}

	// Check if it's in pg_inherits (i.e. still a child of payload_audit_logs)
	var inInherits bool
	var detachPending bool
	err := r.db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    JOIN pg_class p ON p.oid = i.inhparent
    WHERE c.relname = $1 AND p.relname = 'payload_audit_logs'
),
COALESCE((
    SELECT i.inhdetachpending FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    JOIN pg_class p ON p.oid = i.inhparent
    WHERE c.relname = $1 AND p.relname = 'payload_audit_logs'
), FALSE)
`, name).Scan(&inInherits, &detachPending)
	if err != nil {
		return "", fmt.Errorf("payload audit partition state check inherits: %w", err)
	}

	if !inInherits {
		return PartitionStateDetached, nil
	}
	if detachPending {
		return PartitionStateDetachPending, nil
	}
	return PartitionStateAttached, nil
}

// DetachPartitionConcurrently issues ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY
// on a dedicated connection with conservative lock/statement timeouts.
func (r *PayloadAuditRepo) DetachPartitionConcurrently(ctx context.Context, name string) error {
	if !partitionNameRe.MatchString(name) {
		return fmt.Errorf("invalid partition name: %s", name)
	}

	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("payload audit detach partition conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SET lock_timeout = '5s'"); err != nil {
		return fmt.Errorf("payload audit detach set lock_timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = '60s'"); err != nil {
		return fmt.Errorf("payload audit detach set statement_timeout: %w", err)
	}

	_, err = conn.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE payload_audit_logs DETACH PARTITION %s CONCURRENTLY", quoteIdentifier(name)))
	if err != nil {
		return fmt.Errorf("payload audit detach partition: %w", err)
	}
	return nil
}

// FinalizePartitionDetach finalizes a pending DETACH CONCURRENTLY operation.
func (r *PayloadAuditRepo) FinalizePartitionDetach(ctx context.Context, name string) error {
	if !partitionNameRe.MatchString(name) {
		return fmt.Errorf("invalid partition name: %s", name)
	}
	_, err := r.db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE payload_audit_logs DETACH PARTITION %s FINALIZE", quoteIdentifier(name)))
	if err != nil {
		return fmt.Errorf("payload audit finalize detach: %w", err)
	}
	return nil
}

// DropPartition drops a previously detached partition table.
func (r *PayloadAuditRepo) DropPartition(ctx context.Context, name string) error {
	if !partitionNameRe.MatchString(name) {
		return fmt.Errorf("invalid partition name: %s", name)
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", quoteIdentifier(name)))
	if err != nil {
		return fmt.Errorf("payload audit drop partition: %w", err)
	}
	return nil
}

// CreatePartition creates a monthly partition via the SQL helper function.
func (r *PayloadAuditRepo) CreatePartition(ctx context.Context, monthStart time.Time) error {
	_, err := r.db.ExecContext(ctx, "SELECT payload_audit_create_partition($1)", monthStart)
	if err != nil {
		return fmt.Errorf("payload audit create partition: %w", err)
	}
	return nil
}

// scanPayloadAuditRow scans a row from a *sql.Rows into a PayloadAuditRow.
func scanPayloadAuditRow(rows *sql.Rows) (*PayloadAuditRow, error) {
	var row PayloadAuditRow
	var userID, apiKeyID, groupID sql.NullInt64
	err := rows.Scan(
		&row.ID,
		&row.CreatedAt,
		&row.RequestID,
		&userID,
		&row.UserEmail,
		&apiKeyID,
		&row.APIKeyName,
		&groupID,
		&row.GroupName,
		&row.ClientIP,
		&row.Endpoint,
		&row.Provider,
		&row.Model,
		&row.UpstreamModel,
		&row.Stream,
		&row.StatusCode,
		&row.DurationMs,
		&row.InputExcerpt,
		&row.OutputExcerpt,
		&row.InputBody,
		&row.OutputBody,
		&row.InputFormat,
		&row.OutputFormat,
		&row.InputBytes,
		&row.OutputBytes,
		&row.InputTruncated,
		&row.OutputTruncated,
		&row.OutputOmitted,
		&row.ErrorMessage,
	)
	if err != nil {
		return nil, err
	}
	if userID.Valid {
		v := userID.Int64
		row.UserID = &v
	}
	if apiKeyID.Valid {
		v := apiKeyID.Int64
		row.APIKeyID = &v
	}
	if groupID.Valid {
		v := groupID.Int64
		row.GroupID = &v
	}
	return &row, nil
}

// nullableInt64Ptr converts *int64 to a sql-safe nullable value.
func nullableInt64Ptr(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// quoteIdentifier wraps an identifier in double quotes, escaping embedded quotes.
func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
