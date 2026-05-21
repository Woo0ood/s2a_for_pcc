-- 140_drop_payload_audit_logs.sql
-- Drop the PostgreSQL payload_audit_logs partitioned table and its helper functions.
-- Data is gone permanently; the audit store has moved to ClickHouse.
DROP TABLE IF EXISTS payload_audit_logs CASCADE;
DROP FUNCTION IF EXISTS payload_audit_partitions_before(timestamp with time zone);
DROP FUNCTION IF EXISTS payload_audit_create_partition(timestamp with time zone);
