# Changelog

## 0.3.0 — 2026-09-14

- **Find photos by place.** Type a town, an island or a country into the box above the Photos
  grid and the grid, its timeline and the viewer are narrowed to the photos taken there. Where a
  photo was taken is read once out of the backed-up file itself — the GPS tag a phone writes
  into a JPEG, a HEIC, an MP4 or a QuickTime movie — so it works for everything on disk and asks
  Google for nothing. Six in ten of the JPEGs in the library this was built against carry one,
  nearly every HEIC, four in ten of the videos; a phone with its location off wrote nothing, and
  those photos are not found this way. The name is turned into an area by one query to
  OpenStreetMap, the only server gpb talks to besides Google, sent the name and nothing else;
  the answer is remembered so a name is asked once. `lookup_url` under `[places]` points it at a
  Nominatim of your own, or at nothing to have no search.
- **The files are read by a sweep that starts with the daemon**, one at a time, because on a
  cold disk each is a tenth of a second and there are a hundred thousand of them: the first
  sweep takes hours, the page says how far it has got until it is done, and a photo downloaded
  tonight is read as it lands. The database gained the two coordinates and the date each file
  was read, and a table of the places asked about.

## 0.2.4 — 2026-09-14

- **A smaller re-encode of a photo still there is a copy too.** 0.2.3 left a write-off out of
  review when its bytes were still held under another key, and the queue still showed the
  same pictures: the second key carried the same shot at a smaller size, so the bytes differed
  while the photo did not. Nothing was uploaded to make that happen — the photos have been in
  Google Photos for ten years; the extra keys turned up in Google's own listing weeks into the
  backup and were gone again within days, and what Google was doing there is not known. A
  write-off with the same file name and the same capture second as a photo still there, where
  the copy that stayed is at least as large, is now a copy as well — 60 more in this library,
  and in every one of them the larger file was the one Google kept. Nothing is deleted by any
  of this: the larger copy is on Google and on disk exactly where it was, and the smaller one
  stays on disk as well, written off but never removed. The one case still put up for review
  is the opposite one — Google dropping the larger copy and keeping the smaller — because then
  the best copy of the photo exists only in the backup, which is worth knowing rather than
  having waved through. The 60 already waiting are cleared on the first start.

## 0.2.3 — 2026-09-14

- **A photo written off while an identical copy of it is still there is no longer put up for
  review.** Google's listing sometimes carries the same bytes under two keys for a few days
  and then drops one, and each one dropped was reported as a photo Google had lost.
  In this library, 85 of the 300 written off in the first month were byte-for-byte copies of
  photos still on Google and still backed up; a reader checking one found the photo exactly
  where it had always been. Losing one of two identical files is not a loss. The write-off
  itself is unchanged — the file stays, the state and the date it went are recorded — it is
  only not asked about, and the run's log says how many of the items gone were copies. A copy
  that differs at all, even a smaller re-encode of the same shot, still goes to review. The
  77 already waiting are cleared on the first start.

## 0.2.2 — 2026-09-14

- **A cancelled thumbnail request now gives its turn back.** 0.2.1 had the browser let go of
  the pictures a jump along the rail left behind, and it made no difference, because the
  rate limiter had already handed each of those requests a reservation on arrival and a
  cancelled reservation only returns its turn when nobody arrived after it. The abandoned
  screen still cost its seconds — by cancelling instead of by completing. Eighty cancelled
  waits left a fresh request 7.7 s of ghosts to wait behind. Only a handful of requests may
  now hold a reservation at once; the rest wait in a queue where cancelling costs nothing.
- **Only the rows near the viewport ask for their pictures.** The rows mounted a screen above
  and below, so that scrolling never shows a hole, were asking for theirs at the same moment
  — under HTTP/2 all at once — so the screen you landed on was sharing the budget with two
  you could not see. Those rows now wait until they are scrolled towards.

## 0.2.1 — 2026-09-14

- **A jump along the rail lets go of the thumbnails it left behind.** A browser goes on
  loading an image after its cell has left the document, and thumbnails are throttled to a few
  a second because each is a request to Google — so five jumps along the rail queued five
  screens of pictures nobody was looking at ahead of the one they were, and the page took the
  best part of a minute to fill. A row that leaves the document now parks its unfinished
  images, and a cell that comes back asks again. Two things had to follow: a request the
  browser abandoned is no longer answered or logged, and a cell that waited on the same
  thumbnail as one that scrolled away fetches it for itself rather than inheriting the
  cancellation as a grey square.

## 0.2.0 — 2026-09-14

- **The Photos page and every album are one scrolling grid, with the years down the right
  edge.** Ninety-six thousand items at two hundred a page was four hundred and eighty pages
  with nothing but "older page" between them, and a photograph from 2019 was somewhere past the
  three hundredth. Now the page lays the whole grid out from the months and their counts before
  it has fetched a single cell, so the browser's own scrollbar spans the library; months are
  headed, the month you are in floats at the top, and a rail down the right edge carries the
  years — drag along it and the page lands on any month. Only the rows near the window are
  ever in the page. The pictures are fetched once the page stops moving, because every
  thumbnail is one throttled request to Google and a drag across a decade passes thousands of
  them; while it moves you see grey cells and the month's name, which is the trade Google's
  own scrubber makes. A browser without script gets the pages of two hundred it always did.
- **Picking and the viewer follow the grid over cells the browser never fetched.** A
  shift-click range names its two ends and the store fills it in, so a range from January to
  June works whether or not April was ever on screen; "pick all" was already done that way.
  The viewer's arrows step through the whole grid, fetching what they need, and its counter
  says "1 of 96,439" rather than "1 of 200".
- The store gained an index on capture date — every grid was sorting the whole library to show
  two hundred of it.

## 0.1.6 — 2026-09-14

- **The grid's expand button no longer covers a photo's status marks.** Both lived in the
  cell's top-right corner, so hovering a photo that was gone from Google or waiting for review
  hid its own † and ? behind the ⤢. The button now sits in the picture's bottom-right, the one
  corner nothing else uses — the tick has the top-left and the caption the space below.

## 0.1.5 — 2026-09-14

- **A photo Google has lost now opens from its own file.** The review page said those photos
  were safe on disk, and they were — but opening one showed Google's 256-pixel thumbnail
  stretched to fill the viewer, captioned "not backed up yet". The grid decided whether it held
  a file from the item's state being *done*, and writing an item off replaces that state with
  *missing upstream* while leaving the file exactly where it was. Whether a file exists is what
  the recorded path says, so the grid reads that instead: the backed-up mark, the size and the
  viewer's route to the original all come back for the items the page exists to vouch for.

## 0.1.4 — 2026-09-10

- **An album with nothing in it is no longer read as a broken one.** Google answers an empty
  album with the same page as any other — cursor, album record, the lot — and simply nothing
  where the list of items goes, rather than an empty list. Every decoder here is strict on
  purpose, so that read as a failure, and a library that was entirely backed up finished
  `partial` every night with a real album named as unreadable. 0.1.3's drift reports are what
  found it: the payload skeleton beside the position said this page was well formed, and the
  album's own line said Google had listed it that same afternoon as holding nothing.
- **And a walk that lists nothing now has to agree with Google before anything is written off.**
  Reading a page with no items as an empty album would read the same way if Google ever moved
  the items elsewhere in the page — and taken at face value that would empty every album in the
  backup at once. The album listing at the top of every run carries Google's own count, which
  settles it: nothing listed for an album Google calls empty is an empty album, and nothing
  listed for an album Google says holds twenty-four stops the run as drift. The library walk has
  had a rule of this shape since the beginning, for the reason an account is never empty.

## 0.1.3 — 2026-09-10

- **One album that cannot be read no longer costs the whole backup.** A run walks the followed
  albums in order and used to stop at the first one Google answered strangely — so every album
  behind it, and the library timeline behind them all, went unread. Every run, until somebody
  looked at the log. A run now walks on and finishes `partial`, naming what it could not read.
  Nothing is lost by walking on: a listing that failed returns before the step that works out
  what has been deleted, so an album that was not read keeps everything it held.
- **An album Google has stopped listing is no longer asked for.** Following outlives listing —
  nothing clears an album's sync mode when it is deleted or a share is withdrawn — so its id was
  sent on every run for ever and answered with something that was not a listing. The run now
  says which albums those are and that unfollowing them will clear the notice.
- **Two guards, so that this stays a loud failure where it should be.** If *no* album decoded,
  the decoders are wrong rather than the album and the run still fails as `drift`: a run that
  read nothing, reported as a success, would look exactly like an account with no photos in it.
  And if Google's album listing named nothing at all, the run fails rather than treating every
  followed album as deleted — an album missing from a listing of two hundred has gone, while one
  missing from a listing of none says only that the listing came back empty.
- **Drift reports now say what shape arrived.** The error named the position it walked to and
  nothing else, which reads identically whether Google sent `null`, an empty array, an envelope
  with a hole in it, or an array whose slots have moved — four different bugs with four different
  fixes. It now prints a skeleton of the payload beside the position: arity and types only, never
  a value, so it can go in a log without carrying the library with it. The failing album is named
  too, with what Google last said it held and when Google last mentioned it, which is what tells
  a deleted album, an empty one and real protocol drift apart.

## 0.1.2 — 2026-08-14

- **`verify` counts a file before it says it read it.** The progress line was printed between
  incrementing the checked count and hashing the file, so the intact count trailed by exactly
  one and every boundary read "2000 of 96439 files read, 1999 intact". Cosmetic, and it still
  cost something: the first full sweep of the library was read as having found a bad file when
  it had found none. That sweep re-read all 96,439 backed-up files in three hours eight minutes
  and found every one of them intact.

## 0.1.1 — 2026-08-13

- **An image to pull**, [`sodre90/gpb`](https://hub.docker.com/r/sodre90/gpb), linux/amd64 —
  Google ships Chrome for Linux on that architecture and no other. It carries no part of Google
  Chrome: the container downloads Chrome from Google into its own data volume the first time it
  starts, so what is published here is this project and Chrome still reaches you from Google under
  the terms you accept there. 0.1.0 said no image should ever be published; this is the way to
  publish one honestly, and it costs a 110 MB download on the first start and about 430 MB in the
  data directory. The image is smaller for it — 688 MB against 1.3 GB.
- **`gpb verify`**, which re-reads every file the backup calls done and checks it against the
  hash taken while it was written. It is the only thing that reads a finished file again: a run
  notices a file that has *gone*, but one quietly rotted by a failing disk keeps its name, its
  size and its place in the pool. It exits non-zero when it found anything, so a cron entry needs
  no output parsing; `--repair` puts the damaged files back on the work list and deletes nothing.
  An unreadable file — a bad mount, a permissions error — is reported and never requeued.
- **A weekly copy of the database**, `VACUUM INTO /data/state.backup.db`, taken by the daemon.
  The pool can be listed from Google again if it has to be; what was picked, what was reviewed
  and what every run did cannot. It asks the age of the copy on disk rather than counting from a
  timer, so a daemon restarted with every image rebuild still reaches the deadline, and it stages
  under `.part` so last week's copy is never removed before this week's has worked.
- **Docker Compose**, for the reader who has one of those and not systemd. `docker compose up -d
  --build` — a build rather than a pull, because the image carries Google Chrome and is not ours
  to publish. Paths, port and zone come from the environment so a `git pull` leaves them alone,
  and the file sets `container` itself: Docker, unlike Podman, tells the process nothing about
  being supervised, and without it the settings page hides the Restart button.
- **Two installation guides instead of one**, `deploy/compose.md` and `deploy/quadlet.md`, each
  followable end to end. The shared notes — why Google Chrome and not Chromium, the sandbox
  fallback, the stale profile lock — stay in `deploy/README.md`, which is now the chooser.
- **DESIGN.md brought up to date with what was built**: https as an off-by-default setting and
  the reversal of the plain-HTTP recommendation that preceded it, the Restart button and the
  `container` marker, the health check following the scheme, the version constant, Compose, and
  the deployment section rewritten for a reader who does not have the box it was written on.

## 0.1.0 — 2026-08-13

First tagged release, and the first public one. It has been backing up a real library of around
96,000 items across 181 albums since 2026-08-10.

- **Backups.** Daily on a schedule, resuming an interrupted run rather than starting the day
  again. Files land in a pool laid out by capture date with EXIF and GPS untouched and videos
  untranscoded; `<photos>/albums/` is a symlink view of the same pool.
- **Curation.** Follow an album in full, follow only the items you pick, or leave it alone;
  whole-library backup with an optional "taken since" date. Selections live in the database, so
  paging through a ten-thousand-item album keeps them.
- **The pages.** Album list with sorting, favourites and search; per-album grid with click and
  shift-click picking; live progress while a run works; a history of past runs with counts, bytes
  and outcomes; a review queue for items that appeared in a picked album after it was curated.
- **Sessions.** The Google session is warmed on a schedule, and when that is no longer enough you
  sign in through an embedded browser from a phone without touching the server.
- **Home Assistant.** Twelve sensors, two buttons and a library selector arrive over MQTT
  Discovery as one device; the buttons go through the same one-run-at-a-time rule as the pages.
- **https, optionally.** Off by default. A certificate gpb writes for the address you browse to,
  or one you provide; the address follows the setting, and the container's health check follows it
  too, trusting the certificate the config names rather than a trust store that has never heard
  of it.
- **Restart from the settings page**, since the schedule, the limits and encryption are read once
  at startup. Offered only where something would start gpb again.
- **`gpb version`**, and a version line at the head of every start in the log.

Known gaps: no `verify` sweep over what is already on disk, and no scheduled database backup.
