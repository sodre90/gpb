-- mime_type was written by the download path from the response's Content-Type and read by
-- nothing: no page, no query, no command. The one place a content type is ever needed is serving
-- a file to the browser, and that reads the name it was stored under — http.ServeContent takes it
-- from the extension. A listing, which never carries a content type, wrote its own empty value
-- over every recorded one on the next run, so what the column held by the time anyone might have
-- wanted it was NULL for every row a run had walked past.
ALTER TABLE media_items DROP COLUMN mime_type;
