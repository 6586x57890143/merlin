-- audit_log becomes each guild's ledger chain (cygnet/ledger): seq numbers a
-- guild's entries, entry holds the typed entry the card is drawn from, and
-- prev/hash chain each row to the one before it with sha256, so a range can
-- be checked with ledger.Verify rather than trusted. Rows written before
-- this have no seq and stand outside every chain.
ALTER TABLE audit_log
    ADD COLUMN seq   BIGINT,
    ADD COLUMN entry JSONB,
    ADD COLUMN prev  BYTEA,
    ADD COLUMN hash  BYTEA;

CREATE UNIQUE INDEX audit_log_chain_idx ON audit_log (guild_id, seq) WHERE seq IS NOT NULL;
