-- Rung 1.5: a local classifier that decides whether a message is worth a
-- model call at all.
--
-- Two pieces in two places, on the same split the tip jar uses.
--
-- triage_mode is guild configuration read on the message hot path, so it
-- belongs on aimod_config behind cachingStore. Three values rather than a
-- boolean, and the middle one is the point: shadow scores every message and
-- learns from every verdict while still sending everything to the fast pass,
-- so an admin can read what it would have saved off /aimod status before
-- trusting it. Default shadow, for the same reason calibration_mode defaults
-- to suggest: this changes what gets looked at, and a guild should watch it
-- work rather than discover it.
ALTER TABLE aimod_config ADD COLUMN triage_mode TEXT NOT NULL DEFAULT 'shadow'
    CHECK (triage_mode IN ('off', 'shadow', 'on'));

-- The weights are runtime state, written every few hundred messages, and are
-- never read by the config hot path. Their own table keeps them out of the
-- row behind cachingStore, so saving a model needs no cache invalidation.
--
-- weights holds the model itself and nothing else: a fixed-width block of
-- float32s plus a bias. There is deliberately no table of training examples
-- anywhere, because there is no training set. The model learns online from
-- the fast pass's own verdicts as they happen, with the text already in hand,
-- and that text is discarded exactly as it was before. Nothing a member wrote
-- is recoverable from a block of gradient-updated weights, so continuous
-- learning here extends what this plugin retains by nothing at all.
--
-- examples is the count behind the warmup rule. A model below the warmup
-- threshold still scores and still learns, it is simply never allowed to skip
-- anything, which is what makes a fresh guild behave exactly as it did before
-- this rung existed.
CREATE TABLE aimod_triage (
    guild_id   TEXT PRIMARY KEY,
    weights    BYTEA  NOT NULL,
    examples   BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
