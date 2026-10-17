DROP INDEX IF EXISTS audit_log_chain_idx;
ALTER TABLE audit_log
    DROP COLUMN hash,
    DROP COLUMN prev,
    DROP COLUMN entry,
    DROP COLUMN seq;
