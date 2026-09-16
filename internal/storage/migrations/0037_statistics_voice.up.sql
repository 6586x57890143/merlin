-- Voice time, by the hour, in the same shape as stats_hourly.
--
-- Discord keeps no voice history to read back the way a backfill reads
-- messages, so the only way to know who was in voice is to have been
-- watching: the gateway says when a member's channel changes, and the
-- seconds between two such changes are attributed to the hours they fall
-- in. What is kept is that attribution and nothing else: no session log,
-- no join or leave instants, no finer than the hour. It ages out on the
-- same per-guild retention as every other bucket here.
CREATE TABLE IF NOT EXISTS stats_voice_hourly (
    guild_id   TEXT        NOT NULL,
    channel_id TEXT        NOT NULL,
    user_id    TEXT        NOT NULL,
    hour       TIMESTAMPTZ NOT NULL,
    seconds    INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (guild_id, channel_id, user_id, hour)
);
CREATE INDEX IF NOT EXISTS stats_voice_hourly_guild_hour_idx ON stats_voice_hourly (guild_id, hour);
