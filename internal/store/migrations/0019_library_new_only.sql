-- Walking the whole timeline every night was half of every run on the library this was measured
-- on — 52,403 items, about 175 pages, 6m49s of a 13m42s listing on 2026-10-07 — to find the
-- handful of photos that were new. new_only lets the walk stop at the first page it already
-- holds instead. weekly_full_walk keeps one whole walk a week, which is what finds a photo
-- uploaded with an old capture date and notices a deletion below where a nightly walk stops, and
-- walked_in_full_at is when that last happened. All three are the library row's alone, like
-- since_date.
--
-- Every walk before this migration was a whole one, so the last of them counts as one.
ALTER TABLE albums ADD COLUMN new_only INTEGER NOT NULL DEFAULT 0;
ALTER TABLE albums ADD COLUMN weekly_full_walk INTEGER NOT NULL DEFAULT 1;
ALTER TABLE albums ADD COLUMN walked_in_full_at TEXT;

UPDATE albums SET walked_in_full_at = last_synced_at WHERE id = 'library';
