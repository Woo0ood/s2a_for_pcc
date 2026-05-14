-- Add per-user 5h/7d USD rate limits aggregated across all API keys.
-- 与 APIKey 自身的 rate_limit_5h/7d 并行检查，取最严格。0 = 不限制。
-- 窗口开始时间为 NULL 表示尚未激活，首次产生用量时由服务端写入。

ALTER TABLE users ADD COLUMN IF NOT EXISTS rate_limit_5h numeric(20,8) NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS rate_limit_7d numeric(20,8) NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS usage_5h numeric(20,8) NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS usage_7d numeric(20,8) NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS window_5h_start timestamptz NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS window_7d_start timestamptz NULL;

COMMENT ON COLUMN users.rate_limit_5h IS '用户级 5h USD 限额；0 = 不限制；跨所有 API Key 聚合。';
COMMENT ON COLUMN users.rate_limit_7d IS '用户级 7d USD 限额；0 = 不限制；跨所有 API Key 聚合。';
COMMENT ON COLUMN users.usage_5h IS '当前 5h 窗口已用 USD（用户级聚合）。';
COMMENT ON COLUMN users.usage_7d IS '当前 7d 窗口已用 USD（用户级聚合）。';
COMMENT ON COLUMN users.window_5h_start IS '当前 5h 限额窗口起点；NULL 表示尚未激活。';
COMMENT ON COLUMN users.window_7d_start IS '当前 7d 限额窗口起点；NULL 表示尚未激活。';
