INSERT INTO scripts_enabled (guild_id, script)
    SELECT guild_id, 'dialects' FROM dragoman_dialects WHERE language = 'nl' AND name = 'African American Vernacular English'
    ON CONFLICT DO NOTHING;
UPDATE settings_permission_overrides SET action = 'dragoman.scripts' WHERE action = 'dragoman.dialects';
DROP TABLE IF EXISTS dragoman_dialects;
