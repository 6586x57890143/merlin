-- dragoman's ladder (internal/plugins/dragoman/rungs.go). An empty model is
-- AI moderation's deep stack.
ALTER TABLE dragoman_config ADD COLUMN model TEXT NOT NULL DEFAULT '';

-- What translation itself cost, per rung and UTC day: the budget it draws on
-- is moderation's too, and says nothing about either alone. rung 'skipped'
-- counts messages the pre-filter kept from a model.
CREATE TABLE dragoman_usage (
    guild_id TEXT             NOT NULL,
    day      DATE             NOT NULL,
    rung     TEXT             NOT NULL,
    calls    INTEGER          NOT NULL DEFAULT 0,
    cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (guild_id, day, rung)
);
