-- internal/plugins/roles' persistence: runtime state (pending role
-- reversals), not guild configuration, so it lives in the plugin's own
-- tables rather than settings_guild/settings_* -- mirrors how
-- rotation_archives is owned by internal/plugins/rotation rather than
-- internal/settings.

-- One row per currently-jailed member: snapshot of the roles they had
-- before being stripped, so /roles release (manual or scheduled) can
-- restore exactly what was removed. PRIMARY KEY enforces "one active jail
-- per member at a time" -- jailing an already-jailed member is rejected at
-- the application layer rather than silently overwriting the snapshot.
CREATE TABLE role_jails (
    guild_id          TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    snapshot_role_ids TEXT[] NOT NULL DEFAULT '{}',
    jail_role_id      TEXT NOT NULL,
    jailed_at         TIMESTAMPTZ NOT NULL,
    release_at        TIMESTAMPTZ,          -- NULL = indefinite, never auto-released
    jailed_by         TEXT NOT NULL,
    reason            TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (guild_id, user_id)
);

CREATE INDEX role_jails_due_idx
    ON role_jails (guild_id, release_at)
    WHERE release_at IS NOT NULL;

-- One row per single-role temporary (or permanent-but-tracked) grant merlin
-- herself made. A member can hold multiple independent tracked grants at
-- once (unlike jail, which is one all-or-nothing state), so this is keyed
-- by (guild, user, role) rather than (guild, user) alone.
CREATE TABLE role_grants (
    id         BIGSERIAL PRIMARY KEY,
    guild_id   TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    role_id    TEXT NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,                 -- NULL = permanent, never auto-removed
    granted_by TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    UNIQUE (guild_id, user_id, role_id)
);

CREATE INDEX role_grants_due_idx
    ON role_grants (guild_id, expires_at)
    WHERE expires_at IS NOT NULL;
