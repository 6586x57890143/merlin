-- A short random code for a rewritten message, printed under the repost so
-- anybody reading the channel can ask /aimod why about it.
--
-- Random rather than the row id or the message id, and that is the whole
-- reason this column exists: /aimod why answers members, and a guessable
-- key would let any of them walk the table, or paste any message's id to
-- learn whether the filter had quietly flagged it. A code is only ever
-- known to somebody who saw it published. NULL for everything that was not
-- reposted (flags, removals, sanctions).
ALTER TABLE aimod_incidents ADD COLUMN code TEXT;
CREATE UNIQUE INDEX aimod_incidents_code_idx ON aimod_incidents (guild_id, code) WHERE code IS NOT NULL;
