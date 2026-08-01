CREATE TABLE rotation_archives (
    channel_id        TEXT PRIMARY KEY,   -- the archived (old) channel's Discord ID
    guild_id          TEXT NOT NULL,
    source_channel_id TEXT NOT NULL,      -- the configured "logical" channel this replaced
    archived_at       TIMESTAMPTZ NOT NULL,
    delete_after      TIMESTAMPTZ          -- NULL = keep forever, never swept
);

CREATE INDEX rotation_archives_due_idx
    ON rotation_archives (guild_id, delete_after)
    WHERE delete_after IS NOT NULL;
