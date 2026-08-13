-- Google's listing carries no filename, and until now the media key was written into the column
-- in its place. Every item not yet downloaded therefore had an identifier where its name should
-- be, and every page that shows a name showed that: grid captions, checkbox labels, the viewer.
-- Blanking them puts those pages back on the fallback they already had, the capture date.
--
-- The column is NOT NULL, so the placeholder is cleared to an empty string rather than removed.
UPDATE media_items SET filename = '' WHERE filename = media_key;
