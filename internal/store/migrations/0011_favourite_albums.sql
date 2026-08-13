-- A library of 181 albums has perhaps five the user actually watches. sync_mode says which
-- albums are backed up, which on this account is most of them, and says nothing about which
-- ones matter — so the list has no way to lead with them and a run has no reason to fetch them
-- first. This column is the user's own ordering, and the only album fact here that Google has
-- no opinion about: a refresh writes every other column and must never touch this one.
ALTER TABLE albums ADD COLUMN is_favourite INTEGER NOT NULL DEFAULT 0;
