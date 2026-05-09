-- 创建指定月份的分区（month_start 应为某月 1 号 00:00 UTC）
-- 函数内部会将输入规范化为 UTC 月首，确保命名与分区范围一致
CREATE OR REPLACE FUNCTION payload_audit_create_partition(month_start TIMESTAMPTZ)
RETURNS TEXT AS $$
DECLARE
    -- 规范化：取 UTC 月首 00:00
    m_start TIMESTAMPTZ := date_trunc('month', month_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    p_name  TEXT := 'payload_audit_logs_' || to_char(m_start AT TIME ZONE 'UTC', 'YYYY_MM');
    p_end   TIMESTAMPTZ := m_start + INTERVAL '1 month';
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF payload_audit_logs FOR VALUES FROM (%L) TO (%L)',
        p_name, m_start, p_end
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
           make_timestamptz(
               (regexp_match(c.relname, '_(\d{4})_(\d{2})$'))[1]::int,
               (regexp_match(c.relname, '_(\d{4})_(\d{2})$'))[2]::int,
               1, 0, 0, 0, 'UTC'
           ) + INTERVAL '1 month' AS p_end
    FROM pg_inherits i
    JOIN pg_class c ON c.oid = i.inhrelid
    JOIN pg_class p ON p.oid = i.inhparent
    WHERE p.relname = 'payload_audit_logs'
      AND c.relname ~ '^payload_audit_logs_\d{4}_\d{2}$'
      AND make_timestamptz(
              (regexp_match(c.relname, '_(\d{4})_(\d{2})$'))[1]::int,
              (regexp_match(c.relname, '_(\d{4})_(\d{2})$'))[2]::int,
              1, 0, 0, 0, 'UTC'
          ) + INTERVAL '1 month' <= cutoff;
END;
$$ LANGUAGE plpgsql;
