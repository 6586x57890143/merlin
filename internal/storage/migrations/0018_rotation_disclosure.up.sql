-- How much a freshly rotated channel is told about its own rotation.
--
-- The notice posted after every rotation used to be chosen purely on whether
-- retention_hours was NULL: a guild either published both its rotation
-- cadence and its deletion window, or published the cadence plus "the mods
-- can still reach it". There was no way to publish one without the other,
-- and no way to say nothing at all.
--
-- That matters for the community this feature exists for. Stating the
-- deletion window is a promise worth making in a server that wants to point
-- at what the bot actually does; it is also a schedule worth withholding in
-- one that would rather not advertise exactly how long content survives.
-- Both are legitimate, and the choice belongs to the guild.
--
-- Per rotating channel rather than per guild, matching retention_hours
-- itself: a guild running one channel on a three hour window and another
-- kept forever has real reason to disclose them differently.
--
-- Legal values, validated at the point of mutation in
-- rotation/configure.go (validateRotationChannel) rather than by a CHECK
-- constraint, matching how archive_visibility is handled:
--
--   full      cadence and the archival window (today's behaviour)
--   cadence   the rotation window only
--   retention the archival window only
--   generic   neither; just that the channel rotated
--
-- Existing rows default to 'full', which is exactly what they do today, so
-- this migration changes no guild's published policy on its own.
ALTER TABLE settings_rotation_channels
    ADD COLUMN disclosure TEXT NOT NULL DEFAULT 'full';

-- A CHECK here, unlike archive_visibility which is bare TEXT next door.
-- The difference is that archive_visibility was designed to grow (spec.MD §6
-- lists export_then_delete as a future mode) while this is a closed set: the
-- four values are the whole product decision, and a fifth would need code in
-- rotation/templates.go and lines in the voice catalog before it could mean
-- anything. So the constraint costs nothing and is the fail-closed backstop
-- against a hand-edited row, which for this column would silently change
-- what a channel publishes about itself.
ALTER TABLE settings_rotation_channels
    ADD CONSTRAINT settings_rotation_channels_disclosure_chk
    CHECK (disclosure IN ('full', 'cadence', 'retention', 'generic'));
