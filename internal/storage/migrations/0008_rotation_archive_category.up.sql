-- Denormalizes the archive category a channel was actually moved into at the
-- moment it was archived, onto the archive row itself. sweep.go's "was this
-- channel rescued out of its archive category" check previously re-derived
-- the expected category by looking up the LIVE settings_rotation_channels
-- row for the archive's source_channel_id, which broke the moment rotation
-- started retargeting that row's channel_id onto the new live channel after
-- every successful rotation (the row is no longer keyed by source_channel_id
-- at all after the first cycle), making every archive look "unconfigured"
-- and therefore permanently rescued from deletion. Recording the category at
-- archive time makes the check self-contained and independent of whatever
-- the mutable settings row currently says.
ALTER TABLE rotation_archives
    ADD COLUMN archive_category_id TEXT NOT NULL DEFAULT '';
