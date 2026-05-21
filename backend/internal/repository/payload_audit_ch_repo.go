package repository

import (
	"context"
	"fmt"

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
