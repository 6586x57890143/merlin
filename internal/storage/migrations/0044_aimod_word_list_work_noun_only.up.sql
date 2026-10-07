-- "work" on the merlin server (1520884223140036809) should go as a noun
-- ("at work", "my work") and pass as a verb ("this works", "doesn't work").
-- 0043 seeded it without that, so this marks the existing entry noun_only.
-- Touches only an entry whose word is exactly "work", leaving anything an
-- admin there has added or changed since alone.
UPDATE aimod_config
SET word_list = (
        SELECT jsonb_agg(CASE WHEN e->>'word' = 'work' THEN e || '{"noun_only": true}'::jsonb ELSE e END ORDER BY n)
        FROM jsonb_array_elements(word_list) WITH ORDINALITY AS t(e, n)
    ),
    updated_at = now()
WHERE guild_id = '1520884223140036809' AND word_list @> '[{"word": "work"}]'::jsonb;
