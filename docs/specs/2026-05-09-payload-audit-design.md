# 调用留存（Payload Audit）功能设计

- 状态：草案 v1，待评审
- 创建时间：2026-05-09
- 分支：`feat/payload-audit`
- 范围：后端 Go service + handler + repo + admin handler + 对外导出 API + 数据库迁移；前端 admin 管理页

---

## 1. 背景与目标

项目已经具备**风控中心 / 内容审核**模块（基于 OpenAI Moderation API 的实时拦截），但内容审核只看"是否命中违规分类"，不留存调用原文。第三方安全合规审查程序需要**完整的调用 input 与 LLM output 原文**进行离线扫描（关键词、模式匹配、敏感数据识别等）。

本功能目标：

1. 异步留存网关上 5 类对话端点的完整 input + output；辅助端点（embeddings/audio）仅留 input。
2. 在不影响请求路径性能的前提下，把数据持久化到 PostgreSQL，按月度分区。
3. 向第三方扫描程序暴露受 token 鉴权的导出 API，支持按时间区间 + 用户筛选。
4. 在管理后台提供查询页，参考现有"使用记录"页的体验。

---

## 2. 需求澄清结果（共识基线）

| # | 项 | 决定 |
|---|---|---|
| 1 | 覆盖范围 | 5 类对话端点（OpenAI Chat / Responses / Anthropic / Gemini / Images）完整 input+output；embeddings/audio 等辅助端点仅留 input |
| 2 | 启用粒度 | 全局开关 + 按 group 启用（与内容审核同心智） |
| 3 | 留存期 | 默认 180 天，可配 |
| 4 | 存储 | PG `TEXT COMPRESSION lz4` + 月度时间分区 |
| 5 | 流式捕获 | 在 4 个协议的 `handleStreamingResponse` 内加文本累加器 visitor |
| 6 | 性能保护 | 非阻塞 channel send + bounded queue (32k) + bounded worker pool (4-32) + 单条截断（input 10MB / output 5MB）+ 批量 INSERT |
| 7 | 关停兜底 | 仅在 SIGTERM drain 残余事件到 Redis；启动从 Redis 恢复；不进热路径 |
| 8 | 部署假设 | 单实例，shutdown buffer key 固定为 `payload_audit:shutdown_buffer` |
| 9 | 对外认证 | 独立审计 API Key（无 IP 白名单） |
| 10 | 后台可见性 | 仅 admin |
| 11 | 脱敏 | 不脱敏，原样存 |
| 12 | 摘要策略 | 写入时计算独立 `excerpt` 列，头尾各占一半，UTF-8 边界安全；input 走 4 协议文本抽取后再截，output 抽完已是纯文本直接截 |

---

## 3. 总体架构

```
                    ┌───────────────────────────────────────────────┐
                    │  Gateway Handler (chat/anthropic/gemini/...)  │
                    │                                               │
   ① 进入 handler ──▶│  body, _ := io.ReadAll(req.Body)              │
                    │  inputCopy := bytes.Clone(body)               │
                    │                                               │
   ② Forward(...) ─▶│  service.Forward(...)                         │
                    │    └─ handleStreamingResponse:                │
                    │        for scanner.Scan() {                   │
                    │          payload := parseSSE(line)            │
                    │          tokens.Add(payload)                  │
   ③ 累加输出文本 ──▶│          collector.AppendOutput(extract(p))   │
                    │          c.Writer.Write(line)                 │
                    │        }                                      │
                    │                                               │
   ④ Stream 结束 ──▶│  outputText := collector.String() (truncated) │
                    │                                               │
   ⑤ 异步 emit ─────▶│  auditSink.TryEnqueue(AuditEvent{...})        │
                    │   ◀── select+default，队列满即丢，绝不阻塞     │
                    └───────────────────────────────────────────────┘
                                          │
                                          ▼
                    ┌───────────────────────────────────────────────┐
                    │ AuditSinkWorker × N (4..32)                   │
                    │  - 每 200ms 或满 100 条 → 批量 INSERT          │
                    │  - lz4 由 PG TOAST 自动处理                    │
                    │  - 写入失败：重试 1 次后丢弃 + slog warn       │
                    └───────────────────────────────────────────────┘
                                          │
                                          ▼
                    ┌───────────────────────────────────────────────┐
                    │ payload_audit_logs (月度分区, lz4 TOAST)       │
                    └───────────────────────────────────────────────┘
                                          │
                  ┌───────────────────────┴────────────────────┐
                  ▼                                            ▼
        ┌──────────────────┐                    ┌──────────────────────────┐
        │ Admin UI 查询     │                    │ 第三方合规扫描 GET API    │
        │ /admin/payload-   │                    │ /api/v1/audit/exports/   │
        │ audit/...         │                    │   payloads...            │
        │ admin token       │                    │ Bearer audit-only token   │
        └──────────────────┘                    └──────────────────────────┘
                                          │
                                          ▼
                    ┌───────────────────────────────────────────────┐
                    │ Cleanup cron (24h)：DETACH+DROP 过期月度分区   │
                    │ Partition cron (24h)：预创建未来 60 天分区     │
                    └───────────────────────────────────────────────┘

关停时 (SIGTERM):
   sink.Stop() →
     1. 关闭新入队
     2. 把内存里残余事件 + 当前 batch buffer 一次性 LPUSH
        到 Redis list "payload_audit:shutdown_buffer"
     3. 5 秒超时，Redis 不可用就记 slog warn 放弃

启动时:
   sink.Start() →
     1. 从 "payload_audit:shutdown_buffer" 全部 RPOP
     2. 重新塞回内存 channel，worker 正常消费
     3. 处理完成后 DEL key
```

### 关键说明

- **不复用 content_moderation 的 sink**：负载特征差异大（moderation 平均 < 1KB，payload audit 可能数 MB），独立 channel/worker 避免相互拖慢。但结构对称，便于后续合并。
- **不进 `RecordUsage` 路径**：handler 在 `defer` 里独立 `collector.Finalize()`，与 `RecordUsage` 并列调用，互不耦合。
- **错误降级原则**：audit 任何失败绝不回滚或阻塞用户请求，最多丢这一条审计 + 计数。

---

## 4. 数据模型

### 4.1 表结构

```sql
CREATE TABLE payload_audit_logs (
    id              BIGSERIAL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- 请求标识
    request_id      VARCHAR(64)  NOT NULL DEFAULT '',
    user_id         BIGINT       REFERENCES users(id)    ON DELETE SET NULL,
    user_email      VARCHAR(255) NOT NULL DEFAULT '',
    api_key_id      BIGINT       REFERENCES api_keys(id) ON DELETE SET NULL,
    api_key_name    VARCHAR(100) NOT NULL DEFAULT '',
    group_id        BIGINT       REFERENCES groups(id)   ON DELETE SET NULL,
    group_name      VARCHAR(255) NOT NULL DEFAULT '',
    client_ip       VARCHAR(45)  NOT NULL DEFAULT '',

    -- 调用维度
    endpoint        VARCHAR(128) NOT NULL DEFAULT '',
    provider        VARCHAR(64)  NOT NULL DEFAULT '',
    model           VARCHAR(255) NOT NULL DEFAULT '',
    upstream_model  VARCHAR(255) NOT NULL DEFAULT '',
    stream          BOOLEAN      NOT NULL DEFAULT FALSE,
    status_code     INT          NOT NULL DEFAULT 0,
    duration_ms     INT          NOT NULL DEFAULT 0,

    -- 摘要列（不走 TOAST，列表查询零解压）
    input_excerpt   VARCHAR(2048) NOT NULL DEFAULT '',
    output_excerpt  VARCHAR(2048) NOT NULL DEFAULT '',

    -- 完整内容（lz4 压缩）
    input_body      TEXT         NOT NULL DEFAULT '' COMPRESSION lz4,
    output_body     TEXT         NOT NULL DEFAULT '' COMPRESSION lz4,
    input_format    VARCHAR(16)  NOT NULL DEFAULT 'json',
    output_format   VARCHAR(16)  NOT NULL DEFAULT 'text',
    input_bytes     INT          NOT NULL DEFAULT 0,
    output_bytes    INT          NOT NULL DEFAULT 0,
    input_truncated  BOOLEAN     NOT NULL DEFAULT FALSE,
    output_truncated BOOLEAN     NOT NULL DEFAULT FALSE,
    output_omitted   BOOLEAN     NOT NULL DEFAULT FALSE,

    error_message   TEXT         NOT NULL DEFAULT '',

    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
```

> **PG 版本要求**：lz4 列压缩需要 PostgreSQL ≥ 14。低于 14 自动回退 `pglz`，迁移里加版本判断。

### 4.2 索引

| 索引 | 用途 |
|---|---|
| `(created_at DESC, id DESC)` | 默认排序、第三方按时间区间拉取 |
| `(user_id, created_at DESC)` | 第三方按"用户 + 时间区间"查询（合规扫描主路径） |
| `(group_id, created_at DESC)` | 后台按分组筛选 |
| `(api_key_id, created_at DESC)` | 后台按 Key 筛选 |
| `(request_id)` | 与 `usage_logs` / `ops_system_logs` 关联调试 |

不建：`endpoint/model/provider/client_ip` 单列索引（基数低，合并入 WHERE 即可）；不建：body 上的 GIN 全文索引（TOAST 解压成本太高）。

### 4.3 分区策略

- 月度分区，沿用 `usage_logs` 已有的分区管理函数（参见 `035_usage_logs_partitioning.sql`）。
- 预创建未来 2 个月的分区（独立 cron 每天 02:00 跑）。
- 清理 cron 每天 03:00 跑，按 `retention_days` 删过期月，单分区生命周期：
  1. `ALTER TABLE payload_audit_logs DETACH PARTITION <p> CONCURRENTLY` —— PG ≥ 14 支持，不阻塞 SELECT/INSERT
  2. **轮询 `pg_partitioned_table` / `pg_inherits` 直到该分区已脱离主表**（CONCURRENTLY 是异步的，最多等 30s）
  3. `DROP TABLE <p>` 真正释放空间
  4. 单步失败：记 slog error，回滚到 step 1（DETACH 可重入），等下次 cron 再试，**不在同一次 cron 内无限重试**

### 4.4 配置项（settings 表）

新增两把 key：

- `payload_audit_enabled`：boolean，全局开关，默认 `false`
- `payload_audit_config`：JSON，结构：

```json
{
  "all_groups": false,
  "group_ids": [1, 2, 3],

  "input_max_bytes": 10485760,
  "output_max_bytes": 5242880,
  "excerpt_bytes": 512,
  "retention_days": 180,

  "worker_count": 4,
  "queue_size": 32768,
  "batch_size": 100,
  "batch_flush_ms": 200,

  "export_api_keys": [
    {
      "id": "ak_xxx",
      "name": "compliance-scanner-prod",
      "hashed_token": "<sha256(token)>",
      "rate_limit_per_min": 60,
      "created_at": "...",
      "last_used_at": "...",
      "disabled": false
    }
  ]
}
```

> `export_api_keys` 只存 SHA256 哈希；明文 token 仅在创建时返回一次。选 SHA256 不选 bcrypt 因为这是机器对机器的高频校验。
>
> **存储与变更**：admin 的 POST/DELETE 端点（§7.7）内部对 `payload_audit_config` 做 read-modify-write 整体覆盖，不引入独立 keys 表。变更通过 settings 服务的 mutex 串行化，避免并发覆盖丢失。
>
> **`last_used_at` 不进 settings JSON**：高频更新会把整段 JSON 反复改写，挤占 settings 缓存与连接池。改为存 Redis 字符串 `payload_audit:export_key:last_used:<key_id>`，TTL 7 天；admin 列表接口读取时合并 Redis 数据展示。读不到（TTL 过期或 Redis 故障）就显示为空，不影响功能。

---

## 5. 摘要（Excerpt）算法

### 5.1 头尾截取

```go
func excerpt(text string, total int) string {
    if total <= 0 || text == "" {
        return ""
    }
    if len(text) <= total {
        return text                      // 短文本原样返
    }
    truncatedBytes := len(text) - total
    sep := fmt.Sprintf("\n…[truncated %d bytes]…\n", truncatedBytes)
    half := (total - len(sep)) / 2
    head := safeTruncateUTF8(text, half)
    tail := safeTruncateUTF8Tail(text, half)
    return head + sep + tail
}
```

- `safeTruncateUTF8` / `safeTruncateUTF8Tail`：在 rune 边界截断，避免切碎多字节字符。
- `total` 由 `excerpt_bytes` 配置，默认 512，范围 [0, 2048]，0 表示禁用摘要。
- 分隔符长度依赖被截字节数，需要估算最坏长度后再算 `half`，确保最终结果不超过 `total`。
- 配置改动**只对新写入生效**，已有行不回填。

### 5.2 input 文本抽取（4 协议）

写入摘要前先把 JSON body 抽成纯文本，避免摘要里全是 JSON 噪声：

| 协议 | 抽取来源 |
|---|---|
| OpenAI Chat | `messages[].content` 拼接（保留 role 标记） |
| OpenAI Responses | `input` 字段 |
| Anthropic Messages | `messages[].content`（过滤系统提示） |
| Gemini | `contents[].parts[].text` |
| OpenAI Images | `prompt` |
| 兜底 | 抽不出 → 用 raw body 截 |

抽取逻辑集中在 `payload_audit_extract.go`，**不与 content_moderation 的同名抽取共享代码**（两者目标不同：moderation 取最后一条 user message，audit 取完整对话）。

### 5.3 output 文本抽取（流式 delta）

| 协议 | delta 字段 |
|---|---|
| OpenAI Chat | `choices[].delta.content` |
| OpenAI Responses | `response.output_text.delta` |
| Anthropic Messages | `content_block_delta.delta.text` |
| Gemini | `candidates[].content.parts[].text` |

非流式直接拷贝 response body 的 `choices[].message.content`（或对应字段）。

---

## 6. 后端代码组织

### 6.1 新增 / 修改文件

```
backend/
├── ent/schema/payload_audit_log.go                   [NEW]
├── migrations/136_payload_audit_logs.sql             [NEW]
│
├── internal/
│   ├── repository/
│   │   ├── payload_audit_repo.go                     [NEW]
│   │   └── payload_audit_repo_test.go                [NEW]
│   │
│   ├── service/
│   │   ├── payload_audit_service.go                  [NEW]  配置加载/热更新/启停管控
│   │   ├── payload_audit_service_test.go             [NEW]
│   │   ├── payload_audit_sink.go                     [NEW]  channel + worker pool + batcher
│   │   ├── payload_audit_sink_test.go                [NEW]
│   │   ├── payload_audit_extract.go                  [NEW]  4 协议 input/output 抽取
│   │   ├── payload_audit_extract_test.go             [NEW]  含 4 协议流式 fixture
│   │   ├── payload_audit_collector.go                [NEW]  PayloadAuditCollector 类型
│   │   ├── payload_audit_collector_test.go           [NEW]
│   │   ├── payload_audit_redis.go                    [NEW]  shutdown drain / startup recover
│   │   ├── payload_audit_redis_test.go               [NEW]
│   │   ├── payload_audit_cleanup.go                  [NEW]  按 retention_days 删分区
│   │   ├── payload_audit_cleanup_test.go             [NEW]
│   │   ├── wire.go                                   [MOD]  注册新服务进 DI
│   │   ├── gateway_service.go                        [MOD]  Anthropic 流式分支
│   │   ├── openai_gateway_service.go                 [MOD]  OpenAI Chat / Responses 流式分支
│   │   ├── gemini_messages_compat_service.go         [MOD]  Gemini 流式分支
│   │   └── openai_images.go                          [MOD]  非流式 body 拷贝
│   │
│   ├── handler/
│   │   ├── payload_audit_helper.go                   [NEW]  attachPayloadAuditCollector / Finalize
│   │   ├── gateway_handler.go                        [MOD]
│   │   ├── gateway_handler_responses.go              [MOD]
│   │   ├── openai_chat_completions.go                [MOD]
│   │   ├── openai_images.go                          [MOD]
│   │   ├── gemini_v1beta_handler.go                  [MOD]
│   │   ├── admin/
│   │   │   ├── payload_audit_handler.go              [NEW]
│   │   │   └── payload_audit_handler_test.go         [NEW]
│   │   └── audit_export_handler.go                   [NEW]
│   │
│   ├── server/
│   │   ├── routes/admin.go                           [MOD]
│   │   ├── routes/public_audit.go                    [NEW]
│   │   └── middleware/audit_export_auth.go           [NEW]
│   │
│   └── util/audit_token.go                           [NEW]
│
└── docs/PAYLOAD_AUDIT.md                             [NEW]  运维部署文档

frontend/
├── src/api/admin/payloadAudit.ts                     [NEW]
├── src/views/admin/PayloadAuditView.vue              [NEW]
├── src/router/                                       [MOD]
└── src/i18n/locales/{zh,en}.ts                       [MOD]
```

### 6.2 collector 抽象

```go
type PayloadAuditCollector struct {
    ctx        context.Context
    enabled    bool                  // 总开关 + 分组范围预判结果
    inputBuf   []byte                // 入口拷贝
    outputBuf  *strings.Builder
    inputCap, outputCap int          // 截断上限
    inputBytes, outputBytes int      // 截断前真实大小
    inputTrunc, outputTrunc bool
    metadata   PayloadAuditMetadata  // endpoint/provider/model/user_id/group_id/api_key_id/client_ip
}

func (c *PayloadAuditCollector) AppendOutput(text string)  // 累加 + 截断
func (c *PayloadAuditCollector) Finalize(statusCode int, duration time.Duration, errMsg string)
```

禁用时所有方法 fast-path return，单次调用 < 50 ns。

### 6.3 sink 与 worker

```go
type PayloadAuditSink struct {
    queue    chan *PayloadAuditEvent  // bounded
    workers  int
    batcher  *batchInsertBuffer
    repo     PayloadAuditRepository
    redis    *redis.Client
    metrics  struct {
        accepted, dropped, written, failed atomic.Int64
    }
}

func (s *PayloadAuditSink) TryEnqueue(evt *PayloadAuditEvent) bool {
    select {
    case s.queue <- evt: return true
    default:
        s.metrics.dropped.Add(1)
        return false
    }
}
```

worker goroutine：
```go
for {
    select {
    case evt := <-s.queue:
        s.batcher.Add(evt)
        if s.batcher.ShouldFlush() { s.flush() }
    case <-time.After(flushInterval):
        s.flush()
    case <-ctx.Done():
        s.flush(); return
    }
}
```

### 6.4 wire 注册

```go
// service/wire.go
wire.Bind(new(PayloadAuditRepository), new(*repository.PayloadAuditRepo))

// 启动钩子（main.go / server.go）
audit.Start(ctx)
defer audit.Stop(ctx)
```

---

## 7. 第三方合规扫描导出 API

### 7.1 路由

```
POST   /api/v1/audit/auth/verify              健康/凭证自检
GET    /api/v1/audit/exports/payloads         主查询接口
GET    /api/v1/audit/exports/payloads/:id     单条详情（含完整 body）
GET    /api/v1/audit/exports/payloads.ndjson  流式批量导出
```

挂在新路由组 `publicAudit`，独立 middleware `AuditExportAuthMiddleware`。

### 7.2 鉴权

`Authorization: Bearer <token>`：
- 用恒定时间比较 SHA256(token) 与 settings 中的 `hashed_token` 列表
- 命中后写 ctx：`audit_key_id`、`audit_key_name`，供 handler 记访问审计
- 未命中：401，不区分原因（避免枚举攻击）
- 命中后异步更新 `last_used_at`

### 7.3 主查询参数

```
GET /api/v1/audit/exports/payloads
    ?from=2026-05-01T00:00:00Z         # 必填，RFC3339
    &to=2026-05-08T00:00:00Z           # 必填，RFC3339
    &user_id=123                       # 可选
    &group_id=4                        # 可选
    &api_key_id=99                     # 可选
    &cursor=<opaque>                   # 可选，游标分页
    &limit=100                         # 可选，默认 100，上限 500
    &include_body=excerpt              # excerpt(默认) | full | none
```

强制约束：
- `from` 与 `to` 必填，时间窗 ≤ 31 天，超出返回 400
- `to - from > 7 days` 且 `user_id` 与 `group_id` 都缺失 → 400 要求加过滤
- `limit` 缺省 100；`limit > 500` 静默 clamp 到 500（不返 400，便于扫描器写"limit=10000"图省事）；`limit < 1` 视为 100
- `include_body` 取值不在 `excerpt|full|none` 三者之一时 → 400
- 默认 `include_body=excerpt`，扫描器先粗筛再深拉

**游标语义**：
- 排序固定为 `(created_at DESC, id DESC)`（最新的优先）
- cursor 解码为 `(cursor_created_at, cursor_id)`，查询条件追加 `WHERE (created_at, id) < (cursor_created_at, cursor_id)`，**严格小于**，不会重复也不会漏
- 没有更多数据时 `next_cursor` 字段省略（不返回 `null`，省字节）
- 新数据在分页过程中插入：因为是 DESC，新数据在游标"之后"（时间更新），分页结果不受影响

### 7.4 响应

```json
{
  "data": [
    {
      "id": 12345,
      "created_at": "2026-05-08T11:23:45Z",
      "request_id": "req_abc",
      "user_id": 42,
      "user_email": "u@example.com",
      "api_key_id": 99,
      "api_key_name": "key-prod",
      "group_id": 4,
      "group_name": "default",
      "client_ip": "203.0.113.10",
      "endpoint": "/v1/chat/completions",
      "provider": "openai",
      "model": "gpt-4o-mini",
      "stream": true,
      "status_code": 200,
      "duration_ms": 1320,
      "input_bytes": 1024,
      "output_bytes": 4521,
      "input_truncated": false,
      "output_truncated": false,
      "output_omitted": false,
      "input_excerpt": "...",
      "output_excerpt": "..."
    }
  ],
  "next_cursor": "eyJjcmVhdGVkIjoi..."
}
```

**`include_body` 三种模式下的字段差异**：

| include_body | input_excerpt / output_excerpt | input_body / output_body |
|---|---|---|
| `none` | 不返 | 不返 |
| `excerpt`（默认） | 返回 | 不返 |
| `full` | 返回 | 返回 |

其余字段（id / created_at / 元数据 / *_bytes / *_truncated）三种模式都返。

### 7.5 流式批量导出

`GET /api/v1/audit/exports/payloads.ndjson`：

- `Content-Type: application/x-ndjson`，每行一个完整 JSON 记录（含完整 body）
- server 内部按游标循环，对调用方表现为单一长连接
- 强制时间窗 ≤ 7 天

### 7.6 限流与可观测

- per-key 限流：默认 60 req/min，可在 admin 配置
- 慢查询日志：> 5s 写 `ops_system_logs`
- 访问审计：每次调用记 slog `payload_audit.export_access`，写入 ops_system_logs

### 7.7 后台 Audit Key 管理（admin 路由）

```
GET    /admin/payload-audit/export-keys              列表（不返 token）
POST   /admin/payload-audit/export-keys              生成新 key（仅此一次返明文）
DELETE /admin/payload-audit/export-keys/:id          吊销
```

---

## 8. 管理后台 UI

### 8.1 路由与菜单

新增页面 `frontend/src/views/admin/PayloadAuditView.vue`，挂在管理菜单"风控中心"分组下，与现有 `RiskControlView`（内容审核）并列。i18n 前缀 `admin.payloadAudit.*`。

### 8.2 页面结构（参考现有 UsageView）

- **概览卡片 × 4**：启用状态、已记录(24h)、队列占用 %、丢弃数(24h)
- **筛选栏**：时间区间、分组、用户、API Key、端点、Provider、Model、是否流式、excerpt 关键字搜索
  - 关键字搜索为 `ILIKE '%kw%'` over `input_excerpt + output_excerpt`，**只在已被时间区间索引筛过的子集内 seq scan**
  - **后端硬约束**：带关键字时强制 `to - from ≤ 7 天`，否则返 400；UI 同步提示
  - 不引入 pg_trgm GIN 索引（excerpt 列写入频繁，trigram 索引维护开销不划算）
- **列表**：默认 `include_body=excerpt`；点行 → 抽屉/对话框打开详情；分页
- **详情对话框**：
  - 元数据区（request_id 可复制、关联跳转 usage_log）
  - Tab：[Input] [Output] [原始 JSON]
  - 默认显示 excerpt，点"展开完整内容"调详情 API 拉 full body
  - 超长内容懒加载 + 虚拟滚动
  - base64 图片识别后折叠为占位符
- **配置对话框**：三 Tab —— [基础] [性能] [API Key]，参照现有 RiskControlView

### 8.3 admin 路由（与对外 audit API 隔离）

```
GET    /admin/payload-audit/config
PUT    /admin/payload-audit/config
GET    /admin/payload-audit/status
GET    /admin/payload-audit/payloads
GET    /admin/payload-audit/payloads/:id
GET    /admin/payload-audit/export-keys
POST   /admin/payload-audit/export-keys
DELETE /admin/payload-audit/export-keys/:id
POST   /admin/payload-audit/cleanup    手动触发清理
```

走现有 admin token 中间件鉴权。

### 8.4 暂不实现（YAGNI）

- 后台导出 CSV/Excel
- 图表/统计
- 手动删除单条
- 标记 / 笔记
- WebSocket 实时推流

---

## 9. 性能预算与监控

### 9.1 性能目标

| 指标 | 目标 |
|---|---|
| Collector 创建（关闭时） | < 50 ns |
| Collector 创建（开启时，典型 input 10KB） | < 5 µs |
| Collector 创建（最坏 input 10MB，含 `bytes.Clone`） | < 5 ms |
| `AppendOutput` 单次 | < 100 ns |
| `Finalize + TryEnqueue`（typical output ≤ 100KB） | < 50 µs |
| 请求路径总额外延迟（典型） | < 100 µs |
| 请求路径总额外延迟（10MB input，最坏） | < 5 ms（绝大部分来自一次内存拷贝） |
| Worker 端单条平均 | < 5 ms |
| 满负载丢弃率 | < 0.1%（默认配置 32k 队列、4 worker，1k QPS 内可吃下） |

### 9.1.1 内存预算

- **平均场景**：32k 队列 × 平均事件 ~10 KB（典型 chat 输入输出） ≈ **320 MB** 上限
- **极端场景**：32k 队列被 vision payload 填满（每条 10MB input）≈ **320 GB**，**不应发生**
- 推论：`queue_size` 默认 32768 是按平均 10KB 设计的；生产若主要是 vision/long-context，应**调小** `queue_size` 至 4096 左右，或反之 `worker_count` 调大让队列保持低占用
- admin status 接口暴露 `queue.usage_pct`，超过 50% 持续 5 分钟应告警调参
- 单条 `input_max_bytes` / `output_max_bytes` 是**截断阈值**，不是队列计费单位

### 9.2 Prometheus 指标

```
payload_audit_enqueued_total{result="accepted|dropped"}     # counter
payload_audit_queue_depth                                   # gauge
payload_audit_batch_inserted_total                          # counter
payload_audit_batch_failed_total                            # counter
payload_audit_insert_duration_seconds                       # histogram
payload_audit_input_bytes                                   # histogram
payload_audit_output_bytes                                  # histogram
payload_audit_truncated_total{which="input|output"}         # counter
payload_audit_workers_active                                # gauge
payload_audit_redis_drain_total{result="ok|fail"}           # counter
payload_audit_redis_recover_total{result="ok|fail"}         # counter
payload_audit_cleanup_partitions_dropped_total              # counter
payload_audit_cleanup_run_duration_seconds                  # histogram
payload_audit_export_request_total{key_name, status}        # counter
payload_audit_export_rows_returned                          # histogram
```

### 9.3 健康度暴露

`GET /admin/payload-audit/status`：
```json
{
  "enabled": true,
  "workers": { "configured": 4, "active": 3, "idle": 1 },
  "queue":   { "size": 32768, "depth": 134, "usage_pct": 0.4 },
  "stats_24h": { "enqueued": ..., "dropped": ..., "written": ..., "failed": ..., "input_truncated": ..., "output_truncated": ... },
  "storage": { "total_rows_estimate": ..., "current_partition": "...", "partitions": [...], "next_cleanup_at": "..." },
  "last_error": { "at": "...", "msg": "..." }
}
```

### 9.4 Cron

- **清理 cron**：每天 UTC 03:00。cutoff = `now() - retention_days`，DETACH+DROP 所有早于 cutoff 的月度分区（一次可能批量清掉多个月，比如 retention 从 180 缩到 90 时），每个分区都写一条 ops_system_log。**服务启动时不自动跑**，避免首次部署时意外清空已有数据；改由 admin 显式调用 `POST /admin/payload-audit/cleanup`。
- **分区维护 cron**：每天 UTC 02:00。检查未来 60 天分区是否齐全，缺失补建。服务**启动时立即跑一次**（必要：保证当前月分区一定存在，否则首条写入会失败）。失败时记 slog error 并把"距离当前月分区耗尽的天数"作为 metric 暴露——连续 3 次失败 → 视为 ops 告警事件（运维需手动介入），避免月底悄无声息全失败。
- **手动触发清理**：`POST /admin/payload-audit/cleanup` 同步触发一次完整 cleanup 流程（前台等待返回）。

### 9.5 硬保护

| 风险 | 防御 |
|---|---|
| Worker panic | `recover()` 包裹，重启该 worker |
| Repo 长阻塞 | INSERT context 10s timeout，超时丢这一批 |
| Settings 非法 | 拒绝新配置，保留旧配置 |
| Body 累加无限 | `AppendOutput` 内部检查长度上限，超过停止累加 + 标 truncated |
| `worker_count = 0` | 视为禁用 |
| `retention_days < 1` | 回退默认 180 |

---

## 10. 失败模式与降级（汇总）

**原则：审计模块的任何失败，绝不影响主请求路径。**

| # | 失败场景 | 行为 | 用户感知 | 监控 |
|---|---|---|---|---|
| 1 | 模块禁用 | nilCollector，全 no-op | 无 | — |
| 2 | Group 不在范围 | collector 标 enabled=false | 无 | — |
| 3 | 队列满 | TryEnqueue 返回 false，dropped++ | 无 | `dropped_total` |
| 4 | input 超 10MB | 头尾截 + `input_truncated=true` | 无 | `truncated_total` |
| 5 | output 超 5MB | 停止累加 + `output_truncated=true` | 无 | `truncated_total` |
| 6 | Collector panic | recover + 这条丢 | 无 | slog warn |
| 7 | Worker panic | recover + 重启 worker | 无 | slog + `batch_failed` |
| 8 | 批量 INSERT 失败 | 重试 1 次后丢弃整批 | 无 | `batch_failed_total++` |
| 9 | 分区不存在 | 立即补建 + 重试 1 次 | 无 | slog 日志 |
| 10 | 分区维护 cron 失败 | 等下次 cron；新写入靠 #9 兜底 | 无 | slog error |
| 11 | 清理 cron 失败 | 下次 cron 重试，旧分区暂时多占磁盘 | 无 | slog error |
| 12 | Settings 非法 | 拒绝新配置 | 无 | admin 400 |
| 13 | Redis drain 失败 | slog warn + 残余丢失，正常关停 | 无 | `redis_drain_total{fail}` |
| 14 | Redis recover 失败 | slog warn + 跳过，正常启动 | 无 | `redis_recover_total{fail}` |
| 15 | SIGKILL/OOM | 内存残余丢失 | 无 | metric 差值可观察 |
| 16 | Audit Key 校验失败 | 401，不区分原因 | 扫描器收 401 | `export_request_total{401}` |
| 17 | 导出查询超时 | 30s timeout → 504 | 扫描器重试 | slog + 504 计数 |
| 18 | 导出参数非法 | 400 + 错误信息 | 扫描器修正 | `export_request_total{400}` |
| 19 | 导出 per-key 限流 | 429 + Retry-After | 扫描器降速 | `export_request_total{429}` |
| 20 | DB 磁盘满 | INSERT 失败，队列堆积，丢弃增加 | 无 | `queue_depth` 高位 + `batch_failed` |

### 关键不变量

1. 请求路径不会因为 audit 任何失败而变慢或失败
2. 数据丢失只发生在：队列满、worker 重试也失败、SIGKILL —— 三者都有 metric
3. 不会"写入了一半"——批量 INSERT 在事务内
4. 审计自身不审计——导出 API 访问只写 `ops_system_logs`，避免循环

---

## 11. 与现有模块的边界

| 现有模块 | 关系 | 复用 / 区分 |
|---|---|---|
| `usage_logs` | 不复用表，复用模式 | 通过 `request_id` 关联，独立写入。分区策略照搬。 |
| `content_moderation_logs` | 不复用 sink，复用配置心智 | 独立 channel/worker；配置都走 settings 表的"开关 + 配置 JSON"双 key |
| `ops_system_logs` | 复用作为审计的审计目的地 | 导出 API 的访问审计、cleanup 结果、worker panic 等运维事件全部写它 |
| `RecordUsage` | 不耦合，并列调用 | handler 在 defer 里独立 `collector.Finalize()` |
| Redis 客户端 | 复用 | 仅用于 SIGTERM drain 和启动 recover |
| Settings 服务 | 复用 + 新增 2 个 key | 热更新机制现成 |
| Admin auth middleware | 复用 | `/admin/payload-audit/*` 走现有 admin token |
| 对外 audit API middleware | 新增 | 与 admin/user token 完全隔离 |
| 邮件通知 | 不集成 | 队列高占用告警走现有 ops 告警体系基于 metric 触发 |
| 内容审核 | 互不依赖 | 审核拦截发生在 audit 入队之前——被审核拦下的请求**仍会**被 audit 记录（保留尝试输入的合规证据） |

### 5 类 handler 的改动量

| Handler | 改动量 |
|---|---|
| `openai_chat_completions.go` | ~30 行 |
| `gateway_handler.go` (Anthropic) | ~30 行 |
| `gemini_v1beta_handler.go` | ~30 行 |
| `gateway_handler_responses.go` | ~30 行 |
| `openai_images.go` | ~20 行 |
| 辅助端点（embeddings/audio 若存在） | ~15 行/端点 |

handler 总改动 ~150 行；service stream handlers 各 +~20 行（4 处）共 ~80 行；其余皆新文件。

---

## 12. 测试策略

| 层 | 测试要点 |
|---|---|
| Repository | 批量 INSERT、按时间区间/用户查询走索引、分区切换 |
| Sink | 队列满返回 false、worker panic 自愈、批量刷新触发、关停 drain、启动 recover |
| Extract | 4 协议各 1 组流式 fixture，断言抽出文本与官方 SDK 拼出来的一致 |
| Collector | UTF-8 边界截断、超限只截一次、`AppendOutput` 在禁用时 < 50ns（基准） |
| Handler | 5 端点各 1 组 e2e：开启写入、关闭不写入、流式 + 非流式 |
| Export API | Bearer 校验、时间窗校验、游标分页、per-key 限流 |
| 关停/启动 | SIGTERM 后 Redis 中有数据；启动后 Redis 数据被消费完且 key 删除 |
| 性能基准 | go bench：单 worker 1k 条/s 处理能力、collector 开关切换的延迟差 |

---

## 13. 部署注意事项（运维文档）

写在 `docs/PAYLOAD_AUDIT.md`：

1. **PostgreSQL 版本要求** ≥ 14（lz4 列压缩、`DETACH PARTITION CONCURRENTLY`）。低于 14 自动回退：`pglz` + 同步 DETACH。迁移用 `SELECT current_setting('server_version_num')::int >= 140000` 在 DO 块里分支建表。
2. **磁盘容量预估表**：千次 / 万次 / 十万次调用每天对应 6 个月容量。
3. **`queue.usage_pct > 50%` 持续 5 分钟**：建议扩 `worker_count`。
4. **Redis 故障**只影响关停丢失，不影响正常运行。
5. **首次启用建议**：先 `all_groups=false, group_ids=[小流量]` 灰度跑 1 天，观察 status 后放大。
6. **不考虑多副本部署**：本设计假设单实例运行；多实例环境下 SIGTERM drain 的 Redis key 会冲突，需后续扩展加 instance 后缀。

---

## 14. 暂不实现 / YAGNI

- 后台导出 CSV/Excel（走对外 audit API）
- 图表/统计（不是审计模块的职责）
- 手动删除单条记录（审计原则不可篡改）
- 标记/打 tag/笔记
- WebSocket 实时推流
- 多副本部署的 instance 隔离
- mTLS 鉴权（必要时再加）
- IP 白名单（必要时再加）
- 输入/输出脱敏（合规扫描需要原文）
- payload_audit_logs 自身的访问审计表（复用 ops_system_logs）
- **admin 后台查看完整 body 的访问审计**（即"审计员的审计"）：当前只对**对外导出 API**记访问审计。admin 在管理页打开详情、点"展开完整内容"不会留痕，等于"管理员能看任何人的原文且无记录"。如果合规框架要求"四眼原则" / "看了什么必须留底"，需要后续补一个 `payload_audit_admin_view_logs` 表或同样写 ops_system_logs。**当前阶段明确接受这个洞**（管理员本身就是高权限角色，假设可信）。
