-- Prize pledges wait for a mod, and the mod never sees the code.
--
-- /contest prize is TierPublic, and a pledge used to go public the instant it
-- was submitted: the row was written, the snapshot pushed and merlin's "prize
-- pledged" line posted to the announce channel in one breath. So anybody in
-- the server could put their own words, under their own name, onto the
-- contest gallery and into a public channel with nobody reading them first.
--
-- reviewed_at IS NULL is pending, and it is the only state test anything
-- needs. A pending pledge reaches neither the gallery nor a winner.
--
-- code_shape is the whole reason this can be reviewed at all. A mod cannot be
-- shown the code -- the sealing exists to keep it from everyone but the
-- winner, and a moderator is not a smaller exception than anybody else -- but
-- a mod shown nothing cannot tell a Steam key from "lol get rekt". So one
-- line describing the code's *shape* is derived at pledge time, in the one
-- function that already holds the plaintext, and stored here: "steam-shaped
-- key, 17 characters", "link to discord.gift", "4 words of prose". The host
-- of a link is not the secret; the token in its path is, and that never
-- leaves the function. Nothing decrypts to review.
--
-- This does not make a prize verifiable. Nothing does, short of redeeming it:
-- a plausible fake still passes, and the answer to that is a human noticing
-- afterwards, not a column.
ALTER TABLE contest_prizes
    ADD COLUMN IF NOT EXISTS reviewed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS reviewed_by TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS approved    BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS code_shape  TEXT    NOT NULL DEFAULT '';

-- Everything pledged before this existed was already on the gallery and
-- already deliverable. Leaving it pending would retroactively withdraw
-- prizes people were publicly promised, on a deploy nobody announced.
UPDATE contest_prizes
   SET reviewed_at = created_at, approved = true
 WHERE reviewed_at IS NULL;
