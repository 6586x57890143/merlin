DROP INDEX IF EXISTS rotation_archives_rotation_idx;
ALTER TABLE rotation_archives DROP COLUMN IF EXISTS rotation_id;
