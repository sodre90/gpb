-- Web sessions lived in a map in the daemon, so every redeploy signed the user out and dropped
-- them on the login page mid-task. They belong on disk with everything else that has to survive a
-- restart.
--
-- What is stored is a SHA-256 of the cookie value, never the value itself: the row is a bearer
-- token, and this database sits beside the browser profile that a leak would already be about.
CREATE TABLE web_sessions (
    id_hash    TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    last_seen  TEXT NOT NULL
);

CREATE INDEX web_sessions_last_seen ON web_sessions(last_seen);
