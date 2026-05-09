-- 调用留存（payload audit）：异步存储 LLM input/output 原文供合规扫描
-- PG 14+ 启用 lz4 列压缩；若 lz4 不可用则回退 pglz；PG < 14 不设 COMPRESSION

DO $$
DECLARE
    pg14_or_later BOOLEAN := current_setting('server_version_num')::int >= 140000;
    lz4_available BOOLEAN := FALSE;
    comp_clause   TEXT := '';
BEGIN
    -- PG 14+ 才支持 COMPRESSION 语法；同时需要编译时启用 lz4
    IF pg14_or_later THEN
        BEGIN
            EXECUTE 'CREATE TABLE _lz4_probe (c TEXT COMPRESSION lz4)';
            EXECUTE 'DROP TABLE _lz4_probe';
            lz4_available := TRUE;
        EXCEPTION WHEN feature_not_supported OR syntax_error THEN
            lz4_available := FALSE;
        END;
        comp_clause := ' COMPRESSION ' || CASE WHEN lz4_available THEN 'lz4' ELSE 'pglz' END;
    END IF;

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
            input_body      TEXT%s         NOT NULL DEFAULT '',
            output_body     TEXT%s         NOT NULL DEFAULT '',
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
    $f$, comp_clause, comp_clause);
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
