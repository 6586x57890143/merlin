-- The vacation script's marker role (roles plugin, script_vacation.go): the
-- role a member on vacation holds instead of the jail marker. NULL means the
-- guild has not chosen one; merlin never creates it and never edits the
-- channel permissions it carries, since the island's permissions are the
-- guild's own.
ALTER TABLE settings_guild ADD COLUMN vacation_role_id TEXT;

-- Channels a member on vacation is additionally allowed to see. Unlike
-- jail_allowed_channel_ids this is additive only: merlin writes an allow
-- overwrite for the vacation role on each listed channel and nothing else,
-- never a deny elsewhere, so the island's own permissions stay the guild's.
ALTER TABLE settings_guild ADD COLUMN vacation_allowed_channel_ids TEXT[] NOT NULL DEFAULT '{}';
