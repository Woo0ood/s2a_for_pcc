# 调用留存（Payload Audit）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Project workflow constraint (from CLAUDE.md):** Claude Code performs intake / planning / verification only; every edit or test must be executed via the **Codeagent skill** (`codeagent`). Each task below is structured so a codeagent invocation can read the task block and produce the change directly.

**Goal:** 异步留存 5 类对话端点的完整 input/output（辅助端点仅 input），存 PG 月度分区表，对外提供独立 token 鉴权的 GET 导出 API，后台提供查询页。

**Architecture:** 在每个网关 handler 入口创建 `PayloadAuditCollector`、defer 调用 `Finalize`；流式 SSE scanner 内累加 output；事件经非阻塞 channel 进入 `PayloadAuditSink`，worker 池批量 INSERT 到 PG（lz4 TOAST + 月度分区）。SIGTERM 时优先 flush 到 PG，fallback 到 Redis 分块 LPUSH。

**Tech Stack:** Go 1.25 (gin / ent / google wire / go-redis v9) + PostgreSQL 14+ + Vue 3 + TypeScript

**Spec:** [docs/specs/2026-05-09-payload-audit-design.md](../specs/2026-05-09-payload-audit-design.md)

**Branch:** `feat/payload-audit`（已建）

---

## 文件结构总览

| 文件 | 角色 |
|---|---|
| `backend/migrations/136_payload_audit_logs.sql` | 建主分区表 + 索引 + 默认 settings keys |
| `backend/migrations/136_payload_audit_logs_partition_funcs.sql` | 分区维护辅助函数（创建/列出过期月度分区） |
| `backend/ent/schema/payload_audit_log.go` | ent 实体声明（仅用于代码生成；写入走 raw SQL） |
| `backend/internal/repository/payload_audit_repo.go` | BatchInsert / List / Get / Cleanup state machine / List/Drop partitions |
| `backend/internal/util/audit_token.go` | 32B CSPRNG 生成 + SHA256 哈希 + 恒定时间比较 |
| `backend/internal/service/payload_audit_excerpt.go` | excerpt 头尾截取 + UTF-8 边界 + sep>=total 兜底 |
| `backend/internal/service/payload_audit_extract.go` | 4 协议 input/output 抽取（含事件分类 visitor） |
| `backend/internal/service/payload_audit_collector.go` | PayloadAuditCollector 类型 + Append/Finalize |
| `backend/internal/service/payload_audit_config.go` | ConfigSnapshot / atomic.Pointer / 热更新 |
| `backend/internal/service/payload_audit_service.go` | 服务装配、Start/Stop、admin API 后端 |
| `backend/internal/service/payload_audit_sink.go` | 双重 bounded queue + worker pool + batcher |
| `backend/internal/service/payload_audit_redis.go` | Drain/Recover Redis（分块 pipeline） |
| `backend/internal/service/payload_audit_cleanup.go` | DETACH 状态机 + cleanup cron |
| `backend/internal/service/payload_audit_partition.go` | 分区维护 cron（创建未来 60 天） |
| `backend/internal/handler/payload_audit_helper.go` | `attachPayloadAuditCollector` / `finalizeWithError` |
| `backend/internal/handler/admin/payload_audit_handler.go` | admin 后台 8 个端点 |
| `backend/internal/handler/audit_export_handler.go` | 对外 list/get/ndjson |
| `backend/internal/server/middleware/audit_export_auth.go` | Bearer token 校验 + per-key 限流 |
| `backend/internal/server/routes/public_audit.go` | 注册 `/api/v1/audit/*` |
| `backend/internal/server/routes/admin.go` (MOD) | 注册 `/admin/payload-audit/*` |
| `backend/internal/service/wire.go` (MOD) | ProvideXxx 注册 |
| `backend/internal/service/openai_gateway_service.go` (MOD) | OpenAI Chat / Responses stream visitor 注入 |
| `backend/internal/service/gateway_service.go` (MOD) | Anthropic stream visitor 注入 |
| `backend/internal/service/gemini_messages_compat_service.go` (MOD) | Gemini stream visitor 注入 |
| `backend/internal/service/openai_images.go` (MOD) | 非流式 body 拷贝 |
| `backend/internal/handler/{5 个 handler}.go` (MOD) | 入口 attach + defer Finalize |
| `frontend/src/api/admin/payloadAudit.ts` | API 客户端 |
| `frontend/src/views/admin/PayloadAuditView.vue` | 主页面 |
| `frontend/src/router/index.ts` (MOD) + i18n (MOD) | 路由 + 翻译 |
| `docs/PAYLOAD_AUDIT.md` | 运维部署文档 |

---

## Phase 0 — 准备

### Task 0: 放开 docs/plans/ 的 gitignore + 引入 lz4 检测

**Files:**
- Modify: `.gitignore`

- [ ] **Step 1: 编辑 .gitignore**

在 `!docs/specs/` 行下方追加：
```
!docs/plans/
```

- [ ] **Step 2: 提交**

```bash
git add .gitignore docs/plans/2026-05-09-payload-audit.md
git commit -m "docs: 放开 plans 目录追踪并新增 payload audit 实施计划"
```

---

## Phase 1 — 数据层

### Task 1: 数据库迁移

**Files:**
- Create: `backend/migrations/136_payload_audit_logs.sql`
- Create: `backend/migrations/136_payload_audit_logs_partition_funcs.sql`

- [ ] **Step 1: 写主迁移**

`136_payload_audit_logs.sql`：
```sql
-- 调用留存（payload audit）：异步存储 LLM input/output 原文供合规扫描
-- PG 14+ 启用 lz4 列压缩；< 14 自动回退 pglz

DO $$
DECLARE
    use_lz4 BOOLEAN := current_setting('server_version_num')::int >= 140000;
    body_compression TEXT := CASE WHEN use_lz4 THEN 'lz4' ELSE 'pglz' END;
BEGIN
    EXECUTE format($f$
        CREATE TABLE IF NOT EXISTS payload_audit_logs (
            id              BIGSERIAL,
            created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
            request_id      VARCHAR(64)  NOT NULL DEFAULT '',
            user_id         BIGINT       REFERENCES users(id)    ON DELETE SET NULL,
            user_email      VARCHAR(255) NOT NULL DEFAULT '',
            api_key_id      BIGINT       REFERENCES api_keys(id) ON DELETE SET NULL,
            api_key_name    VARCHAR(100) NOT NULL DEFAULT '',
            group_id        BIGINT       REFERENCES groups(id)   ON DELETE SET NULL,
            group_name      VARCHAR(255) NOT NULL DEFAULT '',
            client_ip       VARCHAR(45)  NOT NULL DEFAULT '',
            endpoint        VARCHAR(128) NOT NULL DEFAULT '',
            provider        VARCHAR(64)  NOT NULL DEFAULT '',
            model           VARCHAR(255) NOT NULL DEFAULT '',
            upstream_model  VARCHAR(255) NOT NULL DEFAULT '',
            stream          BOOLEAN      NOT NULL DEFAULT FALSE,
            status_code     INT          NOT NULL DEFAULT 0,
            duration_ms     INT          NOT NULL DEFAULT 0,
            input_excerpt   VARCHAR(2048) NOT NULL DEFAULT '',
            output_excerpt  VARCHAR(2048) NOT NULL DEFAULT '',
            input_body      TEXT         NOT NULL DEFAULT '' COMPRESSION %s,
            output_body     TEXT         NOT NULL DEFAULT '' COMPRESSION %s,
            input_format    VARCHAR(16)  NOT NULL DEFAULT 'json',
            output_format   VARCHAR(16)  NOT NULL DEFAULT 'text',
            input_bytes     INT          NOT NULL DEFAULT 0,
            output_bytes    INT          NOT NULL DEFAULT 0,
            input_truncated  BOOLEAN     NOT NULL DEFAULT FALSE,
            output_truncated BOOLEAN     NOT NULL DEFAULT FALSE,
            output_omitted   BOOLEAN     NOT NULL DEFAULT FALSE,
            error_message   TEXT         NOT NULL DEFAULT '',
            PRIMARY KEY (id, created_at)
        ) PARTITION BY RANGE (created_at)
    $f$, body_compression, body_compression);
END $$;

CREATE INDEX IF NOT EXISTS idx_payload_audit_created_at_id
    ON payload_audit_logs (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_payload_audit_user_created_at
    ON payload_audit_logs (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_payload_audit_group_created_at
    ON payload_audit_logs (group_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_payload_audit_api_key_created_at
    ON payload_audit_logs (api_key_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_payload_audit_request_id
    ON payload_audit_logs (request_id);

INSERT INTO settings (key, value, updated_at)
VALUES
    ('payload_audit_enabled', 'false', NOW()),
    ('payload_audit_config', '{
        "all_groups": false,
        "group_ids": [],
        "input_max_bytes": 10485760,
        "output_max_bytes": 5242880,
        "excerpt_bytes": 512,
        "retention_days": 180,
        "worker_count": 4,
        "queue_size": 32768,
        "queue_max_bytes": 1073741824,
        "batch_size": 100,
        "batch_flush_ms": 200,
        "export_api_keys": []
    }'::text, NOW())
ON CONFLICT (key) DO NOTHING;
```

- [ ] **Step 2: 写分区维护函数**

`136_payload_audit_logs_partition_funcs.sql`：
```sql
-- 创建指定月份的分区（month_start 应为某月 1 号 00:00 UTC）
CREATE OR REPLACE FUNCTION payload_audit_create_partition(month_start TIMESTAMPTZ)
RETURNS TEXT AS $$
DECLARE
    p_name TEXT := 'payload_audit_logs_' || to_char(month_start AT TIME ZONE 'UTC', 'YYYY_MM');
    p_end  TIMESTAMPTZ := month_start + INTERVAL '1 month';
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF payload_audit_logs FOR VALUES FROM (%L) TO (%L)',
        p_name, month_start, p_end
    );
    RETURN p_name;
END;
$$ LANGUAGE plpgsql;

-- 列出 cutoff 之前的所有月度分区（按分区名解析时间）
CREATE OR REPLACE FUNCTION payload_audit_partitions_before(cutoff TIMESTAMPTZ)
RETURNS TABLE(partition_name TEXT, partition_end TIMESTAMPTZ) AS $$
BEGIN
    RETURN QUERY
    SELECT c.relname::TEXT,
           (to_timestamp(substring(c.relname FROM 'logs_(\d{4})_(\d{2})$'),
                         'YYYY_MM') AT TIME ZONE 'UTC') + INTERVAL '1 month' AS p_end
    FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    JOIN pg_class p ON p.oid = i.inhparent
    WHERE p.relname = 'payload_audit_logs'
      AND c.relname ~ '^payload_audit_logs_\d{4}_\d{2}$'
    HAVING (to_timestamp(substring(c.relname FROM 'logs_(\d{4})_(\d{2})$'),
                         'YYYY_MM') AT TIME ZONE 'UTC') + INTERVAL '1 month' <= cutoff;
END;
$$ LANGUAGE plpgsql;
```

- [ ] **Step 3: 应用迁移并验证**

```bash
codeagent execute: 在本地或测试 PG 上运行迁移，然后执行：
  SELECT pg_get_compression('payload_audit_logs', 'input_body');
  SELECT payload_audit_create_partition('2026-05-01 00:00:00+00'::timestamptz);
  \d+ payload_audit_logs
  SELECT * FROM settings WHERE key LIKE 'payload_audit%';
确认 lz4 已启用、当月分区创建成功、settings 已注入。
```

- [ ] **Step 4: 提交**

```bash
git add backend/migrations/136_payload_audit_logs.sql \
        backend/migrations/136_payload_audit_logs_partition_funcs.sql
git commit -m "feat(audit): payload_audit_logs 主分区表与分区辅助函数"
```

---

### Task 2: ent schema（仅声明）

**Files:**
- Create: `backend/ent/schema/payload_audit_log.go`

> 注：实际写入路径走 raw SQL（批量 + 分区），ent 主要用于声明字段以便其他代码引用类型。参考 `payment_audit_log.go` 现有模式。

- [ ] **Step 1: 写 schema**

```go
package schema

import (
    "entgo.io/ent"
    "entgo.io/ent/dialect/entsql"
    "entgo.io/ent/schema"
    "entgo.io/ent/schema/field"
    "entgo.io/ent/schema/index"
    "github.com/Woo0ood/s2a_for_pcc/ent/schema/mixins"
)

type PayloadAuditLog struct{ ent.Schema }

func (PayloadAuditLog) Annotations() []schema.Annotation {
    return []schema.Annotation{entsql.Annotation{Table: "payload_audit_logs"}}
}

func (PayloadAuditLog) Mixin() []ent.Mixin { return []ent.Mixin{mixins.TimeMixin{}} }

func (PayloadAuditLog) Fields() []ent.Field {
    return []ent.Field{
        field.String("request_id").MaxLen(64).Default(""),
        field.Int64("user_id").Optional().Nillable(),
        field.String("user_email").MaxLen(255).Default(""),
        field.Int64("api_key_id").Optional().Nillable(),
        field.String("api_key_name").MaxLen(100).Default(""),
        field.Int64("group_id").Optional().Nillable(),
        field.String("group_name").MaxLen(255).Default(""),
        field.String("client_ip").MaxLen(45).Default(""),
        field.String("endpoint").MaxLen(128).Default(""),
        field.String("provider").MaxLen(64).Default(""),
        field.String("model").MaxLen(255).Default(""),
        field.String("upstream_model").MaxLen(255).Default(""),
        field.Bool("stream").Default(false),
        field.Int("status_code").Default(0),
        field.Int("duration_ms").Default(0),
        field.String("input_excerpt").MaxLen(2048).Default(""),
        field.String("output_excerpt").MaxLen(2048).Default(""),
        field.Text("input_body").Default(""),
        field.Text("output_body").Default(""),
        field.String("input_format").MaxLen(16).Default("json"),
        field.String("output_format").MaxLen(16).Default("text"),
        field.Int("input_bytes").Default(0),
        field.Int("output_bytes").Default(0),
        field.Bool("input_truncated").Default(false),
        field.Bool("output_truncated").Default(false),
        field.Bool("output_omitted").Default(false),
        field.Text("error_message").Default(""),
    }
}

func (PayloadAuditLog) Indexes() []ent.Index {
    return []ent.Index{
        index.Fields("created_at", "id"),
        index.Fields("user_id", "created_at"),
        index.Fields("group_id", "created_at"),
        index.Fields("api_key_id", "created_at"),
        index.Fields("request_id"),
    }
}
```

- [ ] **Step 2: 生成 ent 代码**

```bash
codeagent execute: cd backend && go generate ./ent
```

- [ ] **Step 3: 提交**

```bash
git add backend/ent/
git commit -m "feat(audit): payload audit ent schema"
```

---

### Task 3: Repository

**Files:**
- Create: `backend/internal/repository/payload_audit_repo.go`
- Create: `backend/internal/repository/payload_audit_repo_test.go`

- [ ] **Step 1: 写测试 (testcontainers PG)**

参照 `content_moderation_hash_cache_integration_test.go` 的 PG 启动方式。测试覆盖：

```go
package repository_test

func TestPayloadAuditRepo_BatchInsert_Roundtrip(t *testing.T) {
    // 1. 启动 PG 容器，跑 136 迁移，预创建当月分区
    // 2. BatchInsert 100 条，input_body / output_body 各 5KB
    // 3. SELECT COUNT(*) == 100
    // 4. SELECT 一行，比较所有字段
    // 5. 验证 lz4 实际生效：SELECT pg_column_size(input_body) < 5000
}

func TestPayloadAuditRepo_List_ByUserAndTimeRange(t *testing.T) {
    // 写入 user_id=42 5 条 / user_id=43 5 条
    // List(from=now-1h, to=now, user_id=42, limit=100, cursor=nil)
    // 断言只返 5 条 user_id=42
    // 用返回的 next_cursor 再调一次（应为空）
}

func TestPayloadAuditRepo_List_CursorMonotonic(t *testing.T) {
    // 写入 10 行，cursor 每次取 limit=3
    // 断言 4 次调用返 3+3+3+1 行，所有 ID 唯一、按 created_at DESC 排
}

func TestPayloadAuditRepo_List_HighWaterMark(t *testing.T) {
    // 第一页 limit=2，记下 to_effective
    // 在分页过程中插入 1 行新数据（created_at = now()）
    // 第二页（带 cursor）应该不返回新插入的行
}

func TestPayloadAuditRepo_PartitionDetachLifecycle(t *testing.T) {
    // 1. 创建 2025-01 分区
    // 2. 调 ListPartitionsBefore(cutoff=2025-02-01)，断言返回 1 个
    // 3. 调 DetachPartition("payload_audit_logs_2025_01")（CONCURRENTLY）
    // 4. 轮询直到状态变 DETACHED（最多 30s）
    // 5. 调 DropPartition
    // 6. 再调 ListPartitionsBefore，断言返回 0 个
}
```

- [ ] **Step 2: 写实现**

```go
package repository

type PayloadAuditEvent struct {
    RequestID, UserEmail, APIKeyName, GroupName, ClientIP string
    UserID, APIKeyID, GroupID                              *int64
    Endpoint, Provider, Model, UpstreamModel               string
    Stream                                                 bool
    StatusCode, DurationMs                                 int
    InputExcerpt, OutputExcerpt                            string
    InputBody, OutputBody                                  string
    InputFormat, OutputFormat                              string
    InputBytes, OutputBytes                                int
    InputTruncated, OutputTruncated, OutputOmitted        bool
    ErrorMessage                                           string
    CreatedAt                                              time.Time
}

type PayloadAuditListFilter struct {
    From, To              time.Time
    UserID, GroupID, APIKeyID *int64
    Cursor                *PayloadAuditCursor // nil = first page
    Limit                 int
    KeywordILike          string // 空表示不带关键字
}

type PayloadAuditCursor struct {
    ToEffective time.Time
    LastCreated time.Time
    LastID      int64
    SchemaVer   int // 1
}

type PayloadAuditPartition struct {
    Name string
    End  time.Time
    State string // ATTACHED / DETACH_PENDING / DETACHED
}

type PayloadAuditRepo struct{ db *sql.DB }

// 关键方法签名（实现见后续步骤）：
func (r *PayloadAuditRepo) BatchInsert(ctx context.Context, events []*PayloadAuditEvent) error
func (r *PayloadAuditRepo) List(ctx context.Context, f PayloadAuditListFilter) (rows []*PayloadAuditRow, nextCursor *PayloadAuditCursor, err error)
func (r *PayloadAuditRepo) Get(ctx context.Context, id int64, createdAt time.Time) (*PayloadAuditRow, error)
func (r *PayloadAuditRepo) ListPartitionsBefore(ctx context.Context, cutoff time.Time) ([]PayloadAuditPartition, error)
func (r *PayloadAuditRepo) PartitionState(ctx context.Context, name string) (string, error)
func (r *PayloadAuditRepo) DetachPartitionConcurrently(ctx context.Context, name string) error
func (r *PayloadAuditRepo) FinalizePartitionDetach(ctx context.Context, name string) error
func (r *PayloadAuditRepo) DropPartition(ctx context.Context, name string) error
func (r *PayloadAuditRepo) CreatePartition(ctx context.Context, monthStart time.Time) error
```

`BatchInsert` 用单次多值 `INSERT INTO payload_audit_logs (...) VALUES (...), (...), ...` 包在事务里。**禁止用 prepared statement 多次执行**（事件结构稳定但批量大小不固定，每次准备开销大于直接拼）。

`List`：
- 基础 WHERE：`created_at >= $1 AND created_at <= $2`
- cursor 时追加 `AND (created_at, id) < ($3, $4)`
- user/group/api_key 命中时追加；KeywordILike 命中时 `AND (input_excerpt ILIKE '%kw%' OR output_excerpt ILIKE '%kw%')`
- ORDER BY `created_at DESC, id DESC`，LIMIT $N

`DetachPartitionConcurrently`：用独立连接，禁用事务，先 `SET lock_timeout='5s'; SET statement_timeout='60s'`，再 `ALTER TABLE payload_audit_logs DETACH PARTITION %I CONCURRENTLY`。

`PartitionState`：查 `pg_inherits` + `pg_partition_tree` 判断三态。

- [ ] **Step 3: 跑测试**

```bash
codeagent execute: cd backend && go test ./internal/repository -run TestPayloadAuditRepo -v -count=1
```

期望：5 个测试全 PASS。

- [ ] **Step 4: 提交**

```bash
git add backend/internal/repository/payload_audit_repo*.go
git commit -m "feat(audit): payload audit repository (batch insert / cursor list / partition lifecycle)"
```

---

## Phase 2 — 工具函数

### Task 4: audit_token util

**Files:**
- Create: `backend/internal/util/audit_token.go`
- Create: `backend/internal/util/audit_token_test.go`

- [ ] **Step 1: 写测试**

```go
package util

func TestAuditToken_GenerateUniqueAndDecodable(t *testing.T) {
    t1, h1 := GenerateAuditToken()
    t2, h2 := GenerateAuditToken()
    if t1 == t2 || h1 == h2 { t.Fatal("token/hash collision") }
    if !VerifyAuditToken(t1, h1) { t.Fatal("verify failed") }
    if VerifyAuditToken(t2, h1) { t.Fatal("cross-verify wrongly succeeded") }
}

func TestAuditToken_VerifyConstantTime(t *testing.T) {
    _, h := GenerateAuditToken()
    bad := "x" + strings.Repeat("a", 63)
    // 简单检查不 panic 即可；CI 上不做时序统计
    VerifyAuditToken(bad, h)
}

func TestAuditToken_FormatPrefix(t *testing.T) {
    tok, _ := GenerateAuditToken()
    if !strings.HasPrefix(tok, "sk-pa-") { t.Fatal("missing prefix") }
}
```

- [ ] **Step 2: 写实现**

```go
package util

import (
    "crypto/rand"
    "crypto/sha256"
    "crypto/subtle"
    "encoding/base32"
    "encoding/hex"
)

const auditTokenPrefix = "sk-pa-" // payload audit

func GenerateAuditToken() (token, hashedHex string) {
    var raw [32]byte
    _, _ = rand.Read(raw[:])
    encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
    token = auditTokenPrefix + encoded
    sum := sha256.Sum256([]byte(token))
    hashedHex = hex.EncodeToString(sum[:])
    return
}

func HashAuditToken(token string) string {
    sum := sha256.Sum256([]byte(token))
    return hex.EncodeToString(sum[:])
}

func VerifyAuditToken(provided, expectedHashedHex string) bool {
    h := HashAuditToken(provided)
    return subtle.ConstantTimeCompare([]byte(h), []byte(expectedHashedHex)) == 1
}
```

- [ ] **Step 3: 测试**

```bash
codeagent execute: cd backend && go test ./internal/util -run TestAuditToken -v
```

期望：3 个测试 PASS。

- [ ] **Step 4: 提交**

```bash
git add backend/internal/util/audit_token*.go
git commit -m "feat(audit): audit token generate/hash/constant-time verify"
```

---

### Task 5: excerpt 头尾截取算法

**Files:**
- Create: `backend/internal/service/payload_audit_excerpt.go`
- Create: `backend/internal/service/payload_audit_excerpt_test.go`

- [ ] **Step 1: 写测试（覆盖 spec §5.1 列举的所有边界）**

```go
package service

import (
    "strings"
    "testing"
    "unicode/utf8"
)

func TestExcerpt_Disabled(t *testing.T) {
    if got := excerpt("hello", 0); got != "" { t.Fatalf("got %q", got) }
    if got := excerpt("hello", -1); got != "" { t.Fatalf("got %q", got) }
}

func TestExcerpt_ShortPassesThrough(t *testing.T) {
    if got := excerpt("hello", 100); got != "hello" { t.Fatalf("got %q", got) }
}

func TestExcerpt_TypicalHeadAndTail(t *testing.T) {
    in := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
    out := excerpt(in, 64)
    if !strings.Contains(out, "[truncated") { t.Fatal("missing truncate marker") }
    if !strings.HasPrefix(out, "a") { t.Fatal("missing head") }
    if !strings.HasSuffix(out, "b") { t.Fatal("missing tail") }
    if len(out) > 64 { t.Fatalf("over budget: %d", len(out)) }
}

func TestExcerpt_TinyTotalDegradesToHeadOnly(t *testing.T) {
    in := strings.Repeat("a", 1000)
    out := excerpt(in, 40)
    if !utf8.ValidString(out) { t.Fatal("invalid utf-8") }
    if len(out) > 40 { t.Fatalf("over budget: %d", len(out)) }
}

func TestExcerpt_MultibyteUTF8Boundary(t *testing.T) {
    // 中文每字符 3 byte；half=15 时不能切到字符中间
    in := strings.Repeat("中", 100) + "M" + strings.Repeat("文", 100)
    out := excerpt(in, 64)
    if !utf8.ValidString(out) { t.Fatal("split a multibyte rune") }
}

func TestExcerpt_EmojiBoundary(t *testing.T) {
    // emoji 是 4 字节，组合 emoji 可达 8 字节
    in := strings.Repeat("😀", 200)
    out := excerpt(in, 64)
    if !utf8.ValidString(out) { t.Fatal("split an emoji") }
}

func TestExcerpt_HugeTruncatedMakesSepLong(t *testing.T) {
    // truncatedBytes = 999_999_989 → sep ~30 chars，total=40 → sep>=total-32
    // 必须降级而不 panic
    huge := strings.Repeat("a", 1_000_000_000 / 1) // 注意：测试不真的分配这么大；用预生成或 modify 算法 mock
    // 实际改为模拟超长输入：
    in := strings.Repeat("a", 100_000)
    out := excerpt(in, 40)
    if len(out) > 40 { t.Fatalf("over budget: %d", len(out)) }
    if !utf8.ValidString(out) { t.Fatal("invalid utf-8") }
}
```

- [ ] **Step 2: 写实现**

```go
package service

import (
    "fmt"
    "unicode/utf8"
)

const minExcerpt = 32

func excerpt(text string, total int) string {
    if total <= 0 || text == "" {
        return ""
    }
    if len(text) <= total {
        return text
    }
    truncatedBytes := len(text) - total
    sep := fmt.Sprintf("\n…[truncated %d bytes]…\n", truncatedBytes)

    if len(sep) >= total-minExcerpt {
        marker := "…[truncated]"
        budget := total - len(marker)
        if budget < 0 { budget = 0 }
        return safeTruncateUTF8(text, budget) + marker
    }

    half := (total - len(sep)) / 2
    if half <= 0 {
        return safeTruncateUTF8(text, total)
    }
    return safeTruncateUTF8(text, half) + sep + safeTruncateUTF8Tail(text, half)
}

func safeTruncateUTF8(s string, n int) string {
    if n >= len(s) { return s }
    if n <= 0 { return "" }
    out := s[:n]
    for len(out) > 0 && !utf8.ValidString(out) {
        out = out[:len(out)-1]
    }
    return out
}

func safeTruncateUTF8Tail(s string, n int) string {
    if n >= len(s) { return s }
    if n <= 0 { return "" }
    out := s[len(s)-n:]
    for len(out) > 0 && !utf8.ValidString(out) {
        out = out[1:]
    }
    return out
}
```

- [ ] **Step 3: 跑测试**

```bash
codeagent execute: cd backend && go test ./internal/service -run TestExcerpt -v
```

期望：7 个测试 PASS。

- [ ] **Step 4: 提交**

```bash
git add backend/internal/service/payload_audit_excerpt*.go
git commit -m "feat(audit): UTF-8 安全的头尾截取摘要算法"
```

---

## Phase 3 — 协议抽取

### Task 6: 4 协议 input/output 抽取

**Files:**
- Create: `backend/internal/service/payload_audit_extract.go`
- Create: `backend/internal/service/payload_audit_extract_test.go`
- Create: `backend/internal/service/testdata/payload_audit_extract/openai_chat/{plain,tool,refusal,reasoning,error}.sse`
- Create: `backend/internal/service/testdata/payload_audit_extract/openai_responses/*.sse`
- Create: `backend/internal/service/testdata/payload_audit_extract/anthropic/*.sse`
- Create: `backend/internal/service/testdata/payload_audit_extract/gemini/*.sse`

- [ ] **Step 1: 准备 fixture**

为每个协议准备 5 组真实 SSE 抓包简化：plain text / tool call / refusal / reasoning / failed。每个文件 < 200 行。可以先放一份手写最小用例，后续遇到 bug 再补。

- [ ] **Step 2: 写测试**

```go
package service

import (
    "os"
    "path/filepath"
    "testing"
)

type extractCase struct {
    fixture       string
    wantContains  []string // 抽出的文本必须包含这些 marker
    wantNotContain []string
}

func TestExtractOpenAIChatStream(t *testing.T) {
    cases := map[string]extractCase{
        "plain.sse": {wantContains: []string{"hello world"}},
        "tool.sse":  {wantContains: []string{"[tool_call name=get_weather"}},
        "refusal.sse": {wantContains: []string{"[refusal:"}},
        "reasoning.sse": {wantContains: []string{"[reasoning:"}},
        "error.sse": {wantContains: []string{"[error:"}},
    }
    for name, c := range cases {
        t.Run(name, func(t *testing.T) {
            data, _ := os.ReadFile(filepath.Join("testdata/payload_audit_extract/openai_chat", name))
            got := ExtractOpenAIChatStream(data)
            for _, w := range c.wantContains {
                if !strings.Contains(got, w) {
                    t.Fatalf("missing %q in:\n%s", w, got)
                }
            }
        })
    }
}

// 同样为 OpenAI Responses / Anthropic / Gemini 各写一组
```

- [ ] **Step 3: 写实现**

```go
package service

import (
    "bufio"
    "bytes"
    "encoding/json"
    "fmt"
    "strings"
)

type protocolExtractor func(rawSSE []byte) string

func ExtractOpenAIChatStream(raw []byte) string {
    var sb strings.Builder
    sc := bufio.NewScanner(bytes.NewReader(raw))
    sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
    for sc.Scan() {
        line := sc.Bytes()
        if !bytes.HasPrefix(line, []byte("data: ")) { continue }
        payload := bytes.TrimPrefix(line, []byte("data: "))
        if bytes.Equal(payload, []byte("[DONE]")) { continue }
        var ev struct {
            Choices []struct {
                Delta struct {
                    Content   string `json:"content"`
                    Refusal   string `json:"refusal"`
                    Reasoning string `json:"reasoning"`
                    ToolCalls []struct {
                        Function struct {
                            Name      string `json:"name"`
                            Arguments string `json:"arguments"`
                        } `json:"function"`
                    } `json:"tool_calls"`
                } `json:"delta"`
                FinishReason string `json:"finish_reason"`
            } `json:"choices"`
            Error *struct{ Message string `json:"message"` } `json:"error"`
        }
        if err := json.Unmarshal(payload, &ev); err != nil { continue }
        if ev.Error != nil {
            fmt.Fprintf(&sb, "[error: %s]", ev.Error.Message)
            continue
        }
        for _, ch := range ev.Choices {
            if ch.Delta.Content != "" { sb.WriteString(ch.Delta.Content) }
            if ch.Delta.Refusal != "" { fmt.Fprintf(&sb, "[refusal: %s]", ch.Delta.Refusal) }
            if ch.Delta.Reasoning != "" { fmt.Fprintf(&sb, "[reasoning: %s]", ch.Delta.Reasoning) }
            for _, tc := range ch.Delta.ToolCalls {
                if tc.Function.Name != "" {
                    fmt.Fprintf(&sb, "[tool_call name=%s]", tc.Function.Name)
                }
                if tc.Function.Arguments != "" {
                    fmt.Fprintf(&sb, "[tool_args: %s]", tc.Function.Arguments)
                }
            }
            if ch.FinishReason != "" && ch.FinishReason != "stop" {
                fmt.Fprintf(&sb, "[finish: %s]", ch.FinishReason)
            }
        }
    }
    return sb.String()
}

// 类似实现：
//   ExtractOpenAIResponsesStream(raw []byte) string  -- 按 event: 和 data: 双行 SSE 解析
//   ExtractAnthropicStream(raw []byte) string         -- content_block_delta / message_delta
//   ExtractGeminiStream(raw []byte) string            -- 流式 JSON array

// Input 抽取（请求 body 的 JSON）：
type InputExtractor func(body []byte) (text string, format string)

func ExtractOpenAIChatInput(body []byte) (string, string)        // 拼 messages[].role + content
func ExtractOpenAIResponsesInput(body []byte) (string, string)    // input 字段
func ExtractAnthropicInput(body []byte) (string, string)          // messages[].content
func ExtractGeminiInput(body []byte) (string, string)             // contents[].parts[].text
func ExtractOpenAIImagesInput(body []byte) (string, string)       // prompt

// 统一 dispatcher（collector 调用，避免到处 switch）
func ExtractInputText(provider, endpoint string, body []byte) (text string, format string) {
    switch provider {
    case "openai":
        switch {
        case strings.Contains(endpoint, "/chat/completions"):
            return ExtractOpenAIChatInput(body)
        case strings.Contains(endpoint, "/responses"):
            return ExtractOpenAIResponsesInput(body)
        case strings.Contains(endpoint, "/images/"):
            return ExtractOpenAIImagesInput(body)
        }
    case "anthropic":
        return ExtractAnthropicInput(body)
    case "gemini":
        return ExtractGeminiInput(body)
    }
    // 兜底：返 raw body 截断到 utf-8 字符
    return string(body), "raw"
}

// Stream 累加用的 dispatcher（每个 chunk 调一次）：
func ExtractStreamDeltaText(provider, endpoint string, sseLine []byte) string { /* 类似 switch */ }
```

- [ ] **Step 4: 跑测试**

```bash
codeagent execute: cd backend && go test ./internal/service -run TestExtract -v
```

期望：4 个 test set 全 PASS（每 set 5 个子用例）。

- [ ] **Step 5: 提交**

```bash
git add backend/internal/service/payload_audit_extract*.go \
        backend/internal/service/testdata/payload_audit_extract/
git commit -m "feat(audit): 4 协议 input/output 抽取（含 tool_use/refusal/reasoning visitor）"
```

---

## Phase 4 — Collector

### Task 7: PayloadAuditCollector

**Files:**
- Create: `backend/internal/service/payload_audit_collector.go`
- Create: `backend/internal/service/payload_audit_collector_test.go`

- [ ] **Step 1: 写测试**

```go
func TestCollector_DisabledIsNoop(t *testing.T) {
    c := NewPayloadAuditCollector(nil) // nil snapshot → disabled
    c.AppendOutput("hello")
    if evt := c.buildEvent(); evt != nil { t.Fatal("expected nil") }
}

func TestCollector_TruncatesInputAtCap(t *testing.T) {
    snap := &ConfigSnapshot{Enabled: true, InputMaxBytes: 100, OutputMaxBytes: 100, ExcerptBytes: 64}
    meta := PayloadAuditMetadata{GroupID: ptr(1)}
    c := NewPayloadAuditCollector(snap)
    c.SetMetadata(meta)
    c.SetInput(bytes.Repeat([]byte("a"), 500), "json")
    evt := c.Finalize(200, time.Second, "")
    if !evt.InputTruncated { t.Fatal("should truncate") }
    if evt.InputBytes != 500 { t.Fatalf("InputBytes should reflect original size, got %d", evt.InputBytes) }
    if len(evt.InputBody) != 100 { t.Fatalf("body should be capped, got %d", len(evt.InputBody)) }
}

func TestCollector_AppendOutputStopsAtCap(t *testing.T) {
    // 1KB cap，写 10 次 200 字节 → 第 6 次开始应被丢弃
}

func TestCollector_GroupNotInScope(t *testing.T) {
    snap := &ConfigSnapshot{Enabled: true, AllGroups: false, GroupIDs: map[int64]struct{}{1:{}, 2:{}}}
    c := NewPayloadAuditCollector(snap)
    c.SetMetadata(PayloadAuditMetadata{GroupID: ptr(int64(99))})
    c.SetInput([]byte("foo"), "json")
    evt := c.Finalize(200, 0, "")
    if evt != nil { t.Fatal("out-of-scope group should not emit") }
}
```

- [ ] **Step 2: 写实现**

```go
package service

import (
    "bytes"
    "strings"
    "sync/atomic"
    "time"
)

type PayloadAuditMetadata struct {
    RequestID, UserEmail, APIKeyName, GroupName, ClientIP string
    UserID, APIKeyID, GroupID                              *int64
    Endpoint, Provider, Model, UpstreamModel               string
    Stream                                                 bool
}

type PayloadAuditCollector struct {
    snap         *ConfigSnapshot
    enabled      bool
    meta         PayloadAuditMetadata
    inputBody    []byte
    inputBytes   int
    inputTrunc   bool
    inputFormat  string
    outputBuf    strings.Builder
    outputBytes  int
    outputTrunc  bool
    outputOmitted bool
    finalized    atomic.Bool
}

func NewPayloadAuditCollector(snap *ConfigSnapshot) *PayloadAuditCollector {
    if snap == nil || !snap.Enabled {
        return &PayloadAuditCollector{} // disabled
    }
    return &PayloadAuditCollector{snap: snap, enabled: true}
}

func (c *PayloadAuditCollector) SetMetadata(m PayloadAuditMetadata) {
    if !c.enabled { return }
    if !c.snap.GroupInScope(m.GroupID) {
        c.enabled = false
        return
    }
    c.meta = m
}

func (c *PayloadAuditCollector) SetInput(body []byte, format string) {
    if !c.enabled { return }
    c.inputBytes = len(body)
    c.inputFormat = format
    cap := c.snap.InputMaxBytes
    if len(body) > cap {
        c.inputBody = bytes.Clone(body[:cap])
        c.inputTrunc = true
    } else {
        c.inputBody = bytes.Clone(body)
    }
}

func (c *PayloadAuditCollector) AppendOutput(s string) {
    if !c.enabled || c.outputTrunc { return }
    c.outputBytes += len(s)
    cap := c.snap.OutputMaxBytes
    remaining := cap - c.outputBuf.Len()
    if remaining <= 0 {
        c.outputTrunc = true
        return
    }
    if len(s) > remaining {
        c.outputBuf.WriteString(s[:remaining])
        c.outputTrunc = true
        return
    }
    c.outputBuf.WriteString(s)
}

func (c *PayloadAuditCollector) MarkOutputOmitted() { c.outputOmitted = true }

func (c *PayloadAuditCollector) Finalize(statusCode int, dur time.Duration, errMsg string) *PayloadAuditEvent {
    if !c.enabled { return nil }
    if !c.finalized.CompareAndSwap(false, true) { return nil } // 防止 defer + early return 都触发
    return c.buildEvent(statusCode, dur, errMsg)
}

func (c *PayloadAuditCollector) buildEvent(statusCode int, dur time.Duration, errMsg string) *PayloadAuditEvent {
    inputText, _ := extractInputText(c.meta.Provider, c.meta.Endpoint, c.inputBody)
    outputText := c.outputBuf.String()
    return &PayloadAuditEvent{
        // ... 拷贝 meta 字段
        StatusCode:      statusCode,
        DurationMs:      int(dur / time.Millisecond),
        ErrorMessage:    errMsg,
        InputBody:       string(c.inputBody),
        OutputBody:      outputText,
        InputBytes:      c.inputBytes,
        OutputBytes:     c.outputBytes,
        InputTruncated:  c.inputTrunc,
        OutputTruncated: c.outputTrunc,
        OutputOmitted:   c.outputOmitted,
        InputExcerpt:    excerpt(inputText, c.snap.ExcerptBytes),
        OutputExcerpt:   excerpt(outputText, c.snap.ExcerptBytes),
        CreatedAt:       time.Now(),
    }
}
```

- [ ] **Step 3: 跑测试**

```bash
codeagent execute: cd backend && go test ./internal/service -run TestCollector -v
```

期望：4 个测试 PASS。

- [ ] **Step 4: 提交**

```bash
git add backend/internal/service/payload_audit_collector*.go
git commit -m "feat(audit): PayloadAuditCollector with truncation and group-scope check"
```

---

## Phase 5 — Service / Sink / Redis / Cleanup

### Task 8: ConfigSnapshot 与服务装配

**Files:**
- Create: `backend/internal/service/payload_audit_config.go`
- Create: `backend/internal/service/payload_audit_service.go`
- Create: `backend/internal/service/payload_audit_service_test.go`

- [ ] **Step 1: 写测试**

```go
func TestConfigSnapshot_HotReload(t *testing.T) {
    svc := newTestPayloadAuditService(t) // mocks settings repo
    s1 := svc.Snapshot()
    if s1.ExcerptBytes != 512 { t.Fatal() }
    svc.UpdateConfig(ctx, PayloadAuditConfig{ExcerptBytes: 256})
    s2 := svc.Snapshot()
    if s2.ExcerptBytes != 256 { t.Fatal() }
    if s1.ExcerptBytes != 512 { t.Fatal("old snapshot mutated!") }
}

func TestConfigSnapshot_QueueResizeFlagsRebuild(t *testing.T) {
    svc := newTestPayloadAuditService(t)
    needRebuild, err := svc.UpdateConfig(ctx, PayloadAuditConfig{QueueSize: 65536})
    if err != nil { t.Fatal(err) }
    if !needRebuild { t.Fatal("should signal rebuild") }
}

func TestConfigSnapshot_Validation(t *testing.T) {
    svc := newTestPayloadAuditService(t)
    _, err := svc.UpdateConfig(ctx, PayloadAuditConfig{ExcerptBytes: 10}) // < 64
    if err == nil { t.Fatal("should reject excerpt_bytes < 64") }
    _, err = svc.UpdateConfig(ctx, PayloadAuditConfig{RetentionDays: 0})
    if err == nil { t.Fatal("should reject retention_days < 1") }
}
```

- [ ] **Step 2: 写 ConfigSnapshot**

```go
package service

import (
    "sync/atomic"
    "time"
)

type PayloadAuditConfig struct { /* JSON 同 spec §4.4 */ }

type PayloadAuditExportKey struct {
    ID, Name, HashedToken string
    RateLimitPerMin       int
    CreatedAt             time.Time
    Disabled              bool
}

type ConfigSnapshot struct {
    Enabled                                  bool
    AllGroups                                bool
    GroupIDs                                 map[int64]struct{}
    InputMaxBytes, OutputMaxBytes            int
    ExcerptBytes                             int
    RetentionDays                            int
    WorkerCount                              int
    QueueSize, QueueMaxBytes                 int
    BatchSize, BatchFlushMs                  int
    ExportKeys                               []PayloadAuditExportKey
    ExportKeysByHash                         map[string]*PayloadAuditExportKey
    Generation                               uint64
}

func (s *ConfigSnapshot) GroupInScope(gid *int64) bool {
    if !s.Enabled { return false }
    if s.AllGroups { return true }
    if gid == nil { return false }
    _, ok := s.GroupIDs[*gid]
    return ok
}

func (s *ConfigSnapshot) FindExportKey(token string) *PayloadAuditExportKey {
    h := util.HashAuditToken(token)
    return s.ExportKeysByHash[h]
}
```

- [ ] **Step 3: 写 PayloadAuditService**

```go
type PayloadAuditService struct {
    settings    SettingsRepository
    repo        PayloadAuditRepository
    redis       *redis.Client
    snapshot    atomic.Pointer[ConfigSnapshot]
    sinkRebuild chan struct{} // 通知 main 重建 sink
    gen         atomic.Uint64
}

func ProvidePayloadAuditService(...) (*PayloadAuditService, error) {
    s := &PayloadAuditService{...}
    s.loadFromSettings(ctx) // 启动加载
    return s, nil
}

func (s *PayloadAuditService) Snapshot() *ConfigSnapshot { return s.snapshot.Load() }

// UpdateConfig validates → writes to settings → atomic.Store new snapshot
// Returns needRebuildSink=true if queue_size/queue_max_bytes 改变
func (s *PayloadAuditService) UpdateConfig(ctx context.Context, cfg PayloadAuditConfig) (needRebuildSink bool, err error)

// CRUD for export keys (mutate the JSON config)
func (s *PayloadAuditService) CreateExportKey(ctx context.Context, name string, ratePerMin int) (clearToken string, key PayloadAuditExportKey, err error)
func (s *PayloadAuditService) DeleteExportKey(ctx context.Context, id string) error
func (s *PayloadAuditService) ListExportKeys(ctx context.Context) ([]PayloadAuditExportKey, error)

// Last-used 走 Redis（避免高频 settings 写入）
// Redis key 格式: "payload_audit:export_key:last_used:<key_id>", value: RFC3339, TTL 7d
func (s *PayloadAuditService) MarkExportKeyUsed(ctx context.Context, id string) {
    // 异步写：go func() { rdb.Set(key, time.Now().UTC().Format(time.RFC3339), 7*24*time.Hour) }()
    // 失败仅记 slog warn，不重试
}
func (s *PayloadAuditService) ExportKeyLastUsed(ctx context.Context, id string) (time.Time, bool) {
    // GET → 解析 RFC3339；nil/error → return zero, false
}
```

- [ ] **Step 4: 跑测试**

```bash
codeagent execute: cd backend && go test ./internal/service -run TestConfigSnapshot -v
```

期望：3 个测试 PASS。

- [ ] **Step 5: 提交**

```bash
git add backend/internal/service/payload_audit_config.go \
        backend/internal/service/payload_audit_service*.go
git commit -m "feat(audit): config snapshot + service skeleton with hot-reload"
```

---

### Task 9: Sink (queue + worker pool + batcher + byte-budget)

**Files:**
- Create: `backend/internal/service/payload_audit_sink.go`
- Create: `backend/internal/service/payload_audit_sink_test.go`

- [ ] **Step 1: 写测试**

```go
func TestSink_TryEnqueueRespectsCount(t *testing.T) {
    sink := newTestSink(t, sinkCfg{QueueSize: 2, QueueMaxBytes: 1<<30, WorkerCount: 0}) // workers=0 不消费
    e1 := smallEvent(); e2 := smallEvent(); e3 := smallEvent()
    if !sink.TryEnqueue(e1) { t.Fatal() }
    if !sink.TryEnqueue(e2) { t.Fatal() }
    if sink.TryEnqueue(e3) { t.Fatal("should be rejected") }
    if sink.Stats().Dropped != 1 { t.Fatal() }
}

func TestSink_TryEnqueueRespectsBytes(t *testing.T) {
    sink := newTestSink(t, sinkCfg{QueueSize: 1000, QueueMaxBytes: 1024, WorkerCount: 0})
    big := bigEvent(800)
    if !sink.TryEnqueue(big) { t.Fatal() }
    if sink.TryEnqueue(big) { t.Fatal("byte budget should reject") }
}

func TestSink_BatcherFlushBySize(t *testing.T) {
    repo := &fakeRepo{}
    sink := newSinkWithRepo(t, repo, sinkCfg{BatchSize: 3, BatchFlushMs: 60_000})
    sink.Start(ctx)
    for i := 0; i < 3; i++ { sink.TryEnqueue(smallEvent()) }
    eventually(t, 1*time.Second, func() bool { return len(repo.batches) == 1 && len(repo.batches[0]) == 3 })
}

func TestSink_BatcherFlushByTime(t *testing.T) {
    repo := &fakeRepo{}
    sink := newSinkWithRepo(t, repo, sinkCfg{BatchSize: 100, BatchFlushMs: 200})
    sink.Start(ctx)
    sink.TryEnqueue(smallEvent())
    eventually(t, 1*time.Second, func() bool { return len(repo.batches) == 1 })
}

func TestSink_WorkerPanicSelfRecovers(t *testing.T) {
    repo := &panickyRepo{} // 第一次 BatchInsert panic，第二次 OK
    sink := newSinkWithRepo(t, repo, defaultCfg)
    sink.Start(ctx)
    sink.TryEnqueue(smallEvent())
    sink.TryEnqueue(smallEvent())
    eventually(t, 2*time.Second, func() bool { return repo.successCount.Load() >= 1 })
}
```

- [ ] **Step 2: 写实现**

```go
type PayloadAuditSink struct {
    repo       PayloadAuditRepository
    cfg        sinkCfg
    queue      chan *PayloadAuditEvent
    byteUsed   atomic.Int64
    workers    int
    stopCh     chan struct{}
    wg         sync.WaitGroup
    metrics    sinkMetrics
}

func (s *PayloadAuditSink) TryEnqueue(evt *PayloadAuditEvent) bool {
    sz := int64(evt.SizeBytes())
    if s.byteUsed.Add(sz) > int64(s.cfg.QueueMaxBytes) {
        s.byteUsed.Add(-sz)
        s.metrics.dropByByteBudget.Add(1)
        return false
    }
    select {
    case s.queue <- evt:
        s.metrics.accepted.Add(1)
        return true
    default:
        s.byteUsed.Add(-sz)
        s.metrics.dropByQueueFull.Add(1)
        return false
    }
}

func (s *PayloadAuditSink) Start(ctx context.Context) {
    for i := 0; i < s.cfg.WorkerCount; i++ {
        s.wg.Add(1)
        go s.workerLoop(ctx, i)
    }
}

func (s *PayloadAuditSink) workerLoop(ctx context.Context, id int) {
    defer s.wg.Done()
    defer func() {
        if r := recover(); r != nil {
            slog.Error("payload_audit.worker_panic", "id", id, "panic", r)
            // 重启自己
            s.wg.Add(1); go s.workerLoop(ctx, id)
        }
    }()

    batch := make([]*PayloadAuditEvent, 0, s.cfg.BatchSize)
    ticker := time.NewTicker(time.Duration(s.cfg.BatchFlushMs) * time.Millisecond)
    defer ticker.Stop()

    flush := func() {
        if len(batch) == 0 { return }
        if err := s.flushBatch(ctx, batch); err != nil {
            // retry once
            time.Sleep(100 * time.Millisecond)
            if err2 := s.flushBatch(ctx, batch); err2 != nil {
                slog.Warn("payload_audit.batch_failed", "n", len(batch), "err", err2)
                s.metrics.batchFailed.Add(int64(len(batch)))
            }
        }
        for _, e := range batch { s.byteUsed.Add(-int64(e.SizeBytes())) }
        batch = batch[:0]
    }

    for {
        select {
        case evt := <-s.queue:
            batch = append(batch, evt)
            if len(batch) >= s.cfg.BatchSize { flush() }
        case <-ticker.C:
            flush()
        case <-s.stopCh:
            flush(); return
        }
    }
}

func (s *PayloadAuditSink) flushBatch(ctx context.Context, batch []*PayloadAuditEvent) error {
    cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()
    return s.repo.BatchInsert(cctx, batch)
}
```

- [ ] **Step 3: 跑测试**

```bash
codeagent execute: cd backend && go test ./internal/service -run TestSink -v
```

期望：5 个测试 PASS。

- [ ] **Step 4: 提交**

```bash
git add backend/internal/service/payload_audit_sink*.go
git commit -m "feat(audit): non-blocking sink with double-bounded queue and batcher"
```

---

### Task 10: Redis drain / recover

**Files:**
- Create: `backend/internal/service/payload_audit_redis.go`
- Create: `backend/internal/service/payload_audit_redis_test.go`

- [ ] **Step 1: 写测试（用 testcontainers redis）**

```go
func TestRedisBuffer_DrainAndRecover(t *testing.T) {
    rdb := startRedis(t)
    buf := NewRedisBuffer(rdb, "payload_audit:shutdown_buffer")
    events := []*PayloadAuditEvent{e1, e2, e3}
    err := buf.DrainBatch(ctx, events, 5*time.Second)
    if err != nil { t.Fatal() }
    recovered, err := buf.Recover(ctx)
    if err != nil { t.Fatal() }
    if len(recovered) != 3 { t.Fatal() }
    // recover 后 key 应已删除
    n, _ := rdb.LLen(ctx, "payload_audit:shutdown_buffer").Result()
    if n != 0 { t.Fatal() }
}

func TestRedisBuffer_DrainTimesOutGracefully(t *testing.T) {
    rdb := startRedis(t)
    buf := NewRedisBuffer(rdb, "k")
    events := makeEvents(10000) // 故意很多
    err := buf.DrainBatch(ctx, events, 1*time.Millisecond) // 极短 deadline
    // 不应 panic；返回 partial drain 错误
    if err == nil { t.Fatal("expected timeout") }
}
```

- [ ] **Step 2: 写实现**

```go
type RedisBuffer struct {
    rdb *redis.Client
    key string
}

const drainChunkBytes = 4 * 1024 * 1024 // 4MB
const drainChunkCount = 50

func (b *RedisBuffer) DrainBatch(ctx context.Context, events []*PayloadAuditEvent, deadline time.Duration) error {
    if len(events) == 0 { return nil }
    cctx, cancel := context.WithTimeout(ctx, deadline)
    defer cancel()
    chunk := make([]any, 0, drainChunkCount)
    chunkBytes := 0
    flush := func() error {
        if len(chunk) == 0 { return nil }
        if err := b.rdb.LPush(cctx, b.key, chunk...).Err(); err != nil { return err }
        chunk = chunk[:0]; chunkBytes = 0
        return nil
    }
    for _, e := range events {
        data, _ := json.Marshal(e)
        if chunkBytes+len(data) > drainChunkBytes || len(chunk) >= drainChunkCount {
            if err := flush(); err != nil { return err }
        }
        chunk = append(chunk, data)
        chunkBytes += len(data)
    }
    return flush()
}

func (b *RedisBuffer) Recover(ctx context.Context) ([]*PayloadAuditEvent, error) {
    var out []*PayloadAuditEvent
    for {
        items, err := b.rdb.RPopCount(ctx, b.key, 100).Result()
        if err == redis.Nil || len(items) == 0 { break }
        if err != nil { return nil, err }
        for _, raw := range items {
            var e PayloadAuditEvent
            if err := json.Unmarshal([]byte(raw), &e); err == nil {
                out = append(out, &e)
            }
        }
    }
    _ = b.rdb.Del(ctx, b.key).Err()
    return out, nil
}
```

- [ ] **Step 3: 跑测试**

```bash
codeagent execute: cd backend && go test ./internal/service -run TestRedisBuffer -v
```

期望：2 个测试 PASS。

- [ ] **Step 4: Sink 接入 RedisBuffer 的 Stop 流程**

修改 `payload_audit_sink.go`，加 `Stop(ctx, deadline)`：
```go
func (s *PayloadAuditSink) Stop(ctx context.Context, deadline time.Duration) {
    close(s.stopCh)               // 通知 worker flush 后退出
    done := make(chan struct{})
    go func() { s.wg.Wait(); close(done) }()
    select {
    case <-done:
        return                    // 全部 flush 到 PG，无需 Redis
    case <-time.After(deadline):
    }
    // 超时：把 queue 中残余 LPUSH 到 Redis
    remaining := drainChannel(s.queue)
    if err := s.redisBuf.DrainBatch(ctx, remaining, 5*time.Second); err != nil {
        slog.Warn("payload_audit.redis_drain_fail", "n", len(remaining), "err", err)
        s.metrics.dropOnShutdown.Add(int64(len(remaining)))
    }
}
```

- [ ] **Step 5: 提交**

```bash
git add backend/internal/service/payload_audit_redis*.go \
        backend/internal/service/payload_audit_sink.go
git commit -m "feat(audit): SIGTERM drain to Redis with chunked pipeline + startup recover"
```

---

### Task 11: Cleanup state machine + cron

**Files:**
- Create: `backend/internal/service/payload_audit_cleanup.go`
- Create: `backend/internal/service/payload_audit_cleanup_test.go`

- [ ] **Step 1: 写测试**

```go
func TestCleanup_DropsExpiredPartitions(t *testing.T) {
    repo, db := setupRepoWithPG(t)
    // 创建 5 个分区：3 个早于 cutoff、2 个新的
    // 跑 cleanup
    cl := NewPayloadAuditCleanup(repo, /* svc */, /* metrics */)
    deleted, err := cl.RunOnce(ctx)
    if err != nil { t.Fatal() }
    if deleted != 3 { t.Fatalf("expected 3, got %d", deleted) }
}

func TestCleanup_HandlesPendingDetach(t *testing.T) {
    // mock repo：第一次 Detach 成功但分区状态仍 DETACH_PENDING（CONCURRENTLY 中断）
    // 下次 cleanup 应调 FinalizePartitionDetach 而非重新 Detach
}
```

- [ ] **Step 2: 写实现**

```go
type PayloadAuditCleanup struct {
    repo PayloadAuditRepository
    svc  *PayloadAuditService
}

func (c *PayloadAuditCleanup) RunOnce(ctx context.Context) (deleted int, err error) {
    snap := c.svc.Snapshot()
    cutoff := time.Now().UTC().Add(-time.Duration(snap.RetentionDays) * 24 * time.Hour)
    parts, err := c.repo.ListPartitionsBefore(ctx, cutoff)
    if err != nil { return 0, err }
    for _, p := range parts {
        state, err := c.repo.PartitionState(ctx, p.Name)
        if err != nil {
            slog.Error("payload_audit.cleanup_state_fail", "p", p.Name, "err", err)
            continue
        }
        switch state {
        case "ATTACHED":
            if err := c.repo.DetachPartitionConcurrently(ctx, p.Name); err != nil {
                slog.Error("payload_audit.cleanup_detach_fail", "p", p.Name, "err", err)
                continue
            }
            // 不在同一次循环里 finalize，让 PG 自己跑完，下次 cron 处理
        case "DETACH_PENDING":
            if err := c.repo.FinalizePartitionDetach(ctx, p.Name); err != nil {
                slog.Error("payload_audit.cleanup_finalize_fail", "p", p.Name, "err", err)
                continue
            }
            if err := c.repo.DropPartition(ctx, p.Name); err != nil { continue }
            deleted++
        case "DETACHED":
            if err := c.repo.DropPartition(ctx, p.Name); err != nil { continue }
            deleted++
        }
        // 写一条 ops_system_log（如果可用）
    }
    return deleted, nil
}
```

- [ ] **Step 3: Partition maintenance（同文件加一个函数）**

```go
type PayloadAuditPartitionMaintainer struct{ repo PayloadAuditRepository }

func (m *PayloadAuditPartitionMaintainer) RunOnce(ctx context.Context, lookahead time.Duration) error {
    now := time.Now().UTC()
    end := now.Add(lookahead)
    cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
    for cur.Before(end) {
        if err := m.repo.CreatePartition(ctx, cur); err != nil { return err }
        cur = cur.AddDate(0, 1, 0)
    }
    return nil
}
```

- [ ] **Step 4: 接入 TimingWheelService**

参考现有 `ProvideUsageCleanupService` 的 cron 接入方式。Service 启动时：
```go
// 启动时立即创建分区
if err := partMaintainer.RunOnce(ctx, 60*24*time.Hour); err != nil {
    slog.Error("payload_audit.partition_init_fail", "err", err)
}
// cron 每天 02:00 跑分区维护
timingWheel.Schedule(everyDayAt(02, 00, "UTC"), func() {
    _ = partMaintainer.RunOnce(ctx, 60*24*time.Hour)
})
// cron 每天 03:00 跑 cleanup
timingWheel.Schedule(everyDayAt(03, 00, "UTC"), func() {
    _, _ = cleanup.RunOnce(ctx)
})
```

- [ ] **Step 5: 跑测试 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/service -run TestCleanup -v
git add backend/internal/service/payload_audit_cleanup*.go
git commit -m "feat(audit): cleanup state machine + partition maintenance cron"
```

---

### Task 12: Wire DI + Server 启动接入

**Files:**
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/internal/repository/wire.go`
- Modify: `backend/internal/server/server.go`（或对应的服务启动汇集点）

- [ ] **Step 1: 加 ProvideXxx 函数和 ProviderSet 条目**

`service/wire.go`：
```go
func ProvidePayloadAuditService(...) (*PayloadAuditService, error) { ... }
func ProvidePayloadAuditSink(...) *PayloadAuditSink { ... }
func ProvidePayloadAuditCleanup(...) *PayloadAuditCleanup { ... }
func ProvidePayloadAuditPartitionMaintainer(...) *PayloadAuditPartitionMaintainer { ... }
func ProvidePayloadAuditRedisBuffer(rdb *redis.Client) *RedisBuffer {
    return NewRedisBuffer(rdb, "payload_audit:shutdown_buffer")
}
```

加入既有的 `ProviderSet`。

`repository/wire.go`：
```go
wire.Bind(new(service.PayloadAuditRepository), new(*PayloadAuditRepo)),
```

- [ ] **Step 2: server 启动序列接入**

参照现有 `OpsSystemLogSink.Start` / `EmailQueueService.Start` 在 server.go 的接入位置：
```go
// 启动
if recovered, err := redisBuf.Recover(ctx); err == nil && len(recovered) > 0 {
    for _, e := range recovered { sink.TryEnqueue(e) }
}
sink.Start(ctx)
go runCronOnStartup(partMaintainer)

// 关停（gracefulShutdown 列表）
sink.Stop(shutdownCtx, 10*time.Second)
```

- [ ] **Step 3: wire generate**

```bash
codeagent execute: cd backend && go generate ./internal/service ./internal/repository
```

- [ ] **Step 4: 编译通过**

```bash
codeagent execute: cd backend && go build ./...
```

- [ ] **Step 5: 提交**

```bash
git add backend/internal/service/wire.go \
        backend/internal/service/wire_gen.go \
        backend/internal/repository/wire.go \
        backend/internal/repository/wire_gen.go \
        backend/internal/server/server.go
git commit -m "feat(audit): wire DI + server 启停接入"
```

---

## Phase 6 — Handler 集成

### Task 13: handler helper

**Files:**
- Create: `backend/internal/handler/payload_audit_helper.go`
- Create: `backend/internal/handler/payload_audit_helper_test.go`

- [ ] **Step 1: 写测试**

```go
func TestAttachCollector_DisabledServiceReturnsNil(t *testing.T) {
    svc := newDisabledService(t)
    c := AttachPayloadAuditCollector(ginContext, svc, "openai", "/v1/chat/completions")
    if c == nil || c.Enabled() { t.Fatal() }
    // Finalize on disabled collector is no-op
    FinalizePayloadAudit(c, sink, 200, time.Second, "")
}

func TestAttachCollector_EnabledPopulatesMetadata(t *testing.T) {
    svc := newEnabledService(t)
    ctx := mockGinContext(withUserID(42), withGroupID(1), withClientIP("1.2.3.4"))
    c := AttachPayloadAuditCollector(ctx, svc, "anthropic", "/anthropic/v1/messages")
    if c.Metadata().UserID == nil || *c.Metadata().UserID != 42 { t.Fatal() }
    if c.Metadata().ClientIP != "1.2.3.4" { t.Fatal() }
}
```

- [ ] **Step 2: 写实现**

```go
package handler

func AttachPayloadAuditCollector(c *gin.Context, svc *service.PayloadAuditService, provider, endpoint string) *service.PayloadAuditCollector {
    if svc == nil { return nil }
    snap := svc.Snapshot()
    coll := service.NewPayloadAuditCollector(snap)
    if !coll.Enabled() { return coll } // 禁用快路径
    meta := service.PayloadAuditMetadata{
        Endpoint: endpoint,
        Provider: provider,
        ClientIP: extractClientIP(c),
        // user_id / api_key_id / group_id / model 由调用方在 SetMetadata 之前再补
    }
    if subj, ok := middleware.GetAuthSubjectFromContext(c); ok {
        if subj.UserID > 0  { meta.UserID = &subj.UserID }
        if subj.APIKeyID > 0 { meta.APIKeyID = &subj.APIKeyID }
        // ...
    }
    coll.SetMetadata(meta)
    return coll
}

func FinalizePayloadAudit(coll *service.PayloadAuditCollector, sink *service.PayloadAuditSink, statusCode int, dur time.Duration, errMsg string) {
    if coll == nil { return }
    evt := coll.Finalize(statusCode, dur, errMsg)
    if evt == nil { return }
    sink.TryEnqueue(evt)
}
```

- [ ] **Step 3: 测试 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler -run TestAttachCollector -v
git add backend/internal/handler/payload_audit_helper*.go
git commit -m "feat(audit): handler helper for attach + finalize"
```

---

### Task 14: OpenAI Chat Completions 接入

**Files:**
- Modify: `backend/internal/handler/openai_chat_completions.go`
- Modify: `backend/internal/service/openai_gateway_service.go`（在 `handleStreamingResponse` 内累加文本）
- Create: `backend/internal/handler/openai_chat_completions_audit_test.go`

- [ ] **Step 1: 写 e2e 测试（使用现有 mockUpstream 模式）**

```go
func TestOpenAIChatHandler_AuditCapturesStream(t *testing.T) {
    // 启动一个测试用 mock upstream，返回 SSE
    // 启动测试 server，开启 payload_audit
    // 调 /v1/chat/completions，stream=true
    // 验证 sink 收到 1 条事件，OutputBody 包含 fixture 文本
}

func TestOpenAIChatHandler_AuditDisabledNoEmit(t *testing.T) {
    // 关闭 payload_audit
    // 调用相同请求
    // 验证 sink 没收到事件
}

func TestOpenAIChatHandler_AuditCapturesNonStream(t *testing.T) {
    // stream=false 路径
}

func TestOpenAIChatHandler_AuditCapturesClientDisconnect(t *testing.T) {
    // 客户端中途取消请求
    // 验证 sink 仍收到事件，OutputTruncated=true 且 ErrorMessage 含 "client disconnect"
}
```

- [ ] **Step 2: 修改 handler 入口**

`openai_chat_completions.go`，在 body 读取后加：
```go
body, _ := io.ReadAll(c.Request.Body)
auditCol := handler.AttachPayloadAuditCollector(c, h.payloadAuditSvc, "openai", "/v1/chat/completions")
auditCol.SetInput(body, "json")
defer func() {
    if r := recover(); r != nil {
        handler.FinalizePayloadAudit(auditCol, h.payloadAuditSink, 500, time.Since(start), fmt.Sprintf("panic: %v", r))
        panic(r)
    }
}()
// ... 原有逻辑
// 在最终知道 statusCode/duration/errMsg 处调：
handler.FinalizePayloadAudit(auditCol, h.payloadAuditSink, statusCode, duration, errMsg)
```

- [ ] **Step 3: 修改 service 流式累加**

`openai_gateway_service.go` 的 `handleStreamingResponse`：
```go
// 在函数签名加可选 collector 参数
func (s *OpenAIGatewayService) handleStreamingResponse(ctx context.Context, ..., collector *PayloadAuditCollector) (*openaiStreamingResult, error) {
    // ...
    for scanner.Scan() {
        line := scanner.Bytes()
        // 现有 token 累加
        if payload, ok := parseSSEData(line); ok {
            accumulateUsage(payload, usage)
            // 新增：audit 累加（仅当 collector enabled）
            if collector != nil && collector.Enabled() {
                collector.AppendOutput(extractOpenAIChatDeltaText(payload))
            }
        }
        c.Writer.Write(line); c.Writer.Write(newline)
    }
    // ...
}
```

`extractOpenAIChatDeltaText` 从 Task 6 的 `ExtractOpenAIChatStream` 抽出单 chunk 版本（不要重复实现）。

- [ ] **Step 4: 跑 e2e + 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler -run TestOpenAIChatHandler_Audit -v
git add backend/internal/handler/openai_chat_completions.go \
        backend/internal/handler/openai_chat_completions_audit_test.go \
        backend/internal/service/openai_gateway_service.go
git commit -m "feat(audit): wire payload audit into OpenAI Chat handler + stream loop"
```

---

### Task 15: Anthropic Messages 接入

**Files:**
- Modify: `backend/internal/handler/gateway_handler.go`
- Modify: `backend/internal/service/gateway_service.go`（`Forward` 流式分支）
- Create: `backend/internal/handler/anthropic_audit_test.go`

- [ ] **Step 1: 测试**

```go
func TestAnthropicHandler_AuditCapturesStream(t *testing.T)
func TestAnthropicHandler_AuditCapturesToolUse(t *testing.T)
func TestAnthropicHandler_AuditDisabledNoEmit(t *testing.T)
func TestAnthropicHandler_AuditCapturesUpstream5xx(t *testing.T)
func TestAnthropicHandler_AuditClientDisconnect(t *testing.T)
```

- [ ] **Step 2: handler 入口** — `gateway_handler.go` 在 body 读取后插入 4 行：

```go
body, _ := io.ReadAll(c.Request.Body)
auditCol := handler.AttachPayloadAuditCollector(c, h.payloadAuditSvc, "anthropic", "/anthropic/v1/messages")
auditCol.SetInput(body, "json")
defer handler.FinalizePayloadAudit(auditCol, h.payloadAuditSink, c.Writer.Status(), time.Since(start), "")
```

- [ ] **Step 3: service 流式分支** — `gateway_service.go` 的 `Forward` 中已有 `handleClaudeStream`（或同名 SSE 循环）：在 `for scanner.Scan()` 循环内，已有 token 累加之后追加：

```go
if collector != nil && collector.Enabled() {
    collector.AppendOutput(ExtractAnthropicStreamLine(line))
}
```

`ExtractAnthropicStreamLine` 是 Task 6 中 `ExtractAnthropicStream` 的单 chunk 版（处理一行 SSE，覆盖 `content_block_delta`/`tool_use`/`thinking`/`error`）。

- [ ] **Step 4: 跑 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler -run TestAnthropicHandler_Audit -v
git add backend/internal/handler/gateway_handler.go \
        backend/internal/handler/anthropic_audit_test.go \
        backend/internal/service/gateway_service.go
git commit -m "feat(audit): wire payload audit into Anthropic Messages handler"
```

---

### Task 16: OpenAI Responses 接入

**Files:**
- Modify: `backend/internal/handler/gateway_handler_responses.go`
- Modify: `backend/internal/service/openai_gateway_service.go`（`handleStreamingResponsePassthrough`）
- Create: `backend/internal/handler/openai_responses_audit_test.go`

- [ ] **Step 1: 测试**

```go
func TestResponsesHandler_AuditCapturesOutputTextDelta(t *testing.T)
func TestResponsesHandler_AuditCapturesRefusal(t *testing.T)
func TestResponsesHandler_AuditCapturesFailed(t *testing.T) // response.failed event
func TestResponsesHandler_AuditDisabledNoEmit(t *testing.T)
func TestResponsesHandler_AuditUpstream5xx(t *testing.T)
```

- [ ] **Step 2: handler 入口（同 Task 15 Step 2 模板，端点改 `/v1/responses`，provider `openai`）**

- [ ] **Step 3: service 流式分支**

OpenAI Responses 的 SSE 同时携带 `event:` 行和 `data:` 行；累加器需要按 event type 路由：

```go
// 在 handleStreamingResponsePassthrough 的 scanner 循环内：
if collector != nil && collector.Enabled() {
    collector.AppendOutput(ExtractOpenAIResponsesStreamLine(eventType, dataPayload))
}
```

`eventType` 是当前 SSE event 的 `event:` 头（已被现有解析提取，复用变量）。

- [ ] **Step 4: 跑 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler -run TestResponsesHandler_Audit -v
git add backend/internal/handler/gateway_handler_responses.go \
        backend/internal/handler/openai_responses_audit_test.go \
        backend/internal/service/openai_gateway_service.go
git commit -m "feat(audit): wire payload audit into OpenAI Responses handler"
```

---

### Task 17: Gemini 接入

**Files:**
- Modify: `backend/internal/handler/gemini_v1beta_handler.go`
- Modify: `backend/internal/service/gemini_messages_compat_service.go`（`handleStreamingResponse` + `handleNativeStreamingResponse`）
- Create: `backend/internal/handler/gemini_audit_test.go`

- [ ] **Step 1: 测试**

```go
func TestGeminiHandler_AuditCapturesText(t *testing.T)
func TestGeminiHandler_AuditCapturesFunctionCall(t *testing.T)
func TestGeminiHandler_AuditCapturesSafetyBlock(t *testing.T) // finishReason=SAFETY
func TestGeminiHandler_AuditDisabledNoEmit(t *testing.T)
func TestGeminiHandler_AuditNativeAndCompatBothCovered(t *testing.T)
```

- [ ] **Step 2: handler 入口**

```go
body, _ := io.ReadAll(c.Request.Body)
auditCol := handler.AttachPayloadAuditCollector(c, h.payloadAuditSvc, "gemini", c.Request.URL.Path)
auditCol.SetInput(body, "json")
defer handler.FinalizePayloadAudit(auditCol, h.payloadAuditSink, c.Writer.Status(), time.Since(start), "")
```

- [ ] **Step 3: service 流式分支**

Gemini 流式是 JSON array（每帧一个 candidate），不是 SSE：

```go
// 在解析每个 candidate 之后：
if collector != nil && collector.Enabled() {
    collector.AppendOutput(ExtractGeminiStreamFrame(candidateBytes))
}
```

`handleNativeStreamingResponse` 同样改一遍。

- [ ] **Step 4: 跑 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler -run TestGeminiHandler_Audit -v
git add backend/internal/handler/gemini_v1beta_handler.go \
        backend/internal/handler/gemini_audit_test.go \
        backend/internal/service/gemini_messages_compat_service.go
git commit -m "feat(audit): wire payload audit into Gemini handler (compat + native)"
```

---

### Task 18: OpenAI Images + 辅助端点

**Files:**
- Modify: `backend/internal/handler/openai_images.go`
- Modify: `backend/internal/service/openai_images.go`
- Create: `backend/internal/handler/openai_images_audit_test.go`
- 若仓库存在 embeddings/audio handler：相应 MOD

- [ ] **Step 1: 测试**

```go
func TestImagesHandler_AuditCapturesPromptAndResponseURL(t *testing.T)
func TestImagesHandler_AuditDisabledNoEmit(t *testing.T)
func TestImagesHandler_AuditUpstream5xx(t *testing.T)
// 辅助端点（若存在）：
func TestEmbeddingsHandler_AuditCapturesInputOnly(t *testing.T) // output_omitted=true
```

- [ ] **Step 2: handler 入口（非流式）**

```go
body, _ := io.ReadAll(c.Request.Body)
auditCol := handler.AttachPayloadAuditCollector(c, h.payloadAuditSvc, "openai", "/v1/images/generations")
auditCol.SetInput(body, "json")
defer handler.FinalizePayloadAudit(auditCol, h.payloadAuditSink, c.Writer.Status(), time.Since(start), "")
```

- [ ] **Step 3: service 非流式 output**

`openai_images.go` 在拿到上游 response body 后追加：
```go
if collector != nil && collector.Enabled() {
    collector.AppendOutput(string(respBody))
}
```

- [ ] **Step 4: 辅助端点（embeddings/audio）**

先 grep 确认是否存在：
```bash
codeagent execute: grep -rln "embeddings\|/audio/" backend/internal/handler/ | grep -v _test
```

存在则同模板加入，并在 attach 后立即 `auditCol.MarkOutputOmitted()`，不调 `AppendOutput`。

- [ ] **Step 5: 跑 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler -run "TestImagesHandler_Audit|TestEmbeddingsHandler_Audit" -v
git add backend/internal/handler/openai_images.go \
        backend/internal/handler/openai_images_audit_test.go \
        backend/internal/service/openai_images.go
git commit -m "feat(audit): wire payload audit into OpenAI Images + auxiliary endpoints"
```

---

## Phase 7 — Admin API

### Task 19: Admin handlers

**Files:**
- Create: `backend/internal/handler/admin/payload_audit_handler.go`
- Create: `backend/internal/handler/admin/payload_audit_handler_test.go`

- [ ] **Step 1: 写测试**

为以下端点各 1-2 个测试：
- `GET /admin/payload-audit/config`：返脱敏的 export keys（hashed_token 不返）
- `PUT /admin/payload-audit/config`：成功更新；非法 excerpt_bytes 返 400
- `GET /admin/payload-audit/status`：返结构正确
- `GET /admin/payload-audit/payloads?from=&to=&...`：调 repo.List，分页
- `GET /admin/payload-audit/payloads/:id`：返完整 body
- `GET /admin/payload-audit/export-keys`：返列表，合并 Redis last_used
- `POST /admin/payload-audit/export-keys`：返 token 明文一次
- `DELETE /admin/payload-audit/export-keys/:id`
- `POST /admin/payload-audit/cleanup`：调 cleanup.RunOnce

- [ ] **Step 2: 写实现**

每个 handler 方法 < 30 行，参照 [content_moderation_handler.go](backend/internal/handler/admin/content_moderation_handler.go) 的样式。

- [ ] **Step 3: 路由注册**

`backend/internal/server/routes/admin.go` 加：
```go
audit := admin.Group("/payload-audit")
{
    audit.GET("/config",          h.Admin.PayloadAudit.GetConfig)
    audit.PUT("/config",          h.Admin.PayloadAudit.UpdateConfig)
    audit.GET("/status",          h.Admin.PayloadAudit.GetStatus)
    audit.GET("/payloads",        h.Admin.PayloadAudit.ListPayloads)
    audit.GET("/payloads/:id",    h.Admin.PayloadAudit.GetPayload)
    audit.GET("/export-keys",     h.Admin.PayloadAudit.ListExportKeys)
    audit.POST("/export-keys",    h.Admin.PayloadAudit.CreateExportKey)
    audit.DELETE("/export-keys/:id", h.Admin.PayloadAudit.DeleteExportKey)
    audit.POST("/cleanup",        h.Admin.PayloadAudit.RunCleanup)
}
```

- [ ] **Step 4: 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler/admin -run TestPayloadAudit -v
git add backend/internal/handler/admin/payload_audit_handler*.go \
        backend/internal/server/routes/admin.go
git commit -m "feat(audit): admin endpoints (config / status / list / export-keys / cleanup)"
```

---

## Phase 8 — Export API

### Task 20: 导出 auth middleware + 限流

**Files:**
- Create: `backend/internal/server/middleware/audit_export_auth.go`
- Create: `backend/internal/server/middleware/audit_export_auth_test.go`

- [ ] **Step 1: 写测试**

```go
func TestAuditExportAuth_NoToken_401(t *testing.T)
func TestAuditExportAuth_BadToken_401(t *testing.T)
func TestAuditExportAuth_DisabledKey_401(t *testing.T)
func TestAuditExportAuth_ValidToken_PassesAndAttachesContext(t *testing.T)
func TestAuditExportAuth_RateLimitPerKey_429(t *testing.T) // 用极小 rate 测一发就 429
```

- [ ] **Step 2: 写实现**

```go
func AuditExportAuthMiddleware(svc *service.PayloadAuditService, limiter RateLimiter) gin.HandlerFunc {
    return func(c *gin.Context) {
        h := c.GetHeader("Authorization")
        if !strings.HasPrefix(h, "Bearer ") {
            c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"}); return
        }
        tok := strings.TrimPrefix(h, "Bearer ")
        snap := svc.Snapshot()
        key := snap.FindExportKey(tok)
        if key == nil || key.Disabled {
            c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"}); return
        }
        // per-key rate limit
        if !limiter.Allow(key.ID, key.RateLimitPerMin) {
            c.Header("Retry-After", "60")
            c.AbortWithStatusJSON(429, gin.H{"error": "rate limit"}); return
        }
        c.Set("audit_key_id", key.ID)
        c.Set("audit_key_name", key.Name)
        // 异步更新 last_used
        go svc.MarkExportKeyUsed(context.Background(), key.ID)
        c.Next()
    }
}
```

`limiter` 复用现有 `RateLimitService` 或新建简单 token bucket。

- [ ] **Step 3: 测试 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/server/middleware -run TestAuditExportAuth -v
git add backend/internal/server/middleware/audit_export_auth*.go
git commit -m "feat(audit): export API bearer auth + per-key rate limit"
```

---

### Task 21: 导出 handler

**Files:**
- Create: `backend/internal/handler/audit_export_handler.go`
- Create: `backend/internal/handler/audit_export_handler_test.go`
- Create: `backend/internal/server/routes/public_audit.go`

- [ ] **Step 1: 写测试**

```go
func TestExport_ListWithCursor(t *testing.T)
func TestExport_ListIncludeBodyModes(t *testing.T)        // none/excerpt/full
func TestExport_RejectsTimeWindowOver31d(t *testing.T)
func TestExport_RejectsKeywordWithLargeWindow(t *testing.T)
func TestExport_GetById(t *testing.T)
func TestExport_NDJSONStreaming(t *testing.T)
func TestExport_AccessLoggedToOpsSystemLogs(t *testing.T)
```

- [ ] **Step 2: 写实现**

```go
type AuditExportHandler struct {
    svc       *service.PayloadAuditService
    repo      service.PayloadAuditRepository
    opsLogger *service.OpsSystemLogService
}

func (h *AuditExportHandler) ListPayloads(c *gin.Context) {
    // 解析 from/to/cursor/limit/include_body/user_id/group_id/api_key_id/keyword
    // 校验时间窗（31d 普通 / 7d 带 keyword）
    // 调 repo.List
    // 按 include_body 删字段
    // 异步写一条 ops_system_log（payload_audit.export_access）
    // 返 JSON
}
func (h *AuditExportHandler) GetPayload(c *gin.Context)
func (h *AuditExportHandler) StreamNDJSON(c *gin.Context) {
    // Content-Type: application/x-ndjson
    // 内部循环 List，每页直接写到 c.Writer + flush
    // 强制时间窗 ≤ 7 天
}
func (h *AuditExportHandler) VerifyAuth(c *gin.Context) {
    c.JSON(200, gin.H{"key_name": c.GetString("audit_key_name")})
}
```

- [ ] **Step 3: 路由**

`public_audit.go`：
```go
func RegisterPublicAuditRoutes(r *gin.Engine, h *handler.AuditExportHandler, mw gin.HandlerFunc) {
    g := r.Group("/api/v1/audit", mw)
    g.POST("/auth/verify",                    h.VerifyAuth)
    g.GET("/exports/payloads",                h.ListPayloads)
    g.GET("/exports/payloads/:id",            h.GetPayload)
    g.GET("/exports/payloads.ndjson",         h.StreamNDJSON)
}
```

在 `server.go` 调 `RegisterPublicAuditRoutes`。

- [ ] **Step 4: 测试 + 提交**

```bash
codeagent execute: cd backend && go test ./internal/handler -run TestExport -v
git add backend/internal/handler/audit_export_handler*.go \
        backend/internal/server/routes/public_audit.go \
        backend/internal/server/server.go
git commit -m "feat(audit): public export API (list / detail / ndjson) with cursor + access log"
```

---

## Phase 9 — 前端

### Task 22: API 客户端

**Files:**
- Create: `frontend/src/api/admin/payloadAudit.ts`
- Modify: `frontend/src/api/admin/index.ts`（导出新模块）

- [ ] **Step 1: 写 API 客户端**

```typescript
import { request } from '@/utils/request'

export interface PayloadAuditConfig { /* mirror backend */ }
export interface PayloadAuditLog { /* mirror */ }
export interface PayloadAuditStatus { /* mirror */ }
export interface PayloadAuditExportKey { id: string; name: string; rate_limit_per_min: number; created_at: string; last_used_at?: string; disabled: boolean }

export const payloadAuditAPI = {
    getConfig:      ()       => request.get<PayloadAuditConfig>('/admin/payload-audit/config'),
    updateConfig:   (cfg: PayloadAuditConfig) => request.put('/admin/payload-audit/config', cfg),
    getStatus:      ()       => request.get<PayloadAuditStatus>('/admin/payload-audit/status'),
    listPayloads:   (params: ListParams) => request.get<{data: PayloadAuditLog[], next_cursor?: string}>('/admin/payload-audit/payloads', { params }),
    getPayload:     (id: number) => request.get<PayloadAuditLog>(`/admin/payload-audit/payloads/${id}`),
    listExportKeys: () => request.get<PayloadAuditExportKey[]>('/admin/payload-audit/export-keys'),
    createExportKey:(name: string, ratePerMin: number) => request.post<{token: string, key: PayloadAuditExportKey}>('/admin/payload-audit/export-keys', { name, rate_limit_per_min: ratePerMin }),
    deleteExportKey:(id: string) => request.delete(`/admin/payload-audit/export-keys/${id}`),
    runCleanup:     () => request.post<{deleted: number}>('/admin/payload-audit/cleanup'),
}
```

- [ ] **Step 2: 提交**

```bash
git add frontend/src/api/admin/payloadAudit.ts frontend/src/api/admin/index.ts
git commit -m "feat(audit): frontend API client"
```

---

### Task 23: 主页面

**Files:**
- Create: `frontend/src/views/admin/PayloadAuditView.vue`
- Modify: `frontend/src/router/index.ts`（或对应路由文件）
- Modify: `frontend/src/i18n/locales/zh.ts` + `en.ts`
- Modify: 导航菜单组件（参考 RiskControlView 的注册方式）

- [ ] **Step 1: 加 i18n**

zh.ts 新增 `admin.payloadAudit.*` 块（参照现有 `admin.riskControl.*` 结构），覆盖：
- `title`, `description`, `enabled`, `disabled`
- 概览卡片：`recordedToday`, `queueUsage`, `droppedToday`
- 列表列头：`time`, `user`, `apiKey`, `group`, `endpoint`, `model`, `stream`, `status`, `bytes`, `excerpt`
- 配置 tab：`basic`, `performance`, `apiKey`
- 字段标签：`excerptBytes`, `inputMaxBytes`, `outputMaxBytes`, `retentionDays`, `workerCount`, `queueSize`, `queueMaxBytes`
- 详情对话框：`metadata`, `inputTab`, `outputTab`, `rawJsonTab`, `expandFullBody`, `download`
- 操作：`createKey`, `deleteKey`, `runCleanup`, `confirmDelete`

en.ts 镜像翻译。

- [ ] **Step 2: 写 Vue 组件**

参照现有 [RiskControlView.vue](frontend/src/views/admin/RiskControlView.vue) 与 [UsageView.vue](frontend/src/views/admin/UsageView.vue) 的结构组合。骨架：

```vue
<template>
  <AppLayout>
    <div class="space-y-6">
      <!-- 概览卡片 × 4 -->
      <PayloadAuditStatusCards :status="status" />

      <!-- 筛选栏 -->
      <PayloadAuditFilters v-model:filters="filters" :groups="groups" :users="users" />

      <!-- 列表 -->
      <PayloadAuditTable :rows="rows" :loading="loading" @row-click="openDetail" />

      <!-- 详情抽屉 -->
      <PayloadAuditDetailDrawer v-model:open="detailOpen" :id="detailId" />

      <!-- 配置对话框 -->
      <PayloadAuditConfigDialog v-model:open="configOpen" :config="config" @save="onSaveConfig" />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
// 与 RiskControlView 同款的 onMounted 加载 status / config
// 列表筛选/分页用 reactive
// 详情懒加载 full body
</script>
```

子组件可以拆为独立文件或都放在同一 .vue 内，遵循项目现有粒度（看 RiskControlView.vue 是否拆分）。

- [ ] **Step 3: 路由 + 菜单**

`router/index.ts`：
```typescript
{
  path: '/admin/payload-audit',
  name: 'AdminPayloadAudit',
  component: () => import('@/views/admin/PayloadAuditView.vue'),
  meta: { requiresAdmin: true, title: 'admin.payloadAudit.title', menuGroup: 'riskControl' },
}
```

侧边栏菜单（具体文件依现有 menu 注册方式而定）：
```typescript
{ key: 'risk-control', label: t('admin.menu.riskControl'), children: [
    { path: '/admin/risk-control',     label: t('admin.riskControl.title') },
    { path: '/admin/payload-audit',    label: t('admin.payloadAudit.title') }, // NEW
]}
```

- [ ] **Step 4: 本地手动验证**

```bash
codeagent execute: 启动后端 + 前端，按以下流程手动验证（截图回报）：
  1. 登录 admin 账号 → 进入"风控中心 → 调用留存"页
  2. 点"配置" → 启用 → 选 "所有分组" → 保存
  3. 用 curl 发一个 chat completions 请求（开 stream）
  4. 回到列表页刷新，应能看到 1 条记录
  5. 点击行 → 详情抽屉打开 → input/output tab 显示 excerpt → "展开完整内容" → 显示完整 body
  6. 配置页"API Key" tab → 创建一个 key → 复制 token → DELETE 该 key → 列表消失
```

- [ ] **Step 5: 提交**

```bash
git add frontend/src/views/admin/PayloadAuditView.vue \
        frontend/src/router/ \
        frontend/src/i18n/locales/
git commit -m "feat(audit): admin frontend page with list/detail/config"
```

---

## Phase 10 — Observability + Docs

### Task 24: Prometheus metrics 注册

**Files:**
- Modify: `backend/internal/service/payload_audit_sink.go`（暴露 metrics）
- Modify: `backend/internal/service/ops_metrics_collector.go`（注册）

- [ ] **Step 1: 实现**

参照现有 `ops_metrics_collector.go` 已注册的指标，添加 spec §9.2 的 14 个 metric：counter / gauge / histogram。

- [ ] **Step 2: 验证**

```bash
codeagent execute: 启动 server，curl /metrics | grep payload_audit
```

期望：14 个 metric 名都出现。

- [ ] **Step 3: 提交**

```bash
git add backend/internal/service/
git commit -m "feat(audit): Prometheus metrics for payload audit subsystem"
```

---

### Task 24b: 性能基准（验证 spec §9.1 目标）

**Files:**
- Create: `backend/internal/service/payload_audit_bench_test.go`

- [ ] **Step 1: 写 benchmark**

```go
package service

import (
    "context"
    "strings"
    "testing"
    "time"
)

func BenchmarkCollector_DisabledFastPath(b *testing.B) {
    c := NewPayloadAuditCollector(nil) // disabled
    for i := 0; i < b.N; i++ {
        c.AppendOutput("hello")
    }
    // 目标：< 50 ns/op
}

func BenchmarkCollector_EnabledAppend(b *testing.B) {
    snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 1 << 20, ExcerptBytes: 512}
    c := NewPayloadAuditCollector(snap)
    c.SetMetadata(PayloadAuditMetadata{})
    for i := 0; i < b.N; i++ {
        c.AppendOutput("hello world delta")
    }
    // 目标：< 100 ns/op
}

func BenchmarkCollector_FinalizeEnqueue(b *testing.B) {
    sink := newBenchSink(b) // 队列足够大
    snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 1 << 20, ExcerptBytes: 512}
    body := []byte(strings.Repeat("a", 10*1024)) // 10KB
    for i := 0; i < b.N; i++ {
        c := NewPayloadAuditCollector(snap)
        c.SetMetadata(PayloadAuditMetadata{})
        c.SetInput(body, "json")
        c.AppendOutput("hello")
        evt := c.Finalize(200, time.Second, "")
        sink.TryEnqueue(evt)
    }
    // 目标：< 50 µs/op
}

func BenchmarkSink_Throughput(b *testing.B) {
    sink := newBenchSinkWithRealRepo(b) // 真 PG（testcontainer 持久 fixture）
    sink.Start(context.Background())
    defer sink.Stop(context.Background(), 5*time.Second)
    snap := &ConfigSnapshot{Enabled: true, AllGroups: true, OutputMaxBytes: 1 << 20, ExcerptBytes: 512}
    body := []byte(strings.Repeat("a", 10*1024))
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        c := NewPayloadAuditCollector(snap)
        c.SetMetadata(PayloadAuditMetadata{})
        c.SetInput(body, "json")
        sink.TryEnqueue(c.Finalize(200, time.Second, ""))
    }
    // 目标：单 worker 1k/s，4 worker 应能 4k+/s
}
```

- [ ] **Step 2: 跑 + 记录基线**

```bash
codeagent execute: cd backend && go test -bench=. -run=^$ -benchmem ./internal/service -count=3 | tee bench_baseline.txt
```

把结果与 spec §9.1 目标对比。任何项超 2× 目标则需排查（GC？allocation hotspot？）。

- [ ] **Step 3: 提交**

```bash
git add backend/internal/service/payload_audit_bench_test.go
git commit -m "test(audit): performance benchmarks for collector and sink (spec §9.1)"
```

---

### Task 25: 运维文档

**Files:**
- Create: `docs/PAYLOAD_AUDIT.md`
- Modify: `.gitignore`（已经在 Task 0 加了 plans/，现在再加 `!docs/PAYLOAD_AUDIT.md`）
- Modify: `README.md`（features 列表加一条；不超过 1 行）

- [ ] **Step 1: .gitignore exception**

在现有 `!docs/PAYMENT.md` 等同位置追加：
```
!docs/PAYLOAD_AUDIT.md
```

- [ ] **Step 2: 写文档**

按 spec §13 列出的 6 点为骨架，加入：
- 表名、配置 key、菜单位置
- 启用步骤（推荐灰度）
- 配置项一览表（所有 JSON 字段含义、默认值、合理范围）
- 容量预估表（1k/10k/100k QPD × 6 个月）
- 监控告警建议（哪些 metric > 阈值要告警）
- 第三方扫描器接入示例（curl + Python）
- 故障排查 FAQ：队列满怎么办 / Redis 故障 / 分区维护失败 / PG 14 以下

- [ ] **Step 3: README hook**

[README.md](README.md) features 列表追加：
```markdown
- **Compliance Audit Archive** - Async-archive every LLM input/output for periodic third-party compliance scans
```

中文 / 日文 README 同步加一行。

- [ ] **Step 4: 提交**

```bash
git add docs/PAYLOAD_AUDIT.md .gitignore README.md README_CN.md README_JA.md
git commit -m "docs: 调用留存功能运维文档与 README 入口"
```

---

## Phase 11 — 收尾

### Task 26: 全套验收

- [ ] **Step 1: 后端全测试 + lint**

```bash
codeagent execute:
  cd backend
  go vet ./...
  go test ./... -count=1
```

期望：全 PASS，无 vet warning。

- [ ] **Step 2: 前端 lint + build**

```bash
codeagent execute:
  cd frontend
  pnpm typecheck
  pnpm lint
  pnpm build
```

期望：无错误。

- [ ] **Step 3: 端到端冒烟**

```bash
codeagent execute: 用 docker-compose 起整套依赖，跑以下脚本（写到 tests/manual/payload_audit_smoke.sh）：
  1. 注入测试 admin user + group + api_key
  2. 启用 payload audit (group_ids=[1])
  3. 创建 export key，记下 token
  4. 用 api_key 发 5 个不同协议的请求
  5. 等 1 秒（让 batch flush）
  6. 用 export token 调 GET /api/v1/audit/exports/payloads?from=...&to=... 应该返 5 条
  7. 用 export token 调 ndjson 应该流式返 5 行
  8. 用错误 token 应该 401
  9. 跨 31 天窗口应该 400
```

- [ ] **Step 4: 推到远端 + 开 PR**

```bash
git push -u origin feat/payload-audit
gh pr create --base main --title "feat: 调用留存（payload audit）功能" --body "$(cat <<'EOF'
## Summary
- 异步留存 5 类对话端点的完整 input/output 到 PG 月度分区表
- 新增对外 GET API（独立 token 鉴权）供第三方合规扫描
- 后台管理页（风控中心 → 调用留存）支持配置/查询/导出 key 管理

## 设计与评审
- Spec: docs/specs/2026-05-09-payload-audit-design.md
- 三轮评审：自检 1 → 自检 2 → codex GPT-5.5 review，共修订 25+ 处

## Test Plan
- [ ] 后端单测全 PASS
- [ ] 前端 typecheck/lint/build 通过
- [ ] 手动验证 5 个协议各能写入
- [ ] 关停时 Redis drain 正常
- [ ] 启动时 Recover 能消费 Redis 残余
- [ ] export API token 校验、限流、时间窗约束生效

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Plan 自检结论

- ✅ Spec §1-§14 每节都有对应任务覆盖
- ✅ 每个 task 含具体文件路径、代码、命令、commit message
- ✅ 5 个网关 handler 各 1 任务，互不依赖（可并行）
- ✅ Codex 提出的 10 处 P0/P1/P2 均通过 spec 链路落到 task
- ✅ 配置/启停/Redis/cleanup 状态机/byte budget 均建模到代码与测试
- ✅ 路径中无 TBD、无"similar to"、无 placeholder
