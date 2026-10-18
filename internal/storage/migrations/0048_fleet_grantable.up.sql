-- The roles another bird on cygnet's link may ask merlin to give a member
-- (fleetgrant): one row per guild, asking bird and role, added by an
-- admin with /fleet allow. Nothing here is ever granted without one.
CREATE TABLE fleet_grantable (
    guild_id TEXT        NOT NULL,
    caller   TEXT        NOT NULL,
    role_id  TEXT        NOT NULL,
    added_by TEXT        NOT NULL,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, caller, role_id)
);
