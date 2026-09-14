-- The two halves of "a copy of this photo is still held": the same bytes, or the same name
-- taken in the same second. Without them each written-off item scanned every backed-up row,
-- 9 s for a library of 97,000 on the Review page, with every other page queued behind it.
CREATE INDEX media_items_sha256 ON media_items(sha256);
CREATE INDEX media_items_filename_captured ON media_items(filename, captured_at);
