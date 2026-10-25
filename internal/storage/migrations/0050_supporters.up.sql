-- The role merlin creates to give supporters in a guild, and where she
-- thanks new ones. No role means the guild hasn't turned supporters on.
ALTER TABLE settings_guild ADD COLUMN supporter_role_id TEXT, ADD COLUMN supporter_channel_id TEXT;
