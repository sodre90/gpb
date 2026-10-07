-- Seen-ness was recorded as a timestamp on every row a walk touched, which cost ~98,000 row
-- writes a day to say what the walk already knew in memory: which items it had just walked past.
-- A member is now reconciled from the walk's own seen-set against the membership that was there
-- before it started, so the stamps carry nothing a walk cannot say for itself. The album's own
-- last_seen_at stays: that one is how a walk tells an album Google still lists from one it has
-- stopped listing.
ALTER TABLE album_items DROP COLUMN last_seen_at;
ALTER TABLE media_items DROP COLUMN last_seen_at;
