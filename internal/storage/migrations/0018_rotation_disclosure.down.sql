ALTER TABLE settings_rotation_channels
    DROP CONSTRAINT IF EXISTS settings_rotation_channels_disclosure_chk;

ALTER TABLE settings_rotation_channels
    DROP COLUMN IF EXISTS disclosure;
