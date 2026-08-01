-- Generalizes the per-action allow-whitelist (0005) into a full per-guild
-- permission policy: an optional tier override (does this action require
-- Mod+Admin or Admin-only in this guild, overriding the command's compiled-in
-- default) plus a deny-list alongside the existing allow-list.
ALTER TABLE settings_permission_overrides
    ADD COLUMN required_tier SMALLINT,
    ADD COLUMN deny_role_ids TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN deny_user_ids TEXT[] NOT NULL DEFAULT '{}';

-- Per-guild whole-plugin on/off switch, coarser than any per-action policy —
-- checked by core.CommandRouter before a disabled plugin's commands are even
-- authorized. "adminconfig" (owning /config) is guarded against being listed
-- here at the application layer, since disabling it would permanently lock
-- a guild out of ever re-enabling anything.
ALTER TABLE settings_guild
    ADD COLUMN disabled_plugins TEXT[] NOT NULL DEFAULT '{}';
