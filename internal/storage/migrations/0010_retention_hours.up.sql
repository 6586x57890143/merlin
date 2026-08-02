-- Retention used to be days-only (retention_days INT) while interval was
-- already hours-only (interval_hours INT) -- two different granularities
-- for what's conceptually the same kind of value. Unifying both to
-- whole-hour storage lets /rotation configure accept retention in either
-- days or hours (core.ParseFlexibleDuration/FormatDuration), matching
-- interval's own flexibility, with no change to what either column means
-- for existing rows (only the unit multiplies).
ALTER TABLE settings_rotation_channels RENAME COLUMN retention_days TO retention_hours;
UPDATE settings_rotation_channels SET retention_hours = retention_hours * 24 WHERE retention_hours IS NOT NULL;
