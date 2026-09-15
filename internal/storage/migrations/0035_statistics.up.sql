-- Hourly activity buckets, counted from the gateway as messages arrive.
--
-- /activity read Discord's history over REST for every report, at about one
-- page of a hundred messages per second per channel, so two weeks of one
-- busy room was a half-hour scan that any deploy killed. Discord already
-- pushes every message to this bot once; counting it then is free, exact,
-- and makes a report over any window a single query.
--
-- What is kept is who posted how many messages in which channel in which
-- hour: metadata, never content, and no finer than the hour. It is still a
-- durable record of who was talking where, which is the thing the operator
-- gate on /statistics report exists to protect, so rows age out on a
-- per-guild retention (stats_config.retention_days, 90 by default) and
-- nothing here is exempt from it.
CREATE TABLE IF NOT EXISTS stats_hourly (
    guild_id   TEXT        NOT NULL,
    channel_id TEXT        NOT NULL,
    user_id    TEXT        NOT NULL,
    hour       TIMESTAMPTZ NOT NULL,
    messages   INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (guild_id, channel_id, user_id, hour)
);
CREATE INDEX IF NOT EXISTS stats_hourly_guild_hour_idx ON stats_hourly (guild_id, hour);

-- Joins and departures per hour. No user id: the question this answers is
-- "is the server growing", and a per-member join log is a different, more
-- sensitive record that nothing here needs.
CREATE TABLE IF NOT EXISTS stats_members_hourly (
    guild_id TEXT        NOT NULL,
    hour     TIMESTAMPTZ NOT NULL,
    joined   INTEGER     NOT NULL DEFAULT 0,
    departed INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (guild_id, hour)
);

-- The display name and avatar last seen on a message, so a report can name
-- people without a REST call per row and a member who has since left still
-- appears as who they were. The newest sighting wins.
CREATE TABLE IF NOT EXISTS stats_users (
    guild_id TEXT        NOT NULL,
    user_id  TEXT        NOT NULL,
    name     TEXT        NOT NULL DEFAULT '',
    avatar   TEXT        NOT NULL DEFAULT '',
    seen_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (guild_id, user_id)
);

-- Channel names, for the same reason: rotation deletes channels on a
-- schedule, and a report over last month should still say where.
CREATE TABLE IF NOT EXISTS stats_channels (
    guild_id   TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (guild_id, channel_id)
);

-- live_since is the instant this bot began counting a guild from the
-- gateway, set once and never moved. A backfill fills history strictly
-- before it, so the two sources never both count one message.
CREATE TABLE IF NOT EXISTS stats_config (
    guild_id       TEXT    PRIMARY KEY,
    retention_days INTEGER NOT NULL DEFAULT 90,
    live_since     TIMESTAMPTZ
);

-- One row per channel queued for backfill. cursor is the message id paging
-- resumes below, advanced only when a whole hour has been written, so a
-- restart re-reads at most one partial hour and double counts nothing.
CREATE TABLE IF NOT EXISTS stats_backfill (
    guild_id       TEXT        NOT NULL,
    channel_id     TEXT        NOT NULL,
    from_at        TIMESTAMPTZ NOT NULL,
    until_at       TIMESTAMPTZ NOT NULL,
    cursor         TEXT        NOT NULL DEFAULT '',
    done           BOOLEAN     NOT NULL DEFAULT false,
    error          TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (guild_id, channel_id)
);
