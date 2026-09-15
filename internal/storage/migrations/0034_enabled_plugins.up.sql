-- Plugins that are off until a guild turns them on.
--
-- disabled_plugins makes every plugin on by default, which is right for
-- rotation or roles: a guild that never touched a setting is served rather
-- than silently ignored. whisper is the first plugin where the default has
-- to go the other way, because it lets members post through the bot, and a
-- server should choose that rather than discover it. So a plugin the store
-- has been told is default-off is on only where it is listed here, and its
-- entry in disabled_plugins is never consulted.
ALTER TABLE settings_guild
    ADD COLUMN IF NOT EXISTS enabled_plugins TEXT[] NOT NULL DEFAULT '{}';
