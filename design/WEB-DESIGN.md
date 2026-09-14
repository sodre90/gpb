# gpb Web UI — Design Proposal

Status: proposal for review. Nothing in `internal/web/` changes until this is agreed.
A clickable prototype of every screen and state lives in `design/preview/` — open
`design/preview/index.html` from disk; `preview.css` there is the real proposed
stylesheet, written to drop into `internal/web/static/` later.

The constraints this design lives under, restated so the choices below read as
consequences rather than taste: server-rendered `html/template` with no build step and
no web fonts; every action a plain form POST that JS only upgrades in place (the
`savingInPlace`/`renderBlock` pattern in `internal/web/albums.go` and `render.go` is the
law of the land, not a suggestion); one user on a LAN over plain HTTP; ~200 albums,
~45,000 photos, thumbnails arriving one throttled request at a time; and the house rule
that the simplest sufficient thing wins.

## 1. Audit — what is weak today, and why

The current UI is correct and honest — the failure modes it names are real and the
copy explaining them is better than most products manage. What it lacks is structure:
it grew one handler at a time and it shows.

### Information architecture

- **`/` does five jobs at once.** `templates/albums.html` stacks the action row
  (62–69), the last-run report (71–84), the account-wide backup summary (86–114), the
  library card (116–143), the 132-row album table (145–155) and the 49-row bundles
  section (157–176) on one page. Curation (a thing done occasionally, at length) and
  monitoring (a thing glanced at daily) are interleaved, so neither is served: the
  run report is one sentence squeezed above the fold, and the albums start two cards
  down.
- **There is exactly one run of history.** `albums.go:496` reads `RecentRuns(1)`; a
  run older than the latest is unreachable from the UI even though `sync_runs` keeps
  them all and `store.RecentRuns(limit)` already takes a limit. "Did Tuesday's run
  fail too?" cannot be answered.
- **The review queue does not exist**, but the data does: `MarkMissingUpstream`
  (`store/items.go:183–191`) sets `needs_review = 1`, the album rows show "N to
  review" badges (`albums.html:15`), and there is nowhere to click through to. A badge
  that leads nowhere trains the user to ignore it.
- **`/status` and `/reauth` are the same page grown twice.** `reauth.go:19` embeds the
  whole `statusView` inside `reauthView`; both open with the identical session-state
  line (`status.html:3`, `reauth.html:3`). Meanwhile `/status` mixes Google-session
  facts with configuration facts (photos directory, keepalive interval —
  `status.html:9–10`) that belong to settings, and diagnostics (cookie count, user
  agent — `status.html:7–11`) that belong behind a fold.
- **The nav (`layout.html:12–17`) has three destinations** and no room for the pages
  DESIGN.md §10 already commits to (`/review`, `/runs`, `/settings`).

### Visual system

- **There are no tokens.** `app.css:3` defines exactly one custom property (`--line`);
  every colour after it is a hex literal — `#2c7` (100), `#d44` (107, 111), `#2a6`
  (115), `#d92` (277) — repeated in badges, outcomes, marks and the state dot, with no
  light/dark variants. Several fail WCAG AA as text on white (`#d92` ≈ 2.4:1).
- **`.muted { opacity: 0.6 }` (app.css:262–264)** is used for most secondary text,
  including load-bearing numbers ("1,310 to go"). Opacity-based muting lands around
  the AA boundary and dims whatever it wraps, badges included.
- **`.notice` is green (app.css:114–116)** and does triple duty: success confirmation
  ("Album will be backed up in full"), neutral information ("this page updates
  itself" — `albums.html:64`), and empty states ("No albums known yet" —
  `albums.html:154`). A first-run user is greeted by success-coloured text about
  having nothing.
- **One heading size exists** (`h1` at 1.4rem, app.css:31–33); card `h2`s are locally
  restyled (190–194). Buttons are unstyled UA defaults (83–87), so the page's single
  most important action ("Back up now") renders identically to "Log out".
- **The thumbnail placeholder is dark-mode-only**: `thumb.go:148–151` hardcodes
  `#2a2d33`/`#4a4e57`, so in light mode a failed thumbnail is a dark grey hole in a
  white page.

### Feedback and state

- **Live progress is a whole-page `<meta http-equiv="refresh">`** (`layout.html:7`,
  driven by `RefreshSeconds`). Every 5s poll re-renders everything, discards scroll
  position, restarts every lazy image, and cannot be stopped from the page. DESIGN.md
  §10 specified "a short-poll progress block"; the meta tag was the expedient stand-in
  and it shows most on the album list, which is exactly the page someone is trying to
  *use* during a run.
- **Mid-run there are no numbers.** `albums.go:189–204` documents it: only `FinishRun`
  writes counts, so a run that has fetched thousands shows nothing move except
  eventually the per-album "done" column. The one thing a person watching a backup
  wants — items done, items to go — does not exist as data the UI can read.
- **`AUTH_REQUIRED` is invisible outside `/status`.** DESIGN.md §14 promises a
  "banner on every web page"; `pageData` (`render.go:17–24`) carries no session state,
  so no template can render one. A signed-out daemon serves the album list normally —
  with every thumbnail a grey placeholder and no explanation anywhere on the page.
- **An expired web session dead-ends the JS paths.** `rejectUnauthenticated`
  (`server.go:153–160`) answers non-GET with a bare 401, which `albums.js:46` and
  `grid.js:64` surface as "That change was not saved (HTTP 401)" — accurate, useless,
  and the user has no idea logging in again would fix it.
- **Notices travel as URL query text** (`albums.go:206–210`, `redirectWithNotice`
  312–315) — sound PRG mechanics, but the message survives in history and re-appears
  on back-navigation, and the banners (`layout.html:22–23`) have no `role="status"`,
  so in-place updates are silent to assistive tech (`albums.js:76–85` builds the same
  banner dynamically, also without a role).

### Accessibility

- **Picking is mouse-only and JS-only.** Grid cells are bare `<figure>` elements
  (`album.html:49–61`) toggled by click handler (`grid.js:15–29`): no form, no
  checkbox, no keyboard path, no focus indicator, nothing for the no-JS browser. This
  is the one place the app breaks its own progressive-enhancement rule — with JS off,
  `picked` mode cannot be operated at all.
- **State marks are bare glyphs** — ✓ ! † ? in `album.html:52–56` — with `title` as
  the only text alternative, which touch devices and screen readers largely miss.
- **No `:focus-visible` styling exists anywhere**, and `app.css` contains no width
  media query at all: at 360px the six-column album table overflows and the page
  side-scrolls.
- The meta-refresh poll doubles as an accessibility failure: the page reloads under a
  screen-reader user mid-read, on a timer they cannot see or stop.

### Scale

- **181 rows have sorting but no filtering.** `albumsort.go` is thorough about order,
  but finding "that album from the lake weekend" among 132 titles is a scan or a
  Ctrl-F. One text filter would beat three of the sort columns.
- **The grid pager appears only below 200 images** (`album.html:64–70`) — reaching
  page 2 means scrolling past 200 slowly-filling cells.
- Long-list virtues worth keeping and building on: lazy images, the shimmer that
  distinguishes "loading" from "broken" (`app.css:355–388`), server-side select-all
  (`grid.go:83`), and the sorted-followed-first default.

## 2. Information architecture

The reorganising idea: **separate the three activities the app actually serves** —
*is my backup healthy?* (glanced at often, must be instant), *what should be backed
up?* (visited occasionally, at length), and *fix the thing that broke* (rare,
urgent). Today all three share `/`.

### Pages

| Route | Page | Job |
|---|---|---|
| `GET /` | **Overview** | One screen answering "is everything fine": backup totals with a meter, last run verdict, Google session state, review count, the Back up now button. |
| `GET /albums` | **Albums** | Curation: library card, filterable/sortable album table, bundles section. Everything `/` does today minus monitoring. |
| `GET /album/{id}` | **Album** | The picking grid, as today, upgraded (keyboard, no-JS picking, top pager). |
| `GET /runs` | **Activity** | Live progress while a run works; run history table below it. Nav label "Activity", route `/runs` as DESIGN.md names it. |
| `GET /review` | **Review** | The queue: new items in `picked` albums (approve/dismiss) and items gone from Google (acknowledge). |
| `GET /reauth` | **Google** | `/status` and `/reauth` merged: session state, check-now, the sign-in flow, diagnostics behind a disclosure. Keeps the `/reauth` route because `auth_required` notifications already link to it (DESIGN.md §6). `/status` 301s here. |
| `GET /settings` | **Settings** | Schedule, limits, thumbnail cap, notify hook. Designed now, shipped last (needs a config write path that doesn't exist yet). |
| `GET /login` | Login | As today. |

Nav, in this order: **Overview · Albums · Activity · Review · Google · Settings**,
with a count pill on Review when the queue is non-empty. Six items wrap to two rows
at 360px; no hamburger machinery for one user.

Moving albums off `/` touches redirects: `redirectWithNotice` (`albums.go:312`) and
the sort links in `albumsort.go` currently target `/`. Mechanical, but it is the one
slice of this design that moves an existing page rather than adding one.

### What renders on every page (layout changes)

- **Session banner.** When `auth.Status().State == StateAuthRequired`, every page
  carries one danger banner: "Google has signed this session out — backups are
  paused. → Open the Google page." Same slot shows a warning banner after a `drift`
  outcome. Requires `pageData` to gain a `Session` field filled by a shared helper —
  the smallest handler change in this design and the highest-value one.
- **Review count in the nav.** One `COUNT(*) WHERE needs_review = 1` per page render
  (new store query, trivially cheap in SQLite). If even that offends, it can be
  computed only on Overview and the nav pill dropped — the design works either way.

## 3. Page-by-page specification

Conventions used below: every POST works with JS off (PRG with `?notice=`/`?error=`);
`[Button]` is a real submit; data marked **NEW** needs store or runner plumbing that
does not exist yet, everything else is served by existing queries.

### 3.1 Overview — `/`

```
+---------------------------------------------------------------+
| gpb  Overview  Albums  Activity  Review(3)  Google  Settings  |
+---------------------------------------------------------------+
| [! Google has signed this session out - backups are paused.   |  <- only when
|    Open the Google page ->]                                   |     auth_required
+---------------------------------------------------------------+
| Overview                                                      |
|                                                               |
| +-- Backup --------------------------------------------------+|
| | 12 albums and your whole library                           ||
| |                                                            ||
| |  41,203       39,876      1,310        17                  ||
| |  photos       backed up   to go        failed              ||
| |  [#########################----------------]  96.8%        ||
| |  812.4 GB on disk                                          ||
| +------------------------------------------------------------+|
| +-- Last run ------------------+ +-- Google session ---------+|
| | ok - finished today 03:41    | | * Signed in               ||
| | 214 listed - 122 downloaded  | | last checked 06:12        ||
| | All runs ->                  | | Details ->                ||
| +------------------------------+ +---------------------------+|
| +-- Review -------------------- (only when the queue > 0) ---+|
| | 3 items are waiting for a decision.  Review them ->        ||
| +------------------------------------------------------------+|
| [ Back up now ]                                               |
+---------------------------------------------------------------+
```

**States.**
- *First run:* backup card becomes an empty state — "Nothing is being backed up yet.
  → Choose albums" — no run cards, no meter. The page teaches the setup order:
  albums → follow → run.
- *Working:* the Last run card becomes a live card: activity name, moving counts, a
  meter; Back up now is replaced by "a run is in progress" text (same rule as today,
  `albums.html:63–68`).
- *Error:* last run card shows the outcome badge (`partial`/`error`/`drift`) and the
  first line of `sync_runs.error`, linking to Activity for the rest.
- *Interrupted:* the "started and never finished" sentence, as `albums.html:79–82`
  already words it — that copy survives verbatim.
- *Signed out:* danger banner (above) + the Google card goes red; the backup card
  stays, because cached numbers are still true.

**Data.** `store.BackupSet()` ✓, `store.RecentRuns(1)` ✓, `auth.Status()` ✓,
review count **NEW** (one COUNT), live progress **NEW** (§5).

### 3.2 Albums — `/albums`

```
| Albums                                                        |
| [ Back up now ]  [ Refresh album list ]   [Filter: ______ ]   |
|                                                               |
| +-- Your whole library --------------------------------------+|
| | Every photo and video, incl. those in no album.            ||
| | Back up [Everything v] taken since [2019-01-01] [Save]     ||
| | 28,540 found so far - 27,981 backed up - 559 to go         ||
| +------------------------------------------------------------+|
|                                                               |
| |cv| Album            | Created  | Items | Backed up | Backup||
| |--|------------------|----------|-------|-----------|-------||
| |##| Iceland 2024     | 2024-08  |   843 | 843 done  |[All v]||
| |##| Lake weekend     | 2023-07  |   211 | 118 of 130| ...   ||
| |     picked [3 to review]       |       | picked    |       ||
| |##| Graduation       | 2019-06  |    64 | -         |[No v] ||
| 132 albums, 12 under backup.                                  |
|                                                               |
| > Shared photos (49)                        [collapsed link]  |
+---------------------------------------------------------------+
```

Same bones as today — the table, sortable headings, bundles disclosure, library card
all survive. What changes: the run report and backup summary move out (Overview /
Activity own them); a **filter box** appears (JS-only enhancement: hides non-matching
rows by display title, updates the tally; no server round-trip, no handler — with JS
off it simply isn't there, and sorting still works); during a run the page shows a
slim info banner "Backing up — 1,204 listed · 350 downloaded → watch on Activity"
instead of meta-refreshing itself under the user's open `<select>` (the hazard
`albums.go:19–21` polls around today).

**States.** *Empty/first-run:* empty state card — "No albums known yet" with the
Refresh button inside it (today's bare green sentence, `albums.html:154`, promoted to
a component). *Working:* banner as above; buttons disabled-by-absence as today.
*Signed out:* global banner; the list stays fully usable — offline curation against
cached state is a designed property (DESIGN.md §10) worth a sentence in the banner:
"You can still choose albums; the next run applies them."

**Data.** All existing: `Albums()`, `AlbumStatsByID()`, `BackupSet()` (for the
tally), `Runs.Activity()`. Live counts in the banner reuse the §5 progress plumbing.

### 3.3 Album — `/album/{id}`

```
| <- Albums                                                     |
| Iceland 2024                        [shared with you]         |
| 843 items - 128 picked - 715 backed up                        |
| Back up [Picked items v] [Save]   [ Refresh from Google ]     |
|                                                               |
| [ Pick all 843 ]  [ Clear all ]  [ Save picks ]*  *no-JS only |
| Click a photo to pick it. Shift-click picks a range.          |
| Page 1 of 5                                    [older ->]     |
| +----+ +----+ +----+ +----+ +----+ +----+ +----+ +----+      |
| |[x] | |[ ] | |[x]v| |[ ]!| |[x] | |[ ]?| |....| |[x] |      |
| |6/12| |6/12| |6/13| |6/13| |6/14| |6/14| |    | |6/15|      |
| +----+ +----+ +----+ +----+ +----+ +----+ +----+ +----+      |
|                          ...                                  |
| [<- newer]        Page 1 of 5                    [older ->]   |
```

The grid keeps its cell anatomy (thumb, marks, tick, caption) and gains three things:

- **A real checkbox per cell.** The cell becomes `<label><input type="checkbox"
  name="pick" value="{key}">…</label>` inside one form; CSS drives the picked ring
  from `:has(:checked)` and the focus ring from `:has(:focus-visible)`. That single
  change buys the keyboard path (Tab + Space), the no-JS path (a `[Save picks]`
  button that submits the page's checkboxes; JS removes the button and posts batches
  exactly as `grid.js` does now), and visible focus — for zero new JS. The no-JS
  handler applies the diff *within the rendered page's keys only*, which
  `store.MediaKeysIn` already half-implements; the handler must derive the page's key
  set server-side from the page number rather than trust a hidden field.
- **The pager at both ends**, with the item/picked/backed-up tally in the header so
  the numbers survive scrolling.
- **Marks with text equivalents**: each mark gains a visually-hidden word
  (`<span class="sr-only">backed up</span>`) beside the glyph.

**States.** *Not walked yet:* today's auto-listing (`album.go:97–109`) is good
design — keep it, but render a **skeleton grid** of shimmering placeholder cells
under the "Fetching this album's contents" banner instead of a blank page, so the
wait looks like loading rather than emptiness. *Truly empty:* "Google says this album
is empty" empty state. *Mode `all`:* today's explanatory sentence as an info banner.
*Mode `none`:* likewise. *Signed out:* global banner; cached thumbnails still render,
missing ones get the placeholder; picking still works (selection is local state).

**Data.** All existing (`Album`, `AlbumPage`, `SelectionIn`, `AlbumItemCount`). The
per-album "backed up" figure for the header is in `AlbumStatsByID()` — or a
single-album variant if fetching the whole map rankles.

### 3.4 Activity — `/runs`

```
| Activity                                                      |
| +-- Now ------------------------------------------------------+
| | Backing up - listing "Iceland 2024"                         |
| | 1,204 listed - 350 downloaded - 2 failed                    |
| | [######------------------]                                  |
| +-------------------------------------------------------------+
| History                                                       |
| | Started         | Outcome   | Listed | Down | Fail | Size   |
| | today 03:41     | ok        |  4,211 |  122 |    0 | 1.2 GB |
| | yesterday 03:12 | partial   |  4,198 |   87 |    3 | 800 MB |
| |   3 downloads failed after retries: <first error line>      |
| | 2026-08-05      | interrupted - the daemon stopped mid-run  |
| | 2026-08-04      | auth_required                             |
```

Outcomes render as badges in the status colours; `partial`/`error`/`drift` rows get
their `error` text as an indented second line. A run with null `finished_at` renders
as **"still going"** when `Runs.Activity() != ""` and **"interrupted"** otherwise —
precisely the distinction `albums.go:495–512` already computes; it moves here.

**States.** *No runs yet:* empty state — "No runs yet. The first one happens at
03:30, or [Back up now]." *Working:* the Now card, polling (§5). *Idle:* Now card
absent.

**Data.** `RecentRuns(50)` ✓ (limit already a parameter). The Now card needs live
counters — **NEW**, §5.

### 3.5 Review — `/review`

```
| Review                                                        |
| New items appeared in albums you pick from. Approve to back   |
| them up on the next run; dismiss to leave them out.           |
|                                                               |
| +-- Lake weekend - 4 new -----------------------------------+ |
| | [cells with checkboxes, as the album grid]                | |
| | [ Approve selected ] [ Dismiss selected ] [ Select all ]  | |
| +-----------------------------------------------------------+ |
|                                                               |
| Gone from Google                                              |
| These are still safe on disk; Google no longer has them.      |
| +-- Iceland 2024 - 2 gone ----------------------------------+ |
| | [cells] ... [ Acknowledge ]                               | |
| +-----------------------------------------------------------+ |
```

Two queues share the flag today (`needs_review` is set both by new-in-picked items,
DESIGN.md §4, and by `MarkMissingUpstream`, `store/items.go:185`), but they demand
different verbs, so the page separates them by item state: `discovered` + flagged →
approve/dismiss; `missing_upstream` + flagged → acknowledge (which only clears the
flag; the file stays, as the retention policy demands). Grouped by album, each group
one form, reusing the grid cell component.

**Data — NEW store queries**, all cheap over existing columns:
`ItemsNeedingReview()` (flagged items joined to their albums),
`ClearNeedsReview(keys)`; approve = existing `SetSelection(keys, true)` + clear.
**NEW handlers:** `GET /review`, `POST /review/resolve`.

### 3.6 Google — `/reauth`

```
| Google                                                        |
| * Signed in - last confirmed today 06:12                      |
| Keepalive checks every 12h; the next one confirms on its own. |
| [ Check now ]                                                 |
| > Diagnostics                                                 |
|   cookies held 38 - Chrome sandbox on - user agent Mozilla/...|
+---------------------------------------------------------------+
   -- signed out --
| * Needs sign-in - since 2026-08-09 22:41                      |
| Backups are paused. Signing in opens a real Chrome on the     |
| server, shown here; you finish Google's login in it, phone    |
| prompt and all. It closes itself when you're done.            |
| +-- Confirm your password to open it -----------------------+ |
| | Password [__________]  [ Start browser login ]            | |
| +-----------------------------------------------------------+ |
   -- browser open --
| Login browser started 22:47. Sign in below, then click Done.  |
| +-----------------------------------------------------------+ |
| |            [ noVNC canvas fills this frame ]              | |
| +-----------------------------------------------------------+ |
| [ Done - close the browser ]    closes after 15m idle         |
```

Everything `reauth.go` implements survives; this is presentation only: the status
duplication collapses to one header, the config facts (`PhotosDir`,
`KeepaliveEvery`) move to Settings, and cookie count / UA / sandbox go behind a
`<details>` disclosure — real diagnostics, wanted rarely. The
no-remote-canvas (host display) and unavailable variants keep their current copy.
No auto-refresh while the canvas is open, exactly as `reauth.go:159–163` guards now.

**Data.** All existing (`statusView`, `ReauthStack`).

### 3.7 Settings — `/settings`

One form, four groups mirroring `config.toml` (§12): Schedule (sync time, keepalive),
Limits (workers, req/s), Thumbnails (cache cap), Notifications (hook command). Each
field notes when it applies ("from the next run"). Save = POST, PRG. **NEW:** a
`config.Save` that rewrites the TOML (only `gpb passwd` writes it today) — which is
why this page ships last; the design reserves the space so the IA doesn't shift when
it lands.

### 3.8 Login

Unchanged behaviourally; visually becomes a centred card with the app name above it,
using the same components. Error and lockout messages render in the standard danger
banner.

### 3.9 The timeline — `/photos` and `/album/{id}` with script (added 2026-09-14)

The audit's "reaching page 2 means scrolling past 200 cells" got worse, not better,
once `/photos` existed: fifty thousand items is 262 pages, and a photograph from 2019
was somewhere around page 180 with nothing but "older page" to get there. So a grid
page is now two things. Without script it is what §3.3 describes — page 1, a pager,
a form of checkboxes. With script the page becomes **the whole grid as one piece**:

```
+---------------------------------------------------------------+---+
|  [ July 2021 ]                     <- floating month, sticky  |2025
|                                                               |2024
|  July 2021                         <- month heading           |2023
|  +----+ +----+ +----+ +----+ +----+ +----+ +----+ +----+      |
|  |    | |    | |    | |    | |    | |    | |    | |    |      |2022
|  +----+ +----+ +----+ +----+ +----+ +----+ +----+ +----+      |
|  +----+ +----+ +----+ +----+ +----+ +----+ +----+  [July 2021]| o  <- mark, label
|  |    | |    | |    | |    | |    | |    | |    |             |
|  +----+ +----+ +----+ +----+ +----+ +----+ +----+             |2021
|                                                               |
|  June 2021                                                    |2020
|  +----+ +----+ +----+ +----+ +----+ +----+ +----+ +----+      |
+---------------------------------------------------------------+---+
```

**How it is laid out.** The server puts the grid's months and their counts on the grid
element as JSON, in the grid's own order (newest first on `/photos`, oldest first in an
album — the order §3.3 chose, kept). The script measures one cell and one heading,
works out the columns from the grid's own computed track list, and places every row —
headings and cell rows — at a computed top inside a container of the total height. The
browser's own scrollbar therefore spans the whole library before a single cell has been
fetched. Only rows within a viewport's height of the window are in the document; the
rest are numbers.

**How it is filled.** Cells come in windows of 200 from `/photos/cells` and
`/album/{id}/cells`, rendered through the same `gridcell` block as the page, so a cell
placed by script is byte-for-byte what a reload draws — `data-held`, marks and ticks
included. The page's own 200 are the first window held. A row whose window has not
arrived shows blank cells with the shimmer from §3.3. Windows far from the viewport are
let go after forty are held.

**The rail.** Fixed to the right edge while the grid is on screen and taller than
one-and-a-half windows; the page keeps 3.5rem clear of it. Years down the track where
their first item falls, skipping any that would land on the one before; a mark for the
viewport; a label naming the month under the pointer. A drag lands on the month's
heading. `touch-action: none`, so on a phone a finger on it moves the mark and not the
page; below 40rem the years go and the label under the finger is what names the month.

**The rule that shapes everything else.** A thumbnail is one throttled request to
Google (§11 of DESIGN.md), so a drag across a decade must not queue a minute of
traffic for photographs nobody looked at. Nothing is fetched while the rail is held or
within 150ms of the page moving: the rows show blank and the labels say where you are,
and the pictures come when the page stops. This is the same trade Google's own scrubber
makes, for the same reason.

**What had to change to keep §3.3's promises.** A shift-click range used to be the DOM
cells between two others; on a timeline those may not exist, so the request names its
two ends and `store.SelectRange` fills it in, in grid order, undated items and all — the
browser paints the cells it has and the rest arrive from the server already ticked.
"Pick all" was already server-side. The viewer stepped through a DOM snapshot; it now
steps through a source with a length and an `at(index)` that fetches, so ← from the
first photograph is the last of 52,245 and the counter says so.

**Not done, deliberately.** No keyboard path along the rail: the month headings, the
floating month and the page's own scrolling are the keyboard's route, and a rail that
took arrow keys would take them from the viewer. No day headings: a month is the unit a
person remembers; a day is what the caption is for.

## 4. Visual system

Defined once in `preview.css` (→ future `app.css`). Everything below exists in the
prototype; token values were machine-checked for WCAG AA as text against both
surfaces (and the status set through a CVD validator — the amber/red pair is
distinguishable to deutans only by label, which is why no status colour ever appears
without a word or glyph beside it).

### Colour tokens

Neutrals derive from `currentColor` mixes (the existing trick — it keeps them correct
in both schemes for free); chromatics get explicit light/dark pairs via
`light-dark()`, which the app's `color-scheme: light dark` already opts into.

```css
:root {
  color-scheme: light dark;
  --bg:      light-dark(#fdfdfc, #15171b);   /* pinned, so contrast is knowable */
  --surface: light-dark(#ffffff, #1c1f24);   /* cards */
  --ink:     light-dark(#1b1d21, #e8e8e6);
  --muted:   light-dark(#5d6167, #9a9ea6);   /* replaces opacity: .6 — 6.1:1 / 6.7:1 */
  --line:  color-mix(in srgb, currentColor 18%, transparent);
  --tint:  color-mix(in srgb, currentColor 5%, transparent);
  --ok:     light-dark(#1a7f4b, #4cc38a);    /* 4.9:1 / 8.1:1 */
  --warn:   light-dark(#9a6700, #e2b93d);    /* 4.8:1 / 9.6:1 */
  --danger: light-dark(#c0392b, #e5726a);    /* 5.3:1 / 5.9:1 */
  --accent: light-dark(#15619e, #6cb2e8);    /* 6.4:1 / 7.8:1 — links, primary button, meter */
}
```

Soft washes for banners/badges are `color-mix(in srgb, var(--x) 12%, transparent)` —
no second set of tokens to keep in sync. Semantic mapping is fixed: `--ok` =
done/success only; `--warn` = needs attention, not broken (review, partial, drift,
"nothing picked"); `--danger` = broken or paused (failed, error, auth_required, gone
from Google); `--accent` = interactive, never a status. The placeholder SVG
(`thumb.go:148`) should move to currentColor-based fills so it stops being
dark-only — one string change, noted for the implementation.

### Type scale

System stack as today (`system-ui, sans-serif`). Five steps, no more:

| Token | Size | Used for |
|---|---|---|
| `--t-caption` | 0.75rem | grid captions, badges, marks |
| `--t-small` | 0.875rem | secondary text, table meta, nav |
| `--t-body` | 1rem | everything else |
| `--t-h2` | 1.125rem | card headings |
| `--t-h1` | 1.375rem | page title |

Stat values on Overview use `--t-h1` with `font-variant-numeric: tabular-nums`
(already the habit — `app.css:238–247`). No font weights beyond 400/600.

### Spacing, borders, elevation

Spacing scale `--s-1..--s-6` = 0.25 / 0.5 / 0.75 / 1 / 1.5 / 2.5rem; components use
these, not ad-hoc values. Elevation is **borders only** — `1px solid var(--line)` and
a `--surface` fill; no shadows (a LAN utility does not need depth theatre, and
shadows are the first thing to look wrong across light/dark). Radius: 8px cards,
4px small elements (badges use full round as today).

### Components

| Component | Where it appears |
|---|---|
| **Top bar** | every page; current page marked `aria-current="page"` |
| **Banner** — `info` / `success` / `warn` / `danger`; `role="status"` (`alert` for danger) | session banner, PRG notices, run-in-progress notes, empty-list hints. Splits today's overloaded `.notice` |
| **Card** | Overview cards, library card, review groups, forms |
| **Stat row** | Overview backup card, album header; value+label pairs, tabular numerals, label carries the semantics not the colour |
| **Meter** | backup completion, live run progress; accent fill on tint track, `<div role="progressbar">` with aria values; indeterminate variant (soft pulse, disabled under `prefers-reduced-motion`) for phases with no denominator |
| **Table** | albums, run history; `.kv` variant for label/value pairs (Google diagnostics) |
| **Badge** | neutral (shared with you), warn (to review, nothing picked, partial), danger (gone from Google, failed), ok (run ok) |
| **Button** | one `.primary` per page at most (Back up now, Start browser login); default bordered secondary; `.btn-link` for logout |
| **Select / input / date** | inherit font, token borders, same height as buttons so form rows align |
| **Grid cell** | album grid + review queue; checkbox+label anatomy from §3.3; shimmer while loading (existing, kept) |
| **Empty state** | dashed-border card, one sentence, one action; first-run screens, empty album, empty review, no runs |
| **State dot** | Google session line (kept from `app.css:96–108`, tokenised) |
| **Pager** | grid top and bottom, for a browser without script; the script removes it |
| **Timeline** | a grid page with script: rows placed by measurement, month headings, the floating month; §3.9 |
| **Rail** | the years down the window's edge, the mark, the label under the pointer; §3.9 |
| **Disclosure** | bundles section (URL-driven link, as today — the reasoning in `albumsort.go:169–181` stands); `<details>` for Google diagnostics, where nothing inside reloads the page |

## 5. Interaction and feedback model

**What saves instantly (JS present):** album mode per row (existing
`albums.js` pattern — unchanged), picks (existing `grid.js` pattern, now driving
checkboxes), review approve/dismiss (same batch-POST shape as picks). Every one of
these keeps its no-JS twin: visible submit buttons that JS removes, PRG on the
server. The established contract — *server renders the block, script swaps it in*
(`renderBlock`, `writeSavedRow`) — is how every new in-place update is built too.

**What always takes a button:** anything that starts Google traffic or changes scope
non-reversibly-cheaply — Back up now, refreshes, the library mode+date pair (the
half-typed-date argument in `albums.js:1–6` is correct and keeps its Save), warmup,
re-auth start/stop, settings. Buttons for running work render disabled-with-reason
while `Runs.Activity()` is non-empty, as today.

**Progress while a run works.** The missing piece is data, not markup: the runner
keeps in-memory counters the syncer already tallies for its final report — **NEW:**
`Runs.Progress()` returning `(activity, listed, downloaded, failed)`, no schema
change, reset at run start. Served two ways from one template block:
- a fragment endpoint (`GET /runs/live`, rendering the "Now card" block via
  `renderBlock`) that ~15 lines of JS polls every 5s while a run is active, swapping
  the block in place — scroll, focus and open selects survive;
- with JS off, Overview and Activity keep the `<meta refresh>` fallback (both are
  read-only pages where a reload costs nothing). The album list drops its auto-refresh
  entirely — it shows the slim banner linking to Activity instead, so the one page
  full of open `<select>`s never reloads under the user. The album grid keeps
  refresh only in its cell-less listing state, as `album.go:81–86` guards now.

**How errors surface.** Three tiers: (1) the global session banner —
`auth_required`/`drift` — on every page, dismissable never, because it names why
everything else is quiet; (2) page-level PRG banners for action outcomes, with
`role="status"` and, for danger, `role="alert"`; (3) in-place JS failures reuse the
revert-and-report pattern (`grid.js:60–65` — keep it, it is right) upgraded twice: the
banner gets a live-region role, and a 401 response redirects to
`/login?next=<here>` instead of reporting a dead "HTTP 401".

**What auto-refreshes:** only live-run progress, only while running, by the rules
above. Nothing else on a timer. Thumbnails keep their lazy + shimmer + settle model
(`images.js`) untouched.

## 6. Accessibility and responsiveness

**At 360px:** the album table drops its Created and Items columns (CSS only —
they are the two a phone user can live without; sorting by them still works from the
remaining headers' links if ever needed, but the headers hide with the cells); title
wraps; the mode select and its cell stay. The grid tightens to `minmax(6.5rem, 1fr)`
(3 columns). Cards stack. Nav wraps to two rows. Stat rows wrap 2×2. The noVNC frame
goes `height: 60vh`. All of it is media-query CSS; no markup forks.

**Keyboard:** every action is a link, button, select or checkbox — the picking
checkboxes (§3.3) close the one gap. Focus is visible everywhere via one rule
(`:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px }`), cells
included through `.cell:has(:focus-visible)`. A "skip to content" link precedes the
nav. Tab order is document order; nothing repositions on focus.

**Screen readers:** banners get live-region roles; marks get visually-hidden words;
the meter is a `role="progressbar"` with real values; tables keep real `<th>`s; the
sort indicators become `aria-sort` on the `<th>` (the arrow glyph stays for eyes).
The nav Review pill reads "Review, 3 waiting" via `aria-label`.

**Contrast:** every token in §4 clears 4.5:1 as text on both surfaces (measured, not
assumed). Status is never colour-alone: badges and outcomes are words, marks carry
hidden text, the picked ring pairs with the tick glyph.

**Motion:** `prefers-reduced-motion` already kills the shimmer and the saved-row
flash (`app.css:166–170, 384–388`); the new indeterminate-meter pulse and any
banner transition join the same block. Meta-refresh — motion of the rudest kind — is
confined to the no-JS fallback on read-only pages.

## 7. Staged implementation plan

Ordered so each slice ships alone and the ones before it never have to be redone.
"CSS-only" means no Go or template-logic risk (template class-name edits allowed).

| # | Slice | Touches | Size |
|---|---|---|---|
| 1 | **Tokens + components pass**: rewrite `app.css` as `preview.css` is written — tokens, banner/badge/button/card/empty-state components, focus-visible, muted-as-colour, placeholder SVG to currentColor. Existing pages immediately look intentional. | CSS (+ placeholder string in `thumb.go`) | one sitting, cheap, no risk |
| 2 | **Responsive + a11y floor**: 360px rules, table column drops, grid tightening, pager duplication, sr-only mark words, live-region roles, skip link. | CSS + small template edits | one sitting, low risk |
| 3 | **Global session banner**: `pageData.Session`, one shared fill helper, banner in `layout.html`. Small, but it repays the most: the signed-out state stops being invisible. | handlers (small) + template | short sitting |
| 4 | **Activity page** `/runs`: history table from `RecentRuns(50)`, interrupted-vs-going logic moved from `albums.go`, nav entry. No live card yet. | new handler + template | one sitting |
| 5 | **IA split**: albums to `/albums`, Overview at `/` from existing queries (+ review COUNT), redirects/sort links updated, nav finalised. | handlers + templates | one sitting, the one slice that moves things |
| 6 | **Live progress**: `Runs.Progress()` counters in the runner/syncer, `/runs/live` fragment, poll JS, meta-refresh demoted to fallback, album-list refresh dropped for the banner. | runner + syncer + handler + JS | one sitting; the only slice touching the engine |
| 7 | **Grid picking upgrade**: checkbox anatomy, no-JS save handler (page-scoped diff), keyboard/focus, skeleton listing state. | template + `grid.js` + one handler | one sitting |
| 8 | **Review queue**: store queries, page, resolve handler, nav pill. | store + handler + template | one sitting |
| 9 | **Google page merge**: fold `/status` into `/reauth`, diagnostics disclosure, `/status` redirect. | handlers + templates | short sitting |
| 10 | **Albums filter box**: the JS-only row filter. | one small JS file | short sitting, anytime after 5 |
| 11 | **Settings**: `config.Save` + page. Last because it alone needs new config plumbing (DESIGN.md Phase 4 territory). | config + handler + template | one sitting |

Slices 1–2 are pure polish and could ship today; 3 is the first behavioural change
and the one to do next regardless of how far the rest gets.
