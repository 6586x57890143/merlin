-- Existing allowlists become read-only. Conversation access is explicit opt-in.
ALTER TABLE settings_guild
    ADD COLUMN jail_conversation_channel_ids TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN vacation_conversation_channel_ids TEXT[] NOT NULL DEFAULT '{}';

-- Persist the previously implicit beach so removing it stays removed.
INSERT INTO settings_guild (guild_id, vacation_allowed_channel_ids)
VALUES ('1459404984943644747', ARRAY['1549902953631195227'])
ON CONFLICT (guild_id) DO UPDATE SET vacation_allowed_channel_ids = EXCLUDED.vacation_allowed_channel_ids
WHERE cardinality(settings_guild.vacation_allowed_channel_ids) = 0;

ALTER TABLE settings_guild
    ADD CONSTRAINT jail_access_disjoint CHECK (NOT (jail_allowed_channel_ids && jail_conversation_channel_ids)),
    ADD CONSTRAINT vacation_access_disjoint CHECK (NOT (vacation_allowed_channel_ids && vacation_conversation_channel_ids));
