-- From mid-October 2026 VrseUb answered newly added videos with null where the signed URL
-- belongs, and the download path recorded that as drift until each item reached the failure
-- limit and was written off. pLFTfd now mints those URLs, but nothing offers a written-off item
-- again, so the items that failed for exactly this reason start over. Their last error stays as
-- the record of what happened.
UPDATE media_items SET fail_count = 0
WHERE state = 'failed'
  AND last_error LIKE 'google photos response did not match the expected shape: wanted a signed download URL at VrseUb[1], found nothing%';
