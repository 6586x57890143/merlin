ALTER TABLE settings_permission_overrides
    DROP COLUMN required_tier,
    DROP COLUMN deny_role_ids,
    DROP COLUMN deny_user_ids;

ALTER TABLE settings_guild
    DROP COLUMN disabled_plugins;
