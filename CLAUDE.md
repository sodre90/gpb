# Working on gpb

gpb copies a Google Photos library to a disk you own. It drives a real signed-in Chrome
session against the same private endpoints the Photos website uses, because Google removed
the Library API's read scopes on 31 March 2025 and left no supported way to read your own
library.

[README.md](README.md) is the tour. [DESIGN.md](DESIGN.md) is the reasoning, and it is worth
reading before changing anything structural — most of what looks arbitrary in this codebase
is written down there with the measurement that produced it.

## Build and test

```bash
go build ./...
go test ./...              # 534 tests across 15 packages
cd spike && go test ./...  # the Phase 0 protocol spike, a separate module
```

Two suites are skipped by default because they want a real Chrome:

```bash
GPB_SCREENSHOTS=1 go test ./internal/web/ -run Screenshots      # redraw docs/*.png
GPB_BROWSER=1 go test ./internal/web/ -run 'Viewer|AlbumsPage|Timeline'  # questions no rendered HTML answers
```

Running it without a container:

```bash
export GPB_DATA_DIR=./data
go run ./cmd/gpb passwd     # set the web password; the one step with no default
go run ./cmd/gpb daemon     # serves http://localhost:8080
```

Go 1.26.4 or later. `GPB_DATA_DIR`, `GPB_PHOTOS_DIR`, `GPB_CHROME_PATH`, `GPB_NOVNC_DIR` and
`TZ` are the only environment overrides; everything else lives in `$GPB_DATA_DIR/config.toml`.

## Layout

```
cmd/gpb/                 daemon | status | passwd | albums | follow | unfollow | sync | links | verify
internal/auth/           chromedp warmup, session export, headful re-auth stack
internal/config/         config.toml: defaults, validation, the password hash
internal/gphotos/        the batchexecute protocol: listings, downloads, thumbnails
internal/store/          SQLite: albums, items, selections, runs
internal/syncer/         the backup run itself — listing, download, resume, retry, verify
internal/engine/         assembling a run: warm the profile, harvest the session, wire the syncer
internal/links/          <photos>/albums/, the symlink view of the pool
internal/thumbs/         thumbnail fetch and disk cache
internal/geo/            where a file says it was taken, read from its own metadata
internal/places/         a place name to a box on the map, via a Nominatim server
internal/web/            every page, its templates and its stylesheet
internal/homeassistant/  the MQTT bridge and its discovery messages
internal/daemon/         scheduler, keepalive, notify hook, wiring
deploy/                  the two installation guides, the Quadlet unit, the image's entrypoint
design/                  the web design brief, and static previews of every page
spike/                   Phase 0 throwaway protocol spike (separate Go module)
```

## Handling the account

These are not style points. The credential this project holds is larger than the job it does.

- **The Chrome profile in `$GPB_DATA_DIR/profile` is a whole Google account**, not a
  Photos-scoped token. Anyone who can read it can read the account's Gmail. Mode 0700 and a
  non-root container are the actual boundary (DESIGN.md §15).
- **`spike/captures/` holds cookies, tokens and signed media URLs recorded from a real
  account.** Do not read it, copy it, quote it, or let it into a build context. It is
  gitignored in two places on purpose, and excluded from every rsync in the deploy guides.
- **Log names, never values.** Cookie and header names are useful in a log; their contents are
  the session itself. Signed media URLs carry their own authentication — they do not belong in
  a log, an issue, or a conversation.

## Conventions

- **Standard library first.** `flag` for subcommand dispatch and no CLI framework; hand-written
  SQL and no ORM; `//go:embed` for templates and static files.
- **Names over comments.** If a block wants a "does X because Y" comment above it, extract it
  into a function whose name says the same thing. Comments are for the *why* a name cannot
  carry: a measurement, an external constraint, a workaround with a reference.
- **The store owns the database.** It opens SQLite with `SetMaxOpenConns(1)`, WAL and a 10s
  busy timeout; nothing else opens that file.
- **Tests use real things.** Real SQLite files in `t.TempDir()`, real HTTP servers, recorded
  protocol fixtures — not mocks. The fixtures are scrubbed; keep them that way.
- **Prose is written to be read.** The pages, the logs and the docs use full sentences and say
  what actually happened, including when it went badly. Match that rather than the usual
  clipped UI voice.
