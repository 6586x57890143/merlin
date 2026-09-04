-- Member opt-out from AI moderation.
--
-- Two columns rather than one, and the split is the whole safety argument.
--
-- member_opt_out is the guild's switch and it defaults to FALSE, which is
-- what makes every existing deployment behave exactly as it did before this
-- migration ran. While it is false the list below is inert: it is still
-- stored, still shown, and read by nothing on the message path. Only the
-- guild owner or the bootstrap operator may flip it (aimod.ownerOrOperator),
-- deliberately narrower than TierAdmin for the same reason the tip jar's
-- address is: this decides how much of the server gets looked at, and a
-- guild with five admins would otherwise have five accounts able to hollow
-- out its own moderation one command at a time.
--
-- opt_out_user_ids is who has taken the guild up on it. Members add and
-- remove themselves (/aimod opt-out); nobody adds anybody else, because
-- consent is not something a third party gives. Kept when the switch goes
-- off rather than cleared, so turning it back on restores what people chose
-- rather than silently re-enrolling everybody.
ALTER TABLE aimod_config
    ADD COLUMN member_opt_out     BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN opt_out_user_ids   TEXT[]  NOT NULL DEFAULT '{}';
