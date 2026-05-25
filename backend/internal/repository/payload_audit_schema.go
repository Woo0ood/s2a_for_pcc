package repository

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// payloadAuditSchemaSQL 表 DDL。
//
// ClickHouse 版本要求：>= 26.x。
// 该 DDL 让 DateTime64(3,'UTC') 直接出现在 PARTITION BY / TTL 表达式中。
// CH 26 起放宽了 TTL 表达式类型校验，接受 DateTime64；22 / 23 / 24 / 25 会拒绝
// 并返回 BAD_TTL_EXPRESSION (Code: 450) "TTL expression result column should
// have DateTime or Date type, but has DateTime64(3, 'UTC')"。
//
// 若部署目标 CH 版本低于 26，需把 created_at 出现在 PARTITION BY / TTL 处包裹
// 成 toDateTime(created_at)（语义等价，天级别 TTL 不受秒以下精度影响）。注意：
// 修改后，已有线上表会在下一次 retention 漂移触发 AlterTTL 时被改写表达式，
// 引起一次全分区的 TTL 重评估 mutation。
//
// 同步要求：本常量、EnsureSchema 中的 ALTER 语句、以及
// PayloadAuditCHRepo.AlterTTL 三处的 TTL 表达式必须保持一致，否则 EnsureSchema
// 的 TTL 漂移检测 (ttlIntervalRe) 会因表达式不同而误报漂移。
const payloadAuditSchemaSQL = `
CREATE TABLE IF NOT EXISTS %s
(
    id                Int64                  CODEC(Delta, ZSTD(1)),
    created_at        DateTime64(3, 'UTC')   CODEC(Delta, ZSTD(1)),
    request_id        String,
    user_id           Int64                  DEFAULT 0,
    user_email        String,
    api_key_id        Int64                  DEFAULT 0,
    api_key_name      String,
    group_id          Int64                  DEFAULT 0,
    group_name        String,
    client_ip         IPv6                   DEFAULT toIPv6('::'),
    endpoint          LowCardinality(String),
    provider          LowCardinality(String),
    model             LowCardinality(String),
    upstream_model    LowCardinality(String),
    stream            Bool,
    status_code       UInt16,
    duration_ms       UInt32                 CODEC(T64, ZSTD(1)),
    input_excerpt     String,
    output_excerpt    String,
    input_body        String                 CODEC(ZSTD(3)),
    output_body       String                 CODEC(ZSTD(3)),
    input_format      LowCardinality(String),
    output_format     LowCardinality(String),
    input_bytes       UInt32                 CODEC(T64, ZSTD(1)),
    output_bytes      UInt32                 CODEC(T64, ZSTD(1)),
    input_truncated   Bool,
    output_truncated  Bool,
    output_omitted    Bool,
    error_message     String,

    INDEX idx_id          id          TYPE bloom_filter(0.001) GRANULARITY 1,
    INDEX idx_request_id  request_id  TYPE bloom_filter(0.01)  GRANULARITY 4,
    INDEX idx_user_id     user_id     TYPE bloom_filter(0.01)  GRANULARITY 4,
    INDEX idx_api_key_id  api_key_id  TYPE bloom_filter(0.01)  GRANULARITY 4,
    INDEX idx_group_id    group_id    TYPE bloom_filter(0.01)  GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(created_at)
ORDER BY (created_at, id)
TTL created_at + INTERVAL %d DAY
SETTINGS
    index_granularity = 8192,
    non_replicated_deduplication_window = 1000
`

var ttlIntervalRe = regexp.MustCompile(`(?:INTERVAL\s+(\d+)\s+DAY|toIntervalDay\((\d+)\))`)

// EnsureSchema creates the table if missing, and ALTERs TTL if retention drift is detected.
// Table name is parameterized so tests can use unique names; production uses "payload_audit_logs".
func EnsureSchema(ctx context.Context, conn clickhouse.Conn, table string, retentionDays int) error {
	if retentionDays < 1 {
		retentionDays = 180
	}
	ddl := fmt.Sprintf(payloadAuditSchemaSQL, quoteCHIdentifier(table), retentionDays)
	if err := conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("payload_audit ensure schema create: %w", err)
	}
	current, err := readTableTTLDays(ctx, conn, table)
	if err != nil {
		return err
	}
	if current == retentionDays {
		return nil
	}
	alter := fmt.Sprintf(
		"ALTER TABLE %s MODIFY TTL created_at + INTERVAL %d DAY",
		quoteCHIdentifier(table), retentionDays,
	)
	if err := conn.Exec(ctx, alter); err != nil {
		return fmt.Errorf("payload_audit ensure schema alter ttl: %w", err)
	}
	return nil
}

func readTableTTLDays(ctx context.Context, conn clickhouse.Conn, table string) (int, error) {
	var engineFull string
	err := conn.QueryRow(ctx,
		"SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = ?", table,
	).Scan(&engineFull)
	if err != nil {
		return 0, fmt.Errorf("payload_audit read engine_full: %w", err)
	}
	m := ttlIntervalRe.FindStringSubmatch(engineFull)
	if m == nil {
		return 0, nil
	}
	// m[1] is from "INTERVAL N DAY", m[2] is from "toIntervalDay(N)"
	s := m[1]
	if s == "" {
		s = m[2]
	}
	n, _ := strconv.Atoi(s)
	return n, nil
}

func quoteCHIdentifier(name string) string {
	return "`" + name + "`"
}
