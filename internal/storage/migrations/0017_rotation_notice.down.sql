DROP TABLE IF EXISTS rotation_notices;

ALTER TABLE settings_rotation_channels
    DROP COLUMN IF EXISTS notice_lead_minutes;
