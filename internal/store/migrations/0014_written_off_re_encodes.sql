-- 0013 cleared the review queue of write-offs whose bytes are still held under another key.
-- The same library had 60 more that are the same photo as one still there — the same file
-- name, the same capture second, a smaller file, and in every case the larger copy the one
-- that stayed. Nothing had been uploaded to cause it. Writing off now treats those as copies
-- too; this clears the ones already in the queue.
UPDATE media_items SET needs_review = 0
WHERE state = 'missing_upstream' AND needs_review = 1 AND filename != ''
  AND EXISTS (
	SELECT 1 FROM media_items copy
	WHERE copy.media_key != media_items.media_key AND copy.state = 'done'
	  AND copy.filename = media_items.filename AND copy.captured_at = media_items.captured_at
	  AND copy.size_bytes >= media_items.size_bytes);
