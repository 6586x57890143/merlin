-- Per-guild calibration for internal/plugins/aimod: a small set of labelled
-- examples, learned weekly from the guild's own moderation history, that is
-- rendered into the classifier prompts.
--
-- Columns on aimod_config rather than a table of their own, because every
-- run REPLACES the set wholesale rather than adding to it. Nothing
-- accumulates, so there is no eviction policy, no per-row prune, and no join
-- on a path that already reads this row on every message.
--
-- JSONB rather than parallel arrays: the entries are structs, and the code
-- that reads them (aimod.validateCalibration) drops anything malformed
-- rather than trusting the column, so the database's job here is storage,
-- not schema enforcement.

-- The active set. Rendered into the fast and deep prompts.
ALTER TABLE aimod_config ADD COLUMN calibration JSONB NOT NULL DEFAULT '[]';

-- A proposal awaiting /aimod calibrate apply. Never read by the classifier.
-- Separate from the active set so a run in suggest mode cannot change what
-- the filter does simply by having happened.
ALTER TABLE aimod_config ADD COLUMN calibration_pending JSONB NOT NULL DEFAULT '[]';

-- off     = never review, and no scheduler job exists
-- suggest = review weekly, post the proposal, wait for an admin
-- auto    = review weekly and apply the result
--
-- suggest by default, for the same reason aimod's own mode ladder has flag
-- between off and enforce: this changes what a message-deleting filter does,
-- and a guild should get to watch it work once before it acts. The CHECK is
-- the backstop against a hand-edited row, matching
-- settings_rotation_channels.disclosure.
ALTER TABLE aimod_config ADD COLUMN calibration_mode TEXT NOT NULL DEFAULT 'suggest'
    CHECK (calibration_mode IN ('off', 'suggest', 'auto'));

-- When the last review ran. Displayed by /aimod calibrate show; nothing
-- schedules from it (the Scheduler owns its own last-run bookkeeping).
ALTER TABLE aimod_config ADD COLUMN calibration_ran_at TIMESTAMPTZ;
