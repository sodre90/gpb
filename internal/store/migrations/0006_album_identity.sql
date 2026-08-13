-- Forty-nine of this library's entries are bundles of shared photos, and Google names none of
-- them. Listed together they were forty-nine identical rows reading "Shared photos", which is
-- honest and useless. These three columns are what the listing already returns and this store
-- was throwing away: a cover image to look at, and who the photos came from.
--
-- owner_is_account rather than a comparison at read time: only the sync run holds the session
-- that says which gaia id is the reader's, and the difference between "you shared these" and
-- "someone shared these with you" is the whole point of showing the name.
ALTER TABLE albums ADD COLUMN cover_url TEXT NOT NULL DEFAULT '';
ALTER TABLE albums ADD COLUMN owner_name TEXT NOT NULL DEFAULT '';
ALTER TABLE albums ADD COLUMN owner_is_account INTEGER NOT NULL DEFAULT 0;
