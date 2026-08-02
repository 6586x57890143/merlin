-- rotation_archives.delete_after was written exactly once, at archive time,
-- from whatever retention_hours happened to be configured at that instant,
-- and never revisited. Nothing anywhere updated it again, so changing a
-- rotating channel's retention only ever applied to *future* archives:
--
--   /rotation configure add ... retention:24h   -> archive gets +24h
--   /rotation configure edit ... retention:90d  -> "Rotation updated."
--   ...24 hours later the sweep permanently deletes the archive anyway.
--
-- Both directions were wrong. Extending retention (or switching to
-- retention_forever) silently failed to protect archives the admin had just
-- promised to keep, and permanent channel deletion is not recoverable.
-- Shortening it silently kept content past the new, tighter window, which is
-- the promise this whole feature exists to make (spec.MD §6).
--
-- The fix is to stop treating delete_after as authoritative and re-derive the
-- deadline at sweep time from the live retention setting, the same way every
-- other decision in this plugin re-derives from live state instead of
-- trusting a stored assumption. That needs a stable link from an archive back
-- to the rotation slot that produced it: source_channel_id can't serve,
-- because rotate() retargets a slot's channel_id onto the new live channel
-- after every cycle, so archives from different generations of the same slot
-- carry different source_channel_ids (the same reason archive_category_id had
-- to be denormalized in migration 0008).
--
-- rotation_id is settings_rotation_channels.id (migration 0009) - stable
-- across retargeting for the life of the rotation slot. Nullable: rows
-- archived before this migration have no recoverable link, and the sweep
-- falls back to their stored delete_after for those.
ALTER TABLE rotation_archives ADD COLUMN rotation_id BIGINT;

CREATE INDEX rotation_archives_rotation_idx
    ON rotation_archives (rotation_id)
    WHERE rotation_id IS NOT NULL;
