-- GREATEST(1, ...) because rolling back an interval finer than an hour --
-- the whole point of the up migration -- would otherwise floor to 0, and a
-- zero interval is a rotation job with no cadence at all. Rounding a 30m
-- rotation back up to 1h loses the setting, which is unavoidable when the
-- column can no longer express it, but it stays a valid configuration.
UPDATE settings_rotation_channels SET interval_minutes = GREATEST(1, interval_minutes / 60);
ALTER TABLE settings_rotation_channels RENAME COLUMN interval_minutes TO interval_hours;
