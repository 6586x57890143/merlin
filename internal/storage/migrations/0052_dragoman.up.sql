-- dragoman: translated messages (internal/plugins/dragoman). Off until an
-- admin turns it on, so no row means off.
CREATE TABLE dragoman_config (
    guild_id           TEXT    PRIMARY KEY,
    enabled            BOOLEAN NOT NULL DEFAULT FALSE,
    exempt_channel_ids TEXT[]  NOT NULL DEFAULT '{}',
    opt_out_user_ids   TEXT[]  NOT NULL DEFAULT '{}'
);

-- Whose each repost is, so its author can take it down: a webhook's message
-- belongs to nobody in Discord's eyes. IDs only; what the original said is
-- in the audit log, where a guild keeps message text at all.
CREATE TABLE dragoman_translations (
    guild_id   TEXT        NOT NULL,
    repost_id  TEXT        NOT NULL,
    channel_id TEXT        NOT NULL,
    author_id  TEXT        NOT NULL,
    language   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (guild_id, repost_id)
);
CREATE INDEX dragoman_translations_created ON dragoman_translations (guild_id, created_at);
