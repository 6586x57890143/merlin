-- Pre-rotation heads-up.
--
-- Members' first notice of a rotation used to be the new channel already
-- being there, which is a poor experience for anyone mid-conversation.

-- How long before a rotation to warn the channel, per rotating channel.
-- 0 disables the notice for that slot.
--
-- Defaults to 10 minutes for existing rows as well as new ones. Normally a
-- migration would leave existing behaviour alone, but this is a message
-- being added rather than one changing, and it is the behaviour the feature
-- was asked for. An admin who does not want it sets the lead to 0 via
-- /rotation configure edit.
ALTER TABLE settings_rotation_channels
    ADD COLUMN notice_lead_minutes INTEGER NOT NULL DEFAULT 10;

-- Which notices have already gone out.
--
-- Runtime state, so it lives in its own table rather than in
-- settings_rotation_channels: settings holds guild configuration, and the
-- same split is why rotation_archives and role_jails exist (see
-- roles/store.go's package comment for the precedent).
--
-- Keyed on the rotation slot plus the instant the notice is *for*, not the
-- instant it was sent. That is what makes the claim idempotent: two sweeps
-- racing inside the same minute both compute the same notice_for, so
-- INSERT ... ON CONFLICT DO NOTHING lets exactly one of them win and the
-- loser sees zero rows affected. Keying on "sent at" instead would let both
-- post.
--
-- rotation_id references the stable settings_rotation_channels.id rather
-- than a channel ID, for the same reason rotation_archives does: rotate()
-- retargets a slot's channel_id onto the new channel every single cycle.
CREATE TABLE IF NOT EXISTS rotation_notices (
    rotation_id BIGINT      NOT NULL REFERENCES settings_rotation_channels(id) ON DELETE CASCADE,
    notice_for  TIMESTAMPTZ NOT NULL,
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (rotation_id, notice_for)
);

-- Pruning old claims: they are only interesting until the rotation they
-- refer to has happened.
CREATE INDEX IF NOT EXISTS rotation_notices_notice_for_idx
    ON rotation_notices (notice_for);
