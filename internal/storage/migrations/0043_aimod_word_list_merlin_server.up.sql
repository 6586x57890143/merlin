-- Seeds "the merlin server" (1520884223140036809), where "work" and
-- "employment" are a running joke, so the list is live there from the deploy
-- that ships it. One guild by ID and nothing else: the same words would be
-- vandalism anywhere else. Only fills an empty list, so it never overwrites
-- what an admin there has since set with /aimod words, and only an existing
-- row, since a guild with no aimod_config row has the plugin off anyway.
UPDATE aimod_config
SET word_list = '[{"word": "work", "replacement": "the mines"},
                  {"word": "employment", "replacement": "indentured servitude"}]'::jsonb,
    updated_at = now()
WHERE guild_id = '1520884223140036809' AND word_list = '[]'::jsonb;
