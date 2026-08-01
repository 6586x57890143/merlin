CREATE TABLE audit_log (
    id         BIGSERIAL PRIMARY KEY,
    guild_id   TEXT NOT NULL,
    actor_id   TEXT NOT NULL,
    action     TEXT NOT NULL,
    old_value  TEXT NOT NULL DEFAULT '',
    new_value  TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_guild_idx ON audit_log (guild_id, created_at DESC);
