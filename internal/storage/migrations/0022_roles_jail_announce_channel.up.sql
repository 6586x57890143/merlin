-- Which single channel (besides the one the command was run in) hears jail
-- and release announcements. NULL/empty means the invoking channel only.
ALTER TABLE settings_guild ADD COLUMN jail_announce_channel_id TEXT;
