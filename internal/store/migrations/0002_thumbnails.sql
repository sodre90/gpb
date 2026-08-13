-- The grid must show items the user has deliberately not downloaded yet, so it needs a
-- picture of every listed item, not only the backed-up ones. Both columns come from the
-- listing and are refreshed by it.
ALTER TABLE media_items ADD COLUMN thumbnail_url TEXT;
ALTER TABLE media_items ADD COLUMN is_video INTEGER NOT NULL DEFAULT 0;
