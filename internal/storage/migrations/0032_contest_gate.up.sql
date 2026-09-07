-- Gate the contest forum on the roles the guild already gates on.
--
-- createForum set exactly one overwrite -- @everyone denied CreatePublicThreads
-- -- and otherwise inherited whatever category it was put in. On a server where
-- accounts that have not passed the gate still hold @everyone and only gated
-- members hold a role like @melted, every ungated account could read the
-- contest forum and post to it the moment submissions opened.
--
-- gate_channel_id is the answer, and the reason it is a channel rather than a
-- list of roles is that a list of roles is a copy of a fact the server already
-- states. Point it at a channel the guild already gates the way it wants and
-- merlin mirrors that channel's view permissions onto each new contest forum:
-- nothing about the role layout is stored, so nothing rots, no deleted role
-- ever has to be pruned out of here, and a server that renames or replaces
-- @melted needs no change on this side at all.
--
-- access_role_ids is the escape hatch for a guild with no channel worth
-- mirroring, and it wins over the mirror when set. media_role_ids exists
-- because the mirror deliberately copies only the view-gating bits: a text
-- channel's SendMessages does not mean the same thing as a forum's
-- CreatePublicThreads, so posting and attachment permissions stay merlin's own
-- to write, and a guild that puts attachment rights on a separate role needs
-- somewhere to say so.
--
-- These live on contest_config rather than settings_guild for the reason
-- migration 0029 gives: settings_guild's cache is handed to core.Permissions on
-- every single command and has no business carrying contest configuration.
ALTER TABLE contest_config
    ADD COLUMN IF NOT EXISTS gate_channel_id TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS access_role_ids TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS media_role_ids  TEXT[] NOT NULL DEFAULT '{}';
