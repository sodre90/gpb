-- Every grid is ordered by capture date, and until now nothing indexed it: a page of the
-- whole-library grid sorted fifty thousand rows to show two hundred. A grid that scrolls the
-- library as one piece asks for a window of it on every stop, and asks for the months above it
-- once per visit, so the order has to be something the database can walk rather than build.
CREATE INDEX media_items_captured_at ON media_items(captured_at DESC, media_key);
