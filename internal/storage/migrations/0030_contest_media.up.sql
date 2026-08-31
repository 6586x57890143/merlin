-- Contest entries keep every attachment, not just the first.
--
-- readThread took msg.Attachments[0] and dropped the rest, so somebody who
-- posted four drawings had three of them silently ignored by the gallery
-- while still being visible in the forum thread they came from. The page
-- also rendered media OR text, never both, so a drawing with a caption lost
-- the caption. Neither is a decision anybody made; both fell out of the
-- single-URL column this replaces.
--
-- media_url stays and keeps holding the first URL. It is what the previous
-- release reads, so leaving it populated means a rollback still renders
-- something rather than an entry with no art at all.
ALTER TABLE contest_submissions
    ADD COLUMN IF NOT EXISTS media_urls TEXT[] NOT NULL DEFAULT '{}';

-- Backfill so existing entries do not lose the art they already had.
UPDATE contest_submissions
SET media_urls = ARRAY[media_url]
WHERE media_url <> '' AND cardinality(media_urls) = 0;
