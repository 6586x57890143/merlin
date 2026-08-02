-- internal/settings' DB-backed replacement for guild-scoped config.yaml
-- fields (spec.MD §4a): admins configure all of this via Discord commands,
-- never by editing config.yaml/.env on the host.

CREATE TABLE settings_guild (
    guild_id             TEXT PRIMARY KEY,
    mod_role_ids         TEXT[] NOT NULL DEFAULT '{}',
    admin_user_ids       TEXT[] NOT NULL DEFAULT '{}',
    audit_log_channel_id TEXT   NOT NULL DEFAULT '',
    status_channel_id    TEXT   NOT NULL DEFAULT '',
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-action whitelist grants (independent of mod/admin tier): lets a
-- specific role/user run one named command action (e.g.
-- "rotation.configure") without becoming a full mod or admin.
CREATE TABLE settings_permission_overrides (
    guild_id   TEXT NOT NULL,
    action     TEXT NOT NULL,
    role_ids   TEXT[] NOT NULL DEFAULT '{}',
    user_ids   TEXT[] NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, action)
);

-- Replaces config.yaml's rotating_channels[]: one row per configured
-- rotating channel. Sticky message text lives here directly (not a separate
-- named-template table) since each rotating channel owns its own content.
CREATE TABLE settings_rotation_channels (
    guild_id                    TEXT NOT NULL,
    channel_id                  TEXT NOT NULL,
    interval_hours              INT  NOT NULL,
    archive_category_id         TEXT NOT NULL,
    archive_visibility          TEXT NOT NULL, -- mod_only | whitelist
    archive_whitelist_role_ids  TEXT[] NOT NULL DEFAULT '{}',
    archive_whitelist_user_ids  TEXT[] NOT NULL DEFAULT '{}',
    retention_days              INT,           -- NULL = keep forever, never swept
    sticky_enabled              BOOLEAN NOT NULL DEFAULT false,
    sticky_messages             TEXT[] NOT NULL DEFAULT '{}',
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, channel_id)
);
