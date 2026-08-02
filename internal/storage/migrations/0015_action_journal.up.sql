-- Append-only record of every destructive Discord call Merlin attempted,
-- written by internal/discordguard around the call itself.
--
-- This is deliberately NOT an outbox. Nothing reads it to decide what to do,
-- nothing retries from it, and no plugin logic may consult it. Rotation and
-- roles already achieve crash-safety by re-deriving state from live Discord
-- (archivedChannelOrigin, replacement matching by name+category, jail's
-- record-before-strip and its confused-deputy re-check); a second source of
-- truth driving recovery in parallel with the first would be a new failure
-- mode — two mechanisms that can disagree — not a safety gain.
--
-- What it adds over audit_log is the attempt. audit_log records what
-- succeeded, from the plugin's point of view, after the fact. This records
-- that a call was made at all, and what came back, including the calls that
-- never reached a plugin's audit path: refused by the rate cap, refused by
-- the circuit breaker, or failed at the API. It answers "why did nothing
-- happen" — the question logs alone answer badly once they have rotated away.
CREATE TABLE action_journal (
    id         BIGSERIAL PRIMARY KEY,
    guild_id   TEXT NOT NULL,
    op         TEXT NOT NULL,          -- discordguard's operation class
    target_id  TEXT NOT NULL DEFAULT '', -- channel/user/role the call acted on
    state      TEXT NOT NULL,          -- 'pending' | 'done' | 'failed'
    error      TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at   TIMESTAMPTZ
);

-- Supports both the retention sweep (by age) and "what did we do to this
-- guild lately", which is how it will actually be read during an incident.
CREATE INDEX action_journal_guild_started_idx
    ON action_journal (guild_id, started_at DESC);

-- A row still 'pending' long after started_at is a call that never returned:
-- the process died mid-mutation. Cheap to find, and the first thing worth
-- looking at after an unexplained restart.
CREATE INDEX action_journal_pending_idx
    ON action_journal (started_at)
    WHERE state = 'pending';
