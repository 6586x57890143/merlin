-- A guild's own banned words: [{"word": "...", "replacement": "..."}].
--
-- House style rather than Discord policy, which is why it is per guild and
-- not a bucket in policy/*.yaml: one server wants "work" turned into a joke,
-- and that would be vandalism anywhere else. An empty replacement removes
-- the message instead of rewriting it.
ALTER TABLE aimod_config ADD COLUMN word_list JSONB NOT NULL DEFAULT '[]'::jsonb;
