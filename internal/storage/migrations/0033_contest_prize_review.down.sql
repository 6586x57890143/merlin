ALTER TABLE contest_prizes
    DROP COLUMN IF EXISTS reviewed_at,
    DROP COLUMN IF EXISTS reviewed_by,
    DROP COLUMN IF EXISTS approved,
    DROP COLUMN IF EXISTS code_shape;
