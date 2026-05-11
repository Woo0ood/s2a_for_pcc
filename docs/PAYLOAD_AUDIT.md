# 调用留存（Payload Audit）

## 概述

调用留存子系统异步记录网关 5 类对话端点（chat completions、completions、messages、responses、embeddings/audio 仅 input）的完整 input/output payload，用于事后合规审计与风控回溯。

核心设计：

- **异步无阻塞**：collector 将事件推入内存有界队列，不影响网关请求延迟
- **PG 月度分区表**存储，lz4 列压缩（PG 14+），180 天默认保留
- **第三方合规扫描器**通过 token 鉴权 GET API 拉取数据
- **后台风控中心 → 调用留存**页面可查询记录、调整配置

## 启用步骤

1. **数据库版本**：推荐 PostgreSQL 14+（支持 lz4 列压缩）；< 14 自动回退 pglz，功能不受影响
2. **应用迁移**：启动时自动执行 `136_payload_audit_logs.sql` + `136_payload_audit_logs_partition_funcs.sql`
3. **后台配置**：风控中心 → 调用留存 → 配置
   - 启用开关（默认关闭）
   - 选择适用分组：`all_groups` 或指定 `group_ids`
   - 调整摘要长度、单条上限、留存期等参数
4. **创建 audit API key**：在后台为第三方扫描器创建专用 key，记下明文 token
5. **建议先灰度**：选 1-2 个低流量分组开启，观察 `payload_audit_*` 指标 24 小时后再全量铺开

## 配置项一览

| Key | 默认值 | 范围 | 说明 |
|---|---|---|---|
| `excerpt_bytes` | 512 | [64, 2048] | 摘要长度（字节），0 禁用摘要 |
| `input_max_bytes` | 10 MB | - | input 单条上限，超出截断 |
| `output_max_bytes` | 5 MB | - | output 单条上限，超出截断 |
| `retention_days` | 180 | ≥ 1 | 数据保留天数 |
| `worker_count` | 4 | [1, 32] | sink worker 并发数 |
| `queue_size` | 32768 | > 0 | 队列条数上限 |
| `queue_max_bytes` | 1 GiB | > 0 | 队列字节上限 |
| `batch_size` | 100 | > 0 | 单批 INSERT 条数 |
| `batch_flush_ms` | 200 | > 0 | 定时刷新间隔（毫秒） |

## 容量预估

按平均事件 ~10 KB（lz4 压缩后 ~3 KB）估算：

| 日请求量 | 6 个月数据量 |
|---|---|
| 1k/day | ~540 MB |
| 10k/day | ~5.4 GB |
| 100k/day | ~54 GB |
| 1M/day | ~540 GB |

实际数据量取决于 input/output 大小分布。Vision payload（含图片 base64）会显著放大存储量。

## 监控告警

关键 Prometheus 指标：

- **`payload_audit_queue_depth`** 持续 > `queue_size * 50%` 超过 5 分钟 → 扩大 `worker_count`
- **`payload_audit_enqueued_total{result="dropped"}`** 增量 > 0 → 队列丢弃发生，需调参（增大 `queue_size` / `queue_max_bytes` 或增加 worker）
- **`payload_audit_batch_failed_total`** 增量 > 0 → DB 写入异常，排查 PG 连接
- **`payload_audit_redis_drain_total{result="fail"}`** 增量 > 0 → SIGTERM 时 Redis 暂存回写失败

## 故障排查

**队列满 (`drop_queue_full`)**
- worker_count 太少 / batch 太慢 / DB 响应慢
- 解决：增大 `worker_count`，优化 PG 写入性能（索引、连接池）

**byte_budget 拒绝**
- 单条事件过大（如 Vision 巨型 payload），超出 `queue_max_bytes` 预算
- 解决：调小 `input_max_bytes` 或调大 `queue_max_bytes`

**分区维护失败**
- 02:00 UTC cron 创建下月分区失败
- 排查：PG 连接 + 权限，确认 `payload_audit_ensure_partition()` 函数存在

**Cleanup cron 失败**
- 03:00 UTC 清理过期分区失败
- 可能原因：`DETACH PARTITION` 锁冲突
- 解决：等待下次 cron 自动重跑，或手动执行

## 第三方扫描器接入示例

### 创建 audit API key

在后台 UI 创建专用 audit key，记下明文 token（仅创建时展示一次）。

### cURL 示例

```bash
TOKEN="sk-pa-XXXXXXXX"

# 列表查询（支持时间范围、分页）
curl -H "Authorization: Bearer $TOKEN" \
    "http://localhost:8080/api/v1/audit/exports/payloads?from=2026-01-01T00:00:00Z&to=2026-01-08T00:00:00Z&include_body=excerpt&limit=100"

# 单条详情（含完整 body）
curl -H "Authorization: Bearer $TOKEN" \
    "http://localhost:8080/api/v1/audit/exports/payloads/12345"

# NDJSON 流式批量导出
curl -H "Authorization: Bearer $TOKEN" \
    "http://localhost:8080/api/v1/audit/exports/payloads.ndjson?from=2026-01-01T00:00:00Z&to=2026-01-08T00:00:00Z" \
    > daily_audit.ndjson
```

### Python 示例

```python
import requests

TOKEN = "sk-pa-XXXXXXXX"
BASE = "http://localhost:8080/api/v1/audit/exports/payloads"

cursor = None
while True:
    params = {
        "from": "2026-01-01T00:00:00Z",
        "to": "2026-01-08T00:00:00Z",
        "limit": 500,
    }
    if cursor:
        params["cursor"] = cursor
    r = requests.get(
        BASE,
        params=params,
        headers={"Authorization": f"Bearer {TOKEN}"},
    ).json()
    for log in r["data"]:
        # 扫描 log["input_excerpt"], log["output_excerpt"]
        ...
    cursor = r.get("next_cursor")
    if not cursor:
        break
```

## 限制与约束

- 列表查询时间窗 ≤ 31 天；NDJSON 导出 ≤ 7 天（含 keyword 搜索时）
- `limit` 上限 500（超过自动 clamp）
- 仅 admin 可查看完整 body；无"审计员的审计"功能（即无法审计谁查了审计记录）
- 不支持多副本部署（SIGTERM Redis key 会冲突）
- 不支持 OpenAI Realtime / WebSocket（不在覆盖范围）

## 与现有模块的关系

- **`usage_logs`**：通过 `request_id` 关联，两者独立写入
- **`content_moderation_logs`**：被审核拦截的请求**仍会**被 audit 记录（保留尝试输入证据）
- **`ops_system_logs`**：导出 API 访问记录、cleanup 失败等运维事件写入 ops log
