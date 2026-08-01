-- Tracks whether the one-time "run /config setup" onboarding nudge has been
-- sent for a guild (internal/plugins/adminconfig's NudgeIfUnconfigured), so
-- it fires at most once regardless of how many times the bot restarts.
-- NULL means "not sent yet" (or the guild configured itself before the
-- nudge ever got a chance to fire).
ALTER TABLE settings_guild ADD COLUMN onboarding_nudge_sent_at TIMESTAMPTZ;
