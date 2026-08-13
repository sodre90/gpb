-- The library is everything the account holds — 122,331 items on this one, against roughly ten
-- thousand across every album. Most of a photo library is in no album at all, and until now
-- those photos were invisible to this program and were never backed up.
--
-- It is followed through an albums row rather than machinery of its own, so sync_mode, the item
-- links, the progress counts, the grid and the review queue all work on it unchanged. Only the
-- listing behind it differs: albums come from F2A0H, the library from the timeline rpc.
--
-- The id is a word rather than the empty string an earlier draft proposed: '' is already the
-- store's answer for "no album covers this item", and a download path that cannot tell those
-- two apart would silently skip every library photo.
ALTER TABLE albums ADD COLUMN since_date TEXT;

INSERT INTO albums (id, title, item_count, sync_mode, kind, first_seen_at, last_seen_at)
VALUES ('library', 'Library', NULL, 'none', 'library',
        strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'),
        strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'));
