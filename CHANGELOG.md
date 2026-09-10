# Changelog

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
