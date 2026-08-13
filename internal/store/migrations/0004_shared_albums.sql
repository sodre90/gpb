-- Google does not send a title for an album shared with this account, so a shared album is
-- indistinguishable from one the user never named. Recording the kind lets the list say which
-- it is instead of showing 49 albums as "untitled".
ALTER TABLE albums ADD COLUMN is_shared INTEGER NOT NULL DEFAULT 0;
