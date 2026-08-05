-- Add optional configured jail marker role per guild
ALTER TABLE settings_guild ADD COLUMN jail_marker_role_id TEXT;