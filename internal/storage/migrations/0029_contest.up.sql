-- Milestone 11: contests.
--
-- Four tables, and the split between them is the same one migration 0021
-- drew for aimod: contest_config is guild configuration, the other three are
-- runtime state that belongs to the plugin and to nothing else. None of it
-- goes in settings_guild, whose cache is handed to core.Permissions on every
-- single command and has no business carrying a prize ledger.
--
-- What is deliberately NOT here: submission files. Members post in a Discord
-- forum channel, so Discord's own CDN hosts the bytes for free and forever,
-- and merlin stores a URL it re-derives from the thread rather than a copy.
-- That is why media_url can go stale without being a problem: signed CDN
-- links expire in about a day, and re-reading the thread's starter message
-- returns a fresh one, so the column is a cache with a known refresh path
-- rather than a record.
--
-- Votes are not here either. They live in the Cloudflare Worker's D1, keyed
-- by an HMAC of the voter's Discord ID, so the vote ledger holds no Discord
-- identities at all and merlin holds no ballots. What comes back across that
-- boundary is a tally, stored in contests.results.

CREATE TABLE IF NOT EXISTS contest_config (
    guild_id            TEXT PRIMARY KEY,
    announce_channel_id TEXT,
    forum_category_id   TEXT,
    default_max_votes   INT  NOT NULL DEFAULT 3,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS contests (
    id                  TEXT PRIMARY KEY,
    guild_id            TEXT NOT NULL,
    -- 128 bits of base32, and the reason it is unguessable rather than
    -- sequential: the gallery is a public page showing members' work under
    -- their display names, on a server whose threat model is mass reporting.
    -- Anyone with the link can browse; nobody enumerates their way in.
    slug                TEXT NOT NULL UNIQUE,
    title               TEXT NOT NULL,
    theme               TEXT NOT NULL DEFAULT '',
    phase               TEXT NOT NULL
                        CHECK (phase IN ('announce','submit','vote','results','cancelled')),
    submit_at           TIMESTAMPTZ NOT NULL,
    vote_at             TIMESTAMPTZ NOT NULL,
    results_at          TIMESTAMPTZ NOT NULL,
    max_votes           INT  NOT NULL DEFAULT 3,
    forum_channel_id    TEXT NOT NULL DEFAULT '',
    announce_channel_id TEXT NOT NULL DEFAULT '',
    announce_message_id TEXT NOT NULL DEFAULT '',
    created_by          TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at           TIMESTAMPTZ,
    -- The tally as merlin computed it, so /contest status and a re-announce
    -- never have to ask the Worker again for a number that is already final.
    results             JSONB,
    -- Set when the Worker could not be reached at close. The scheduler
    -- retries; this is what /contest status reads to say so out loud rather
    -- than showing a contest that looks finished and has no winner.
    tally_error         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS contests_guild_phase_idx ON contests (guild_id, phase);

CREATE TABLE IF NOT EXISTS contest_submissions (
    id          TEXT PRIMARY KEY,
    contest_id  TEXT NOT NULL REFERENCES contests (id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL,
    thread_id   TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    author      TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT 'other',
    media_url   TEXT NOT NULL DEFAULT '',
    link        TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    withdrawn_at TIMESTAMPTZ
);

-- One live entry per member. Partial, so withdrawing (deleting your forum
-- post) and posting again is allowed and does not collide with the row that
-- records the first attempt.
CREATE UNIQUE INDEX IF NOT EXISTS contest_submissions_one_live_idx
    ON contest_submissions (contest_id, user_id)
    WHERE withdrawn_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS contest_submissions_thread_idx
    ON contest_submissions (thread_id);

CREATE TABLE IF NOT EXISTS contest_prizes (
    id            TEXT PRIMARY KEY,
    contest_id    TEXT NOT NULL REFERENCES contests (id) ON DELETE CASCADE,
    donor_id      TEXT NOT NULL,
    donor_name    TEXT NOT NULL DEFAULT '',
    title         TEXT NOT NULL,
    details       TEXT NOT NULL DEFAULT '',
    -- AES-256-GCM under MERLIN_SECRET_KEY, via internal/secret, same as
    -- aimod's gateway keys. A Steam key or a Nitro gift link is worth exactly
    -- as much as whoever reads it first, so it is never logged, never
    -- audited, never pushed to the Worker, and NULLed the moment it has been
    -- delivered.
    secret_sealed BYTEA,
    awarded_to    TEXT,
    awarded_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS contest_prizes_contest_idx ON contest_prizes (contest_id);
