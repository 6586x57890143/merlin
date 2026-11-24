DROP TABLE IF EXISTS dragoman_usage;
ALTER TABLE dragoman_config DROP COLUMN IF EXISTS model;
DELETE FROM aimod_triage WHERE guild_id LIKE 'dragoman:%';
