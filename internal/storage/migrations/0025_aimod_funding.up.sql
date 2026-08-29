-- A per-guild tip jar: a wallet address members can donate USDC to, and the
-- balance this bot has observed on it.
--
-- The bot never holds a key for this wallet and never moves money. It reads
-- the balance over a public RPC and renders it next to the OpenRouter credit
-- gauge, so the members whose server is being filtered can see the tank
-- draining and top it up. Buying the credits themselves is still a human
-- clicking through OpenRouter's web checkout: their programmatic crypto
-- purchase endpoint returns 410 Gone, and their auto top-up charges a saved
-- card rather than a wallet.
--
-- A separate table rather than columns on aimod_config, which is deliberate:
-- the poller writes here every 15 minutes, and aimod_config is the hot-path
-- row sitting behind cachingStore. Keeping funding out of it means the
-- funding setters need no cache invalidation and the message path pays
-- nothing for them.
CREATE TABLE aimod_funding (
    guild_id     TEXT PRIMARY KEY,
    address      TEXT NOT NULL,
    -- set_by and set_at are not bookkeeping. Only the guild owner or the
    -- bootstrap operator may point this at a wallet, and the public view
    -- names who set it and warns for 24 hours after a change, because a
    -- silently repointed payout address is the one way this feature can cost
    -- somebody money and nothing on chain can be clawed back.
    set_by       TEXT NOT NULL,
    set_at       TIMESTAMPTZ NOT NULL,
    balance_usd  NUMERIC(14,6) NOT NULL DEFAULT 0,
    received_usd NUMERIC(14,6) NOT NULL DEFAULT 0,
    donations    INTEGER NOT NULL DEFAULT 0,
    -- NULL means never polled. The first poll records the balance as a
    -- baseline and counts no donation, or pointing the bot at a wallet that
    -- already holds money reports a phantom gift on day one.
    checked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
