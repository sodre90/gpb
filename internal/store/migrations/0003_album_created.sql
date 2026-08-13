-- The album listing has always carried a creation date and this store has always discarded
-- it. It is the one attribute that orders albums the way a person remembers them, which
-- neither the title nor the order Google returns them in does.
ALTER TABLE albums ADD COLUMN created_at TEXT;
