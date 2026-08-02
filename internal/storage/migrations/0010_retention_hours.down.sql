UPDATE settings_rotation_channels SET retention_hours = retention_hours / 24 WHERE retention_hours IS NOT NULL;
ALTER TABLE settings_rotation_channels RENAME COLUMN retention_hours TO retention_days;
