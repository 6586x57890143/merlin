-- Per-guild emergency controls over destructive Discord actions, enforced
-- centrally in internal/discordguard rather than at each call site.
--
-- writes_paused stops every mutating Discord call for this guild while
-- leaving read/inspect commands working, so a rotation loop or a runaway
-- sweep can be halted from inside Discord instead of by redeploying.
-- writes_dry_run lets a rotation or sweep run end to end, writing its full
-- audit trail, without touching Discord at all — the pre-launch rehearsal
-- for a feature whose failure mode (permanent channel deletion) has no undo.
--
-- Both default false so an existing guild's behavior is unchanged. The
-- process-wide equivalent is MERLIN_PAUSE_ALL_WRITES, deliberately env-only:
-- it must work when the database is the thing that is broken.
ALTER TABLE settings_guild ADD COLUMN writes_paused BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE settings_guild ADD COLUMN writes_dry_run BOOLEAN NOT NULL DEFAULT FALSE;
