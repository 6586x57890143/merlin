-- internal/plugins/aimod's persistence.
--
-- Deliberately the plugin's own tables rather than columns on
-- settings_guild, for two reasons. The first is the same one that keeps
-- role_jails out of internal/settings: incidents and daily spend are runtime
-- state, not guild configuration. The second is specific to this plugin and
-- is a security argument: settings.Store caches every column it reads in
-- process memory and hands that cache to core.Permissions as GuildAuthData,
-- so a third-party API key living there would be sitting in the same struct
-- the authorization layer reads on every single command. It stays here,
-- encrypted, read only by the plugin that needs it.

CREATE TABLE aimod_config (
    guild_id           TEXT PRIMARY KEY,
    -- AES-256-GCM sealed under MERLIN_SECRET_KEY. NULL means no key
    -- configured, which is a working state: the plugin still runs its free
    -- local rungs and simply never calls a model.
    api_key_sealed     BYTEA,
    -- off      = scan nothing at all
    -- flag     = classify and record, but never touch a message
    -- enforce  = act on the per-bucket action below
    mode               TEXT NOT NULL DEFAULT 'off' CHECK (mode IN ('off', 'flag', 'enforce')),
    -- USD per UTC day, per guild. 0 means "never spend", which leaves the
    -- free rungs working rather than disabling the plugin.
    daily_budget_usd   NUMERIC(12, 6) NOT NULL DEFAULT 0.50 CHECK (daily_budget_usd >= 0),
    -- How long the original text of an actioned message is kept so a mod can
    -- reverse a false positive. 0 = keep nothing. Re-derived on every prune,
    -- never frozen into a per-row column: see aimod.Store.PruneEvidence and
    -- the rotation_archives.delete_after mistake it is avoiding.
    evidence_hours     INTEGER NOT NULL DEFAULT 24 CHECK (evidence_hours >= 0),
    -- Ordered model IDs. Empty means "use the compiled-in defaults", so a
    -- guild that never touches these still works and still tracks whatever
    -- the current defaults are, rather than freezing a model ID chosen on
    -- the day it ran /aimod configure key.
    fast_models        TEXT[] NOT NULL DEFAULT '{}',
    deep_models        TEXT[] NOT NULL DEFAULT '{}',
    exempt_channel_ids TEXT[] NOT NULL DEFAULT '{}',
    exempt_role_ids    TEXT[] NOT NULL DEFAULT '{}',
    -- Members who have asked to be sanctioned even though their rank would
    -- otherwise protect them. Purely additive: nothing reads this to decide
    -- whether to protect anybody. See aimod/optin.go.
    sanction_optin_user_ids TEXT[] NOT NULL DEFAULT '{}',
    -- Whether a confirmed violation, or a member draining the scan budget,
    -- also jails them. See aimod/sanction.go for the escalating ladder.
    -- Defaults to flag: an automatic jail is a real punishment, and a guild
    -- should switch it on deliberately rather than discover it.
    sanction_action    TEXT NOT NULL DEFAULT 'flag' CHECK (sanction_action IN ('off', 'flag', 'jail')),
    -- bucket name -> "off" | "flag" | "rewrite" | "remove". Only the buckets
    -- an admin has actually changed are stored; anything absent falls back
    -- to the compiled-in default for that bucket, so adding a bucket in a
    -- later release does not need a data migration to become active.
    bucket_actions     JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per guild per UTC day. The day is part of the key rather than a
-- reset job: a counter that has to be zeroed on a schedule is a counter that
-- stays wrong when the schedule misses, and this one gates spending.
-- The token counters are not bookkeeping for its own sake: they are what
-- makes /aimod models show able to price a model the guild is not currently
-- using. Cost per message is prompt tokens times that model's prompt rate,
-- and the only half of that this bot cannot look up is how many tokens its
-- own traffic actually costs, so it measures that and asks OpenRouter for
-- the rest.
CREATE TABLE aimod_spend (
    guild_id          TEXT NOT NULL,
    day               DATE NOT NULL,
    spent_usd         NUMERIC(14, 8) NOT NULL DEFAULT 0,
    -- Messages that got past the free local rungs and were sent to a model.
    scanned           INTEGER NOT NULL DEFAULT 0,
    fast_calls        INTEGER NOT NULL DEFAULT 0,
    deep_calls        INTEGER NOT NULL DEFAULT 0,
    fast_prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    fast_completion_tokens BIGINT NOT NULL DEFAULT 0,
    deep_prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    deep_completion_tokens BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (guild_id, day)
);

-- One row per message this plugin acted on. content holds the original text
-- and is what /aimod undo restores from, so it is also the only reason this
-- table holds message content at all; PruneEvidence clears it on the guild's
-- own schedule while leaving the row (the audit fact) in place.
CREATE TABLE aimod_incidents (
    id         BIGSERIAL PRIMARY KEY,
    guild_id   TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    author_id  TEXT NOT NULL,
    bucket     TEXT NOT NULL,
    -- flag/rewrite/remove are what happened to the message. sanction is a
    -- row about the member rather than the message, written by the ladder in
    -- aimod/sanction.go; it is what the next offence counts as a prior.
    action     TEXT NOT NULL CHECK (action IN ('flag', 'rewrite', 'remove', 'sanction')),
    confidence REAL NOT NULL DEFAULT 0,
    reason     TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL DEFAULT '',
    replacement TEXT NOT NULL DEFAULT '',
    undone     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (guild_id, message_id)
);

CREATE INDEX aimod_incidents_recent_idx ON aimod_incidents (guild_id, created_at DESC);
-- Covers the prior-sanction count the escalation ladder runs on every
-- confirmed violation, which is per member and time bounded.
CREATE INDEX aimod_incidents_offender_idx ON aimod_incidents (guild_id, author_id, created_at DESC);
-- Partial: PruneEvidence only ever looks at rows that still hold text.
CREATE INDEX aimod_incidents_evidence_idx ON aimod_incidents (guild_id, created_at) WHERE content <> '';
