CREATE TABLE albums (
    id             TEXT PRIMARY KEY,
    title          TEXT NOT NULL,
    item_count     INTEGER,
    sync_mode      TEXT NOT NULL DEFAULT 'none',
    first_seen_at  TEXT NOT NULL,
    last_seen_at   TEXT NOT NULL,
    last_synced_at TEXT
);

CREATE TABLE media_items (
    media_key     TEXT PRIMARY KEY,
    filename      TEXT NOT NULL,
    captured_at   TEXT,
    size_bytes    INTEGER,
    mime_type     TEXT,
    sha256        TEXT,
    state         TEXT NOT NULL DEFAULT 'discovered',
    fail_count    INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    local_path    TEXT,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    downloaded_at TEXT,
    missing_since TEXT,
    needs_review  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX media_items_state ON media_items(state);

CREATE TABLE album_items (
    album_id     TEXT NOT NULL REFERENCES albums(id),
    media_key    TEXT NOT NULL REFERENCES media_items(media_key),
    last_seen_at TEXT NOT NULL,
    PRIMARY KEY (album_id, media_key)
);

CREATE INDEX album_items_media_key ON album_items(media_key);

CREATE TABLE media_selection (
    media_key TEXT PRIMARY KEY REFERENCES media_items(media_key),
    selected  INTEGER NOT NULL
);

CREATE TABLE sync_runs (
    id          INTEGER PRIMARY KEY,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    outcome     TEXT,
    listed      INTEGER NOT NULL DEFAULT 0,
    downloaded  INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0,
    error       TEXT
);
