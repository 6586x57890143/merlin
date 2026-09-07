ALTER TABLE contest_config
    DROP COLUMN IF EXISTS gate_channel_id,
    DROP COLUMN IF EXISTS access_role_ids,
    DROP COLUMN IF EXISTS media_role_ids;
