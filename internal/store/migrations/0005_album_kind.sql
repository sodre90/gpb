-- A boolean could not say what these albums are. Google returns three different things in one
-- listing pair: albums this account owns, albums shared with it (which carry a title only in
-- the shared listing), and bundles of loose shared photos that have no name anywhere. The
-- old flag conflated the last two, which is why 49 bundles were shown as albums.
--
-- Everything already flagged is a bundle: the flag was set from the album listing's own marker,
-- and shared albums were not being fetched at all when it was written.
ALTER TABLE albums ADD COLUMN kind TEXT NOT NULL DEFAULT 'own';

UPDATE albums SET kind = 'bundle' WHERE is_shared = 1;

ALTER TABLE albums DROP COLUMN is_shared;
