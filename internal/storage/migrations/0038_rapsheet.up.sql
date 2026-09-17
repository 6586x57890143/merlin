-- Milestone 13: rapsheets.
--
-- A per-member moderation ledger. Until now merlin had no moderation history
-- at all: role_jails is current state and its row is deleted on release,
-- aimod_incidents keeps a sanction's length and prior count inside a reason
-- string, and audit_log has no target column, no read path and a one-year
-- prune. A moderator asking "what has this person done before" had nowhere
-- to look, and aimod's own ladder counted only what aimod itself had done.
--
-- Five tables, split the way 0021 and 0029 split theirs: rapsheet_config is
-- guild configuration, the rest is runtime state that belongs to this plugin
-- and to nothing else. None of it goes in settings_guild.
--
-- What is deliberately NOT here: message content. An entry records what was
-- decided and why, in a moderator's words or a policy bucket's name, never
-- what the member wrote. aimod holds its own evidence under its own retention
-- and an entry points at it by id.

CREATE TABLE IF NOT EXISTS rapsheet_config (
    guild_id         TEXT PRIMARY KEY,
    -- off: ledger only. suggest: the ladder posts a recommendation with an
    -- Apply button. auto: the ladder acts on its own, never against staff and
    -- never permanently. Suggest by default, the same watch-it-first shape as
    -- aimod's calibration_mode and triage_mode, because this changes who gets
    -- jailed and a guild should see it work before it is trusted.
    escalation_mode  TEXT NOT NULL DEFAULT 'suggest'
                     CHECK (escalation_mode IN ('off', 'suggest', 'auto')),
    -- Score decay half-life. Points fade continuously rather than falling
    -- off a cliff at a window edge, so old history never counts in full and
    -- never vanishes entirely.
    half_life_hours  INTEGER NOT NULL DEFAULT 720 CHECK (half_life_hours > 0),
    -- Per-category point overrides; an absent key means the compiled-in
    -- severity default. Same overrides-only shape as aimod's bucket_actions.
    category_points  JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- The ladder, as [{"min":50,"action":"jail","duration_secs":7200},...].
    -- Empty means the compiled-in defaults.
    bands            JSONB NOT NULL DEFAULT '[]'::jsonb,
    mod_channel_id   TEXT NOT NULL DEFAULT '',
    forum_channel_id TEXT NOT NULL DEFAULT '',
    alt_hints        BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rapsheet_entries (
    -- The case id, rendered #123. Global rather than per guild: a per-guild
    -- counter needs its own table and a lock, and nothing about a case
    -- number needs to be small or unguessable.
    id            BIGSERIAL PRIMARY KEY,
    guild_id      TEXT NOT NULL,
    user_id       TEXT NOT NULL,
    kind          TEXT NOT NULL
                  CHECK (kind IN ('note', 'warn', 'removal', 'jail', 'timeout', 'kick',
                                  'ban', 'unban', 'release', 'suggestion')),
    -- The ten aimod policy buckets plus two for things Discord's rules do not
    -- cover. Closed set: a new category needs points in points.go before it
    -- can mean anything, so the CHECK is the backstop against a hand-edited
    -- row rather than the definition.
    category      TEXT NOT NULL DEFAULT 'other'
                  CHECK (category IN ('child_safety', 'violent_extremism', 'threats', 'doxxing',
                                      'ncii', 'malicious', 'hate_speech', 'gore', 'self_harm',
                                      'spam', 'server_rule', 'other')),
    -- Resolved when the row is written and never recomputed. A guild that
    -- retunes its points table must not silently re-weight decisions already
    -- taken, and the band a member was actioned at has to stay reproducible.
    points        INTEGER NOT NULL DEFAULT 0,
    -- Ladder rows only: the band this suggestion or consequence was for.
    -- Escalation idempotency is read off this column, not kept in memory.
    band          INTEGER NOT NULL DEFAULT 0,
    -- A user snowflake, or 'system' (core.ActorSystem) for automatic actions.
    actor_id      TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    -- Sentence length as decided. NULL where there is none, or for a
    -- permanent ban.
    duration_secs BIGINT,
    ends_at       TIMESTAMPTZ,
    -- Who wrote the row: a /rapsheet command, the ladder, this plugin's own
    -- unban sweep, an aimod incident, a roles jail or release, or Discord's
    -- own audit log (a ban done through the client rather than through
    -- merlin).
    source        TEXT NOT NULL
                  CHECK (source IN ('command', 'ladder', 'sweep', 'aimod', 'roles', 'discord')),
    -- The publisher's own id for the event that produced this row: an aimod
    -- incident id, a Discord audit-log entry id. The ingestion dedupe key.
    ref           TEXT NOT NULL DEFAULT '',
    -- Bans: set when the unban happened, whether by the sweep, by a command,
    -- or by a human that the audit log then told merlin about. A ban with
    -- ends_at in the past and lifted_at NULL is the sweep's work.
    lifted_at     TIMESTAMPTZ,
    -- A voided entry stays on the sheet, struck through, and counts for
    -- nothing. Same rule as aimod's undone incidents: one false positive
    -- must not lengthen every future sentence.
    voided_at     TIMESTAMPTZ,
    voided_by     TEXT NOT NULL DEFAULT '',
    void_reason   TEXT NOT NULL DEFAULT '',
    -- The message mirroring this entry into the member's case-file thread.
    -- Empty means not mirrored (yet, or the forum was unreachable).
    thread_message_id TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS rapsheet_entries_member_idx
    ON rapsheet_entries (guild_id, user_id, created_at DESC);
-- Ingestion idempotency: an event delivered twice writes once. Partial so
-- command rows, which have no external id, never collide with each other.
CREATE UNIQUE INDEX IF NOT EXISTS rapsheet_entries_ref_idx
    ON rapsheet_entries (guild_id, source, ref) WHERE ref <> '';
-- The unban sweep's entire working set.
CREATE INDEX IF NOT EXISTS rapsheet_entries_ban_due_idx
    ON rapsheet_entries (guild_id, ends_at)
    WHERE kind = 'ban' AND ends_at IS NOT NULL AND lifted_at IS NULL AND voided_at IS NULL;

-- One row per member ON FILE: created with their first entry, never on join.
-- A server of five thousand members does not get five thousand rows here,
-- and the forum does not get five thousand empty threads.
--
-- The identity snapshot is what alt detection matches a joiner against. It
-- is here rather than fetched because a REST lookup per candidate per join
-- does not scale, and because GuildMember fails for a banned user, who is
-- exactly the candidate that matters most.
CREATE TABLE IF NOT EXISTS rapsheet_case_files (
    guild_id    TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    -- The forum post holding this member's mirrored entries. Empty until
    -- created, and cleared again if Discord says it is gone, so the next
    -- entry recreates it.
    thread_id   TEXT NOT NULL DEFAULT '',
    username    TEXT NOT NULL DEFAULT '',
    global_name TEXT NOT NULL DEFAULT '',
    avatar_hash TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, user_id)
);

-- Accounts a moderator has said belong to the same person. group_id is the
-- user_id of whichever account was linked first: a label that everything in
-- the group shares, not a row anything joins on.
CREATE TABLE IF NOT EXISTS rapsheet_links (
    guild_id   TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    group_id   TEXT NOT NULL,
    linked_by  TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, user_id)
);
CREATE INDEX IF NOT EXISTS rapsheet_links_group_idx ON rapsheet_links (guild_id, group_id);

-- What the join-time heuristics thought, for a moderator to confirm or
-- ignore. Not entries, because an entry opens a case file and a joiner who
-- merely resembles somebody is not on file.
CREATE TABLE IF NOT EXISTS rapsheet_alt_hints (
    guild_id     TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    candidate_id TEXT NOT NULL,
    signals      TEXT[] NOT NULL,
    score        INTEGER NOT NULL,
    opinion      TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, user_id, candidate_id)
);
