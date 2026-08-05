-- Drop the optional jail marker role column
ALTER TABLE settings_guild DROP COLUMN IF EXISTS jail_marker_role_id;