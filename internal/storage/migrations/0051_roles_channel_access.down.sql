ALTER TABLE settings_guild DROP CONSTRAINT jail_access_disjoint, DROP CONSTRAINT vacation_access_disjoint;
UPDATE settings_guild SET
    jail_allowed_channel_ids = jail_allowed_channel_ids || jail_conversation_channel_ids,
    vacation_allowed_channel_ids = vacation_allowed_channel_ids || vacation_conversation_channel_ids;
ALTER TABLE settings_guild DROP COLUMN jail_conversation_channel_ids, DROP COLUMN vacation_conversation_channel_ids;
