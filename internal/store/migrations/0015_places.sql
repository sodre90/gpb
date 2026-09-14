-- Where a photo was taken, read once from the file's own metadata: Google's listing says
-- nothing about place, and the originals in the pool carry their GPS tags untouched. located_at
-- is when the file was read; set with no coordinates, it says the file was looked at and had
-- none, which is different from a file nobody has looked at yet.
ALTER TABLE media_items ADD COLUMN latitude REAL;
ALTER TABLE media_items ADD COLUMN longitude REAL;
ALTER TABLE media_items ADD COLUMN located_at TEXT;

CREATE INDEX media_items_location ON media_items(latitude, longitude);

-- A place name looked up once, kept so that the same search never asks the geocoder twice. A
-- name it did not know is kept too, with no box, so that it is not asked again on every reload.
CREATE TABLE places (
    query        TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    south        REAL,
    north        REAL,
    west         REAL,
    east         REAL,
    looked_up_at TEXT NOT NULL
);
