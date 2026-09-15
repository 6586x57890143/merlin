-- Scripts: per-server behaviour below plugins, off until turned on.
-- A row here means the script is on in that guild; nothing else is stored,
-- so the zero state of every guild is off. See internal/scripts.
CREATE TABLE IF NOT EXISTS scripts_enabled (
    guild_id   TEXT        NOT NULL,
    script     TEXT        NOT NULL,
    enabled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, script)
);

-- The eternal-role script's copy of the role it protects (roles plugin,
-- script_eternalrole.go). Captured once, when the script first runs for
-- that member, and never re-captured from Discord: only role_id and
-- icon_hash move, after merlin herself recreates the role. origin_role_id
-- is the ID compiled into the script; role_id is whichever role currently
-- stands in for it.
CREATE TABLE IF NOT EXISTS script_eternal_roles (
    guild_id       TEXT        NOT NULL,
    user_id        TEXT        NOT NULL,
    origin_role_id TEXT        NOT NULL,
    role_id        TEXT        NOT NULL,
    name           TEXT        NOT NULL,
    color          INTEGER     NOT NULL,
    hoist          BOOLEAN     NOT NULL,
    mentionable    BOOLEAN     NOT NULL,
    permissions    BIGINT      NOT NULL,
    unicode_emoji  TEXT        NOT NULL,
    icon_hash      TEXT        NOT NULL,
    icon           BYTEA,
    captured_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (guild_id, user_id, origin_role_id)
);
