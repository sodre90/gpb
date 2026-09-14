-- Google drops items whose bytes it already holds under another key, and every one of those
-- was put up for review as a photo Google had lost. In a library of 96,000, 85 of the 300
-- written off in the first month were byte-for-byte copies of photos still there and still
-- backed up. Writing off now leaves such a copy out of the queue; this clears the ones
-- already in it. The file, the state and the date it went stay as they were.
UPDATE media_items SET needs_review = 0
WHERE state = 'missing_upstream' AND needs_review = 1 AND sha256 IS NOT NULL
  AND EXISTS (
	SELECT 1 FROM media_items copy
	WHERE copy.sha256 = media_items.sha256 AND copy.media_key != media_items.media_key
	  AND copy.state = 'done');
