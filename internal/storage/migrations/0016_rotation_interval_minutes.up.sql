-- Rotation interval was whole hours (interval_hours INT), so "every 90
-- minutes" or "every 2h30m" could not be expressed at all: /rotation
-- configure parsed a flexible duration and then truncated it with
-- int(interval / time.Hour), silently rounding 90m down to 1h. Storing
-- minutes keeps the same value for every existing row (only the unit
-- multiplies, exactly as migration 0010 did for retention) while letting the
-- command accept anything down to the minute.
--
-- The one-hour floor stays, but it is now a validation rule
-- (minRotationInterval in configure.go) rather than an artifact of the
-- column's granularity -- which is the right place for it, since the reason
-- for a floor is rate limits and member whiplash, not storage.
ALTER TABLE settings_rotation_channels RENAME COLUMN interval_hours TO interval_minutes;
UPDATE settings_rotation_channels SET interval_minutes = interval_minutes * 60;
