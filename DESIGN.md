# Google Photos Backup — Design Document

Status: built, running, and released as **0.1.0** on 2026-08-13 under **Apache-2.0**, against a
real library of ~96,000 items across 181 albums. The binary is `gpb` and the name is settled.

This stays a design document rather than becoming a manual: it is the reasoning, including the
parts of it that turned out to be wrong, and sections carry their own *Built* notes recording
what building found. README.md is the tour; installation is `deploy/compose.md` or
`deploy/quadlet.md`, whichever way in you take.

One consequence of the release runs through the whole document: it is written for one box on one
LAN, and other people now read it. Where a decision was measured on that box the measurement
stays — it is the evidence — but the box's own address, paths and ports have moved out of
`deploy/` and into the reader's hands.

## 1. Problem statement

Google Photos holds the canonical copy of the user's photo library. There is no longer any
sanctioned way to continuously back it up:

- **Library API — dead for this purpose.** On 31 March 2025 Google removed the
  `photoslibrary.readonly`, `photoslibrary`, and `photoslibrary.sharing` OAuth scopes. The
  surviving scopes (`appendonly`, `readonly.appcreateddata`, `edit.appcreateddata`) only see
  media the calling app itself uploaded. An app can no longer read the user's existing
  library or albums at all. `gphotos-sync` was archived by its maintainer for exactly this
  reason. Even before the scope removal the API was not a true backup: it transcoded video
  and stripped GPS EXIF from downloads.
- **Picker API — interactive only.** Requires a per-session human selection in a Google UI;
  cannot drive unattended periodic sync.
- **Takeout — rejected.** Whole-library only, roughly two-month minimum practical cadence,
  multi-hundred-GB archives to download and unpack each time, and metadata scattered into
  sidecar JSON that must be re-merged. Unfit for incremental, curated backup.

What remains is the Google Photos **web application**, which a logged-in browser can use
freely and whose Download action yields the true original bytes (full EXIF including GPS,
untranscoded video). This project scrapes that surface — deliberately, with eyes open.

### Risks, stated plainly

- **Undocumented internal endpoints.** The web app speaks Google's private `batchexecute`
  RPC protocol. Google can and does change RPC ids, payload shapes, and page structure
  without notice. Every such change is a breakage we must detect and fix by hand. This
  project has a permanent maintenance tail; it is not write-once software.
- **ToS grey area.** Automating one's own account through the web UI is contrary to a
  literal reading of Google's ToS. Worst realistic case is account-level friction:
  captchas, forced re-login, or (unlikely at polite request rates against one's own data)
  account action. The design throttles hard and mimics a normal browser to stay boring.
- **Bot detection at login.** Google sometimes refuses logins from automated or unusual
  browsers ("This browser or app may not be secure"). Mitigations in §6; residual risk
  remains.

The user has accepted these trade-offs; the design's job is to contain them (single
volatile protocol package, drift detection, loud failure, easy re-auth), not to eliminate
them.

**Since 0.1.0 that paragraph has a second audience.** Whoever runs this takes the same risks on
their own account, so the README states them rather than leaving them to be discovered: the
endpoints are undocumented and change without notice, a run that breaks after such a change is
the expected failure mode, and whether an account is ever challenged for looking automated is
Google's call. None of it touches the licensing — the ToS is a contract between an account holder
and Google, not a claim on this code, and nothing here circumvents authentication: the user signs
in themselves, in a real browser, to their own account. The one genuine distribution constraint
is Google Chrome (§13), which is why the published image contains none of it and fetches it from
Google on the first start.

### Prior art

- `perkeep/gphotos-cdp` and the maintained `spraot/gphotos-cdp` fork (Apache-2.0, active
  May 2026): drive Chrome via CDP end-to-end. Persistent Chrome profile dir for auth
  (`chromedp.UserDataDir`), `--disable-blink-features=AutomationControlled`, navigation via
  `photos.google.com/photo/{id}` and `/album/{id}` URLs plus arrow-key traversal, downloads
  by triggering the UI Download action with `browser.SetDownloadBehavior` routing files to a
  directory, download completion tracked via `EventDownloadWillBegin`/`EventDownloadProgress`.
  Logout detected via `signin/rejected` URL and login timeout. Proves the auth model and the
  originals-quality claim, but doing *everything* through the browser is slow and fragile —
  hence our hybrid.
- `xob0t/Google-Photos-Toolkit` (userscript): drives the internal `batchexecute` API
  directly from a logged-in browser — enumerating the library by upload/taken date, listing
  albums and album contents, item metadata, trash operations. Proves that **listing over
  plain HTTP with session cookies is feasible** and documents much of the protocol. This is
  the main reference for our protocol module.

## 2. Fixed requirements (already decided)

1. Hybrid access: real browser (CDP) only to establish/refresh the session; plain Go HTTP
   client for listing and downloading.
2. Go.
3. Interactive curation via a **web UI** served by the daemon itself: select albums and
   individual items to sync from any browser on the LAN. (This replaces the earlier
   Norton Commander-style TUI requirement, which is dropped, not deferred.)
4. Deployment target: an always-on **Fedora server on the home LAN, x86_64**, running the
   app as a container under **rootless Podman with a systemd Quadlet unit**, on a schedule.
   Downloaded photos live on **local disk** on that box. Everything measured below was measured
   on that machine; since the release it is one deployment rather than the only one, and Docker
   Compose is supported beside the Quadlet (§13).
5. The Google account authenticates with **password + Android phone-prompt approval**.
   Standard login therefore works in a containerised Chrome (no platform authenticator
   needed) — this closes the earlier open question about passkey-only accounts.
6. Web UI protection: a **single configured password plus an HTTP-only session cookie**,
   reachable on the LAN only. Auth is the app's own job — not "none", not delegated to a
   reverse proxy.

## 3. Architecture overview

One Go binary, several subcommands, one container, one long-lived process:

```
gpb daemon   container entrypoint: web UI server + scheduler + keepalive
gpb sync     one headless sync run (the scheduler invokes the same code in-process)
gpb albums   list albums, refreshing from Google unless --local
gpb follow   set an album's sync mode (all | picked); gpb unfollow clears it
gpb links    rebuild the album symlink view (§8) from the store, without touching Google
gpb status   session health, last runs, pending counts; --healthcheck for the container
gpb verify   re-hash downloaded files against the store; --repair queues the bad ones again
gpb passwd   set/replace the web UI password (writes an argon2id hash into config)
gpb version  the release this binary was built from
```

*All built.* `version` was not in the original list and became worth a
subcommand the moment other people could build this: the number is a constant in the source, not
a linker flag, because the image is built by hand on the box it runs on and a flag left off one
midnight build is a binary that cannot say what it is. It also heads every start in the journal.

`albums`/`follow`/`unfollow` are the CLI half of album selection; the web picker (§10) is
the other half over the same `albums.sync_mode` column. Album ids are ~50 characters, so
every command that takes one accepts any unambiguous prefix and refuses an ambiguous one.
`sync --limit N` caps items per run, which is what makes a first run against a large
library safe to try.

```
                  ┌──────────────── container (rootless podman, Quadlet) ────────────────┐
                  │                                                                      │
 browser on LAN ─▶│  web UI :8080 ── login, albums, item grid, review queue, runs,       │
 (desktop/phone)  │      │           settings, re-auth page (noVNC, on demand, proxied)  │
                  │      │ in-process                                                    │
                  │      ▼                                                               │
                  │  scheduler/keepalive ─▶ sync: warmup (headless Chrome) ──┐         │
                  │      │                                                     ▼         │
                  │      ▼                                          /data/profile        │
                  │  /data/state.db ◀── store                     (Chrome user dir)    │
                  │      ▲                                                               │
                  │      │        gphotos HTTP client ─▶ list albums/items,              │
                  │      │                  │            thumbnails, download originals  │
                  │      └──────────────────┤                                            │
                  │                         ▼                                            │
                  │  /photos/… (originals, atomic writes)   /data/cache/thumbs           │
                  │                                         (disposable, LRU-capped)     │
                  └──────────────────────────────────────────────────────────────────────┘
```

Component boundaries:

- **`auth`** — owns the Chrome profile. Launches the browser (headful for the re-auth
  flow, headless for warmup), asserts logged-in state, exports cookies + page tokens to a
  `Session` value consumed by the HTTP client. The *only* package that touches chromedp.
- **`gphotos`** — the volatile protocol package. Speaks batchexecute over `net/http` using
  a `Session`: list albums, page through album/library contents, resolve original-download
  URLs, fetch bytes, fetch grid thumbnails. All knowledge of Google's wire format is
  confined here so drift fixes touch one package.
- **`store`** — SQLite state: albums, media items, selections, sync runs.
- **`syncer`** — pure orchestration: diff upstream listings against the store, plan work,
  run the download pool, verify, record. Depends on `gphotos` and `store` through small
  interfaces so it is testable with fixtures.
- **`thumbs`** — thumbnail disk cache and fetch orchestration for the web grid (§11).
- **`web`** — the curation and admin front-end over `store` (plus `gphotos` for on-demand
  refresh, `thumbs` for images, `auth` for the re-auth flow). Replaces the former `tui`.
- **`daemon`** — scheduling loop, notification hook execution, health state; starts the
  web server.

The earlier design ran the curation UI and the unattended syncer as *separate processes*
meeting in SQLite. That split is gone: one daemon serves the web UI and runs the
scheduler, so selection edits are ordinary in-process state changes and most of the
cross-process coordination machinery disappears (§4).

## 4. The sync set and the review queue

Curation's output is a persisted **sync set**; the scheduler's input is that sync set.
This survives the TUI's removal unchanged, because it was never about the UI — it is what
makes unattended sync meaningful.

### What the sync set is

Selection is **rule-based per album, with optional per-item pick lists** — not a frozen
snapshot of item ids. Concretely, in SQLite:

- Each album has `sync_mode`: `none` (default), `all`, or `picked`.
  - `all`: the album is followed. Items added to it later are downloaded automatically on
    the next run. This is the normal case.
  - `picked`: only items the user explicitly marked inside the album are synced.
- A virtual entry **`[Library]`** appears at the top of the album list and can itself be
  set to `all` (whole-library backup via date-paged timeline enumeration) — same machinery,
  special enumerator. *Built,* with two deviations. It is a real `albums` row under the id
  `library` rather than the empty string first proposed, because `''` already means "no album
  covers this item" in the download path and the two must not be confused. And it carries a
  **date to start from**, which bounds the listing as much as the download: the timeline
  arrives newest first, so a walk that has fallen behind the date can stop rather than page
  through a hundred thousand items to learn nothing. `picked` is not offered for it — 122,331
  items is not a grid anyone curates — but nothing in the store forbids it.
- Per-item marks live in `media_selection(media_key, selected)` and only matter for
  `picked` albums.

### What happens when contents change after curation

| Upstream event | `all` album | `picked` album |
|---|---|---|
| New item appears | Downloaded next run | Recorded, marked `needs_review`, **not** downloaded; surfaced as a badge in the web UI, on the review queue page, and in the sync summary notification |
| Item removed from this album | Local file kept forever; the membership goes, so it leaves this album's symlink view and, unless another followed album holds it, the sync set | Same |
| Item removed from every album | Local file kept forever; row marked `missing_upstream` with timestamp; surfaced in the web UI and `status` (see §9, Reconcile) | Same |
| Album renamed | Album id is the key; title updated, on-disk pool layout unaffected (see §8) | Same |

Backups never delete: upstream deletion is information, not an instruction.

### Coordination between curation and a running sync

Now that both live in one process, this shrinks to two rules and one residual guard:

- The web UI edits only selection fields; the syncer snapshots the whole selection in a
  single read transaction at run start. Mid-run edits simply apply from the next run.
- One sync run at a time, enforced by an in-process guard (a mutex-held "run" slot; the
  UI's "Sync now" button queues or reports "already running").
- **WAL stays, on its own merits, not by inheritance:** a sync run performs long batches
  of upserts inside write transactions, and web handlers must keep reading (album lists,
  counts, run progress) without stalling behind them. WAL gives readers-don't-block-writer
  and vice versa for the cost of two extra files; in rollback-journal mode every grid page
  load could block mid-sync. `busy_timeout` stays set as a backstop.
- **`flock` is demoted, not deleted:** the in-process guard covers the daemon, but
  `podman exec gpb gpb sync` is still a legitimate second *process* in the container. The
  exclusive `flock` on `/data/sync.lock` exists solely to make that race safe. Built as
  `syncer.AcquireRunLock`; `gpb sync` takes it before the warmup, so the second process
  refuses in milliseconds rather than after starting a browser.
- A **second** `flock`, on `/data/profile.lock`, guards the Chrome user data directory
  itself. It is a distinct resource from the run lock: `gpb albums` needs a warmup but not
  a run slot, and two Chrome instances on one profile corrupt it. `auth.Manager` takes it
  around every warmup, so the daemon and the CLI exclude each other for the seconds a
  warmup lasts and no longer.

The web UI also *reads* live state — per-album synced/pending/missing counts, session
health, last run outcome — so curation and monitoring are the same surface.

## 5. The Google Photos protocol module (`gphotos`)

What is known versus what needs a spike, stated honestly:

**Feasible, evidenced by Google-Photos-Toolkit:** album enumeration, album contents,
library timeline paging, and item metadata are available as batchexecute POSTs to
`photos.google.com/_/PhotosUi/data/batchexecute` given the session cookies plus a CSRF
token (`at` parameter) that the page embeds in `WIZ_global_data` (field `SNlM0e`). The auth
warmup therefore harvests **cookies and page globals**, not cookies alone. Responses are
the `)]}'`-prefixed nested-array format; the module maps known RPC ids to typed decoders.
Exact RPC ids and array positions are implementation-time details captured as fixtures, not
hardcoded here — they are precisely the part that drifts.

**Probable but unverified — the Phase 0 gate:** that the *original* file (the exact bytes
the web UI's Download button yields) can be fetched with a plain authenticated HTTP GET.
The old API's `=d` baseUrls did not qualify (GPS stripped, video transcoded); the web
download endpoint does, and the spike must capture, via DevTools during a manual Shift+D,
the request shape a plain client must replicate — URL scheme, which cookies, and whether
download URLs are short-lived signed googleusercontent links.

**Thumbnails — a smaller spike sub-question.** The web grid (§10) needs item thumbnails,
which the listings expose as googleusercontent base URLs with size suffixes. Unknown:
whether fetching them requires the session cookies, and whether the URLs are short-lived.
Either way the answer only shapes the `thumbs` fetch path (§11) — the app always fetches
them itself rather than handing Google URLs to the user's browser.

**Answered 2026-08-10:** the cookies are required (403 without them) and the URL carries no
signature or expiry, so it is safe to persist. Full measurements in §11.

**Fallback if the spike fails:** keep HTTP for all listing (the high-volume chatty part)
and drive the actual byte transfer through the headless browser the way `spraot/gphotos-cdp`
does (`browser.SetDownloadBehavior` + triggering the Download action + download events).
Slower, but confined behind the same `Downloader` interface:

```go
type Lister interface {
	Albums(ctx context.Context) ([]Album, error)
	AlbumItems(ctx context.Context, albumID string, pageToken string) (ItemPage, error)
	Timeline(ctx context.Context, pageToken string) (ItemPage, error)
}

type Downloader interface {
	DownloadOriginal(ctx context.Context, item MediaItem, w io.Writer) (DownloadInfo, error)
}
```

(`thumbs` declares its own one-method fetch interface where it consumes it; same
convention.)

### What the listings actually return (measured 2026-08-10 against the real account)

The album listing is not the library, and not everything it returns is an album. Both facts
were found by measurement, after the UI had already shipped a wrong answer built on the
assumption that one listing was the whole story.

**`F2A0H` — the album listing.** Returns 181 entries for this account: 132 albums and 49
*bundles*, sets of loose photos someone shared. Slot 0 tells them apart exactly — `1` for an
album, `4` for a bundle. A bundle has **no name in any response Google serves**: not the
listing, not its contents, not its own page, which Google itself heads "Shared photos". They
are not albums and are not shown as such — treating them as untitled albums told the user
they had forgotten to name a quarter of their library.

**`Z5xsfc` — the "shared with me" listing**, behind the albums page's filter chip. It is a
superset of the album listing where the two overlap: all 132 album ids, titles, item counts
and creation dates matched exactly. It also returns **18 albums holding 3,714 photos that
`F2A0H` never mentions** — albums other people shared, including the one the user reported
as wrongly "untitled". It is callable from plain `net/http` like the rest; its first page
needs a **null** cursor (an empty string is refused), and the continuation token is a bare
string at index 1. Its entries put the metadata record in a single-key object at the *end*
of the entry, and entry length varies by album kind (12 slots or 9), so it is read from the
end rather than by fixed index.

Neither listing says whether one of its entries is the reader's own or someone else's — the
same entry shape covers both. Which listing first claimed an id is what separates them, so
the sync reads `F2A0H` first and lets it claim; anything new in `Z5xsfc` is shared with the
account. Note the consequence: `own` means "the album listing knew it", not "the user owns
it" — by owner gaia id only 51 of those 132 are actually theirs, the rest being collaborative
albums they are a member of.

The two listings use **disjoint id namespaces** (70-character ids in `F2A0H`, 44-character
in `Z5xsfc`) and issue **different media keys per share identity** for identical content. One
album in this library exists as both a bundle and a named shared album; following both would
download its contents twice. Content-level de-duplication is not built — one overlap in 181.

**People.** Slot 10 of an `F2A0H` entry is the list of people an album or bundle belongs to,
first the one it came from (id at `[1]`, display name at `[11][0]`); it decoded on all 181
entries. `Z5xsfc` entries carry no person at all, so albums shared with the account cannot
name who shared them. Telling the account holder apart from everyone else needs the
signed-in gaia id, which the page shell carries in `WIZ_global_data` field `S06Grb` — the
warmup harvests it alongside the CSRF token. Its absence is never fatal: it decides a
caption, not access.

**`lcxiM` — the library timeline**, the rpc behind photos.google.com's own main scroll, found
by recording that page's batchexecute traffic through DevTools. This is the only listing that
reaches a photo belonging to no album, and for this account that is most of them: it reports
**122,331 items**, against roughly ten thousand across every album. Pages are **300 entries**,
newest capture first, and the cursor is the first request argument (`[cursor, null, null,
null, 1, 1, null]` — the two flags are always 1 and the time bounds the page leaves empty mean
"from the newest"). Three consecutive live pages walked strictly backwards in time, so the
cursor continues rather than restarting.

Its entries are **the same media record `snAcKc` returns** — media key, thumbnail, capture and
upload times, timezone, dedup key, video metadata, all in the same slots — so one decoder
serves both. Only the page wrapper differs: entries at slot 0 and cursor at slot 1, with no
album record, because the library is not a thing with a title.

Downloading one of these needed a correction: `VrseUb` takes an album id for its permission
check, and a photo in no album has none to give. A **null** album id mints a URL; an empty
string is refused. The two were previously conflated, so the client now sends null for "no
album" — verified end to end on a real library photo, which arrived as its original 1.8 MB JPEG.

**`rJ0tlb` — the timeline index** behind the scrubber rail: a total item count and date ranges
with per-range counts (`[fromMs, toMs, count, 1]`). Not used, but it is where a "how much is
there" figure would come from without walking the library.

**Drift detection:** every decoder validates shape strictly (expected array arity, id
formats, non-empty required fields — but note the Phase 0 spike found album *titles* are
legitimately null for untitled albums, so "required" must be established against real
captures rather than assumed) and on mismatch returns a distinct `ErrProtocolDrift`, rather
than limping on with partial data. The error names the position it walked to *and* prints a
skeleton of the whole payload — arity and types, never values, so it can travel in a log
line without carrying the user's library with it. The skeleton is there because the position
alone underdescribes the failure: "found nothing at `snAcKc[1]`" reads identically whether
Google sent `null`, `[]`, `[null]`, or an array whose second slot has moved, and those are
four different bugs with four different fixes.

**What a run does about drift depends on how much of it there is.** A single album whose
contents will not decode is stepped over: the run records which album it was, walks the rest,
and finishes `partial` with the skipped albums named in its detail line. Stepping over is
safe only because a listing that failed returns before reconciling, so a skipped album keeps
every link it has and merely goes another day unrefreshed — the early return in `listAlbum`
is load-bearing for the skip as well as for itself. If *no* followed album decoded, the
decoders are wrong rather than the album, and the run fails as `drift`: a run that read
nothing, reported as a success, would look exactly like an account with no photos in it,
which is the one reading strict decoders exist to prevent. The rule is that plain — whether
anything decoded — with no threshold to tune and no count to get wrong.

Only drift is stepped over among failures. A rejected session means the account is signed out
and the next four hundred albums would fail identically; a cancelled context means the daemon
is stopping. Neither improves for being asked again four hundred times. The library timeline
is not skipped either: it is one listing rather than one of four hundred, so there is nothing
left to salvage behind it.

**A followed album Google has stopped listing is not walked at all.** Following outlives
listing — nothing clears `sync_mode` when an album is deleted or a share is withdrawn — so
its id would otherwise be asked for on every run for ever, and the answer would not be a
listing of anything. The refresh at the top of each run is what knows: every album Google
still names carries that refresh's instant in `last_seen_at`, and one that does not has gone.
The run reports it, says to unfollow it, and costs one request less rather than one more.

Believing that requires the listing to be worth believing, and the count of albums Google
named is what says so. A followed album missing from a listing of two hundred has gone. A
followed album missing from a listing of *none* says only that the listing came back empty,
and acted on, it would leave the user with a backup that had quietly stopped walking
anything — so a listing that named no albums at all fails the run instead. The count that
went missing cannot make this distinction: it is "all of them" either way.

The **canary** — fetching page one of a designated small album before each sync, so drift is
caught by one request instead of mid-run — is **not built**. Stepping over a drifted album
took the urgency out of it, since a run no longer dies at the first bad album; but a run
against genuinely drifted decoders still walks every followed album before concluding as
much, and that request budget is what a canary would save.

Writing the offending payload to a file for later study is **not built** either. The skeleton
in the error is what a maintainer actually reads, and it reaches them through the log and the
run page without a new directory to secure: a whole payload is media keys, signed URLs and
album titles, which is to say the library itself, and it would want the same care as the
Phase 0 captures for the sake of a debugging aid nobody has yet needed.

The HTTP client sends the same User-Agent string as the profile's Chrome — read from the
running browser at each warmup, not baked into the binary, so Chrome package upgrades
(§13) never cause the fingerprint to diverge (Google associates sessions with client
fingerprints; diverging invites re-auth challenges). It shares a `cookiejar` seeded from
the browser export.

## 6. Hard problem: auth bootstrap and session lifetime in a container

### Bootstrap options considered

1. **Log in on a desktop, copy the Chrome profile to the server. Rejected.** Chrome
   encrypts the cookie store with an OS-keyring-derived key: a macOS profile is bound to
   the Keychain and is unreadable elsewhere; Linux desktop profiles are bound to
   gnome-keyring/kwallet unless Chrome ran with `--password-store=basic`. Cross-machine
   profile copying only works between identically-configured Linux environments — too many
   sharp edges to be the recommended path.
2. **X11 forwarding from the server (`ssh -X`). Rejected.** Requires an X server on the
   client, slow, brittle, terrible on anything but Linux desktops.
3. **Expose the container's CDP port and interact via `chrome://inspect` screencast.
   Rejected.** Works, but the CDP port is unauthenticated total browser control, and the
   screencast UX for a 2FA login is poor.
4. **Recommended: headful Chrome *inside* the container, presented as a page of the
   admin web UI.** The stack is Xvfb + x11vnc + websockify with the noVNC client assets
   served by the app itself — but unlike the earlier design there is no separate published
   port and no separate service the user has to know about. The `/reauth` page of the web
   UI (§10) starts the stack on demand, embeds the noVNC canvas, and proxies the VNC
   websocket through the app's own authenticated session; websockify binds to
   `127.0.0.1` inside the container and is never reachable directly. The user completes
   Google login (password, then the approval prompt on their Android phone) in that
   canvas, and the profile lands directly on the persistent volume where the daemon needs
   it — no copying, no keyring coupling. Chrome runs headful with
   `--disable-blink-features=AutomationControlled` and `--password-store=basic`.
   Because this page exposes a fully authenticated Google browser, its lifecycle is
   deliberately restrictive — see §15, "The re-auth page".

   Portability corollary: because the profile is created *inside the container image's
   Linux with `--password-store=basic`*, running the identical image on a desktop
   (`podman run -p 8080:8080 -v …:/data gpb`) and completing the login there, then
   transferring the `/data` volume to the server, also works — a useful fallback if Google
   refuses the login when it originates from the server's IP.

   The old caveat about passkey-only accounts is resolved: the account uses password +
   phone-prompt approval, which needs nothing from the machine running the browser.

### Ongoing session lifecycle

- **How long it lives.** Community experience with gphotos-cdp-style persistent profiles:
  sessions survive for months *provided the browser exercises them regularly*. Google
  rotates session cookies (the `__Secure-*PSIDTS` family, roughly daily); the rotation
  happens as a side effect of the browser loading a Google page with the profile. A profile
  left cold for weeks risks invalidation. Treat the exact rotation behaviour as observed
  folklore to be confirmed in the spike, and design for it:
- **Keepalive is decoupled from sync.** The daemon runs an auth **warmup** at least every
  `keepalive_interval` (default 12h) even when no sync is due: launch Chrome on the profile —
  headful on a virtual display of its own where there is no screen, for the reason recorded
  below; `--headless=new` on a workstation that has one — navigate to
  `photos.google.com`, and assert logged-in state (final URL still on photos.google.com,
  not redirected to `accounts.google.com`; the spraot fork additionally watches for
  `signin/rejected`). The browser executes Google's own refresh JavaScript and persists any
  rotated cookies into the profile. Warmup then exports the session:

```go
func exportSession(ctx context.Context) (Session, error) {
	var wiz map[string]any
	var cookies []*network.Cookie
	err := chromedp.Run(ctx,
		chromedp.Navigate("https://photos.google.com/"),
		chromedp.WaitReady("body"),
		chromedp.Evaluate("window.WIZ_global_data", &wiz),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			cookies, err = storage.GetCookies().Do(ctx)
			return err
		}),
	)
	return newSession(cookies, wiz), err
}
```

  (`storage.GetCookies` from `github.com/chromedp/cdproto/storage` is the current API; the
  `network` variant is deprecated.) The exported `Session` lives in memory for the run;
  the durable credential is always the profile directory itself.
- **Sync-time flow.** Every sync run begins with a warmup, so the HTTP client always
  starts with fresh cookies and a fresh `at` token.
- **Staleness detection in the HTTP client.** Mid-run, credentials are considered stale
  when a request draws a redirect to `accounts.google.com`, a 401/403, or an HTML login
  page where batchexecute framing was expected. Reaction: pause workers, run one fresh
  warmup, rebuild the session, retry the failed request once. If the warmup itself cannot
  reach a logged-in state, the run stops.
- **When a human is genuinely needed.** The daemon enters `AUTH_REQUIRED`: syncs are
  suspended (keepalive attempts continue at a gentle rate in case the state heals) and the
  notification hook fires with event `auth_required` and a message containing the direct
  link to the re-auth page (`{external_url}/reauth`). The intended flow is one device:
  the user taps the link on their Android phone (on the home Wi-Fi), logs into the web UI,
  starts the browser login on the `/reauth` page, and the Google approval prompt lands on
  the same phone. `gpb status` says so in one line, and the container healthcheck goes
  unhealthy so it is visible in `systemctl --user status` and podman. The next warmup
  notices the restored session and resumes automatically. Re-auth is expected a few times
  a year, not weekly — if it becomes weekly, that is a signal Google is flagging the
  automation and the polite-rate settings need revisiting.

### What actually keeps a session alive (measured 2026-08-10 on the Fedora box)

The bullets above were written from folklore. Six consecutive interactive sign-ins on the
deployed server were each signed out of every Google property within two minutes, while the
identical code on a Mac held a session for days. What the measurements found:

- **The browser must be Google Chrome, not Chromium.** This was the whole cause. Chromium is
  built without Google's API keys and tells every site it is "Chromium"; Google's account
  machinery answered its request to `accounts.google.com/RotateCookiesPage` — the endpoint that
  keeps a signed-in session current — with `401`, every time, and deleted the `.google.com`
  session cookies. Installing `google-chrome-stable` in the image, at the *same version number*
  as the Chromium it replaced, turned that `401` into a `200` and the sessions have held since.
  The `Dockerfile` says so at the `ADD` line; do not "simplify" it back to `apt install chromium`.
- **The warmup must not read the page out of the disk cache.** The login browser leaves a fresh
  `photos.google.com` shell behind, batchexecute token and all. A warmup moments later served
  from that cache reports a live session Google was never asked to confirm, and the next one,
  past the cache, finds the account signed out. `network.SetCacheDisabled(true)` is not an
  optimisation; without it the health check is measuring itself.
- **The warmup browser must be allowed to do background work.** `chromedp.DefaultExecAllocatorOptions`
  is Puppeteer's list, meant for scraping a page and leaving, and its first entry is
  `--disable-background-networking` — which switches off the account-consistency machinery this
  browser exists to run. `browserOptions` therefore builds its flags from nothing.
- **The browser must be asked to shut down, not killed.** Chrome batches cookie writes in memory
  and commits them on a timer far longer than a warmup lives, so a `SIGKILL`ed browser persists
  nothing it learned. `closeBrowserCleanly` sends the CDP `Browser.close` command and waits.
- **Diagnosis needs the redirect trail, not the final URL.** A `302` away from
  `photos.google.com` and a `200` on it that then hands off in script land end in the same place
  and mean opposite things. `documentTrail` logs every top-level document with its status and
  whether it came from cache; it is what found the `401`, after the final URL had been read as
  evidence for three wrong theories in a row.

Two things were built on theories this displaced and have since been removed: a settle delay
before tearing down the login browser, and a wait for the freshness cookie to rotate and reach
disk before closing the warmup. The headful warmup below is the last survivor of that group and
is still unmeasured.

## 7. State store and data model

**Decision: SQLite via `modernc.org/sqlite`** (pure-Go, CGO-free port; driver name
`"sqlite"`; DSN pragmas like `?_pragma=busy_timeout(10000)&_journal=WAL`).

- Why SQLite at all, versus JSON files or bbolt: the store must answer set-difference
  queries over ~10⁴–10⁵ rows (what is selected but not downloaded, what vanished upstream,
  what failed fewer than N times), act as a crash-safe resumable work queue, and be
  inspectable ad hoc (`sqlite3 state.db` when something looks wrong). That is exactly a
  relational workload; hand-rolling it over flat files is more code and less safety.
- Why modernc over `mattn/go-sqlite3`: no CGO means a trivially static binary and a
  clean cross-compile; the performance delta is irrelevant at this scale.
- Migrations: embedded ordered `.sql` files applied by a 30-line migrator keyed on
  `PRAGMA user_version`. No migration framework.
- What deliberately does **not** live here: web UI sessions (in-memory only — a daemon
  restart logs the single user out, which is acceptable; see §15) and the thumbnail cache
  (plain files with no index table, §11).

```sql
CREATE TABLE albums (
    id            TEXT PRIMARY KEY,          -- Google album key; 'library' row = [Library]
    title         TEXT NOT NULL,
    item_count    INTEGER,
    sync_mode     TEXT NOT NULL DEFAULT 'none',  -- none | all | picked
    created_at    TEXT,                          -- Google's date for the album
    kind          TEXT NOT NULL DEFAULT 'own',   -- own | shared | bundle | library (§5)
    cover_url     TEXT NOT NULL DEFAULT '',
    owner_name    TEXT NOT NULL DEFAULT '',      -- who it came from; '' when unknown
    owner_is_account INTEGER NOT NULL DEFAULT 0, -- resolved at sync time against WIZ S06Grb
    since_date    TEXT,                          -- library only: oldest capture worth walking
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    last_synced_at TEXT
);

CREATE TABLE media_items (
    media_key     TEXT PRIMARY KEY,          -- Google media key (the /photo/{id} id)
    filename      TEXT NOT NULL,
    captured_at   TEXT,
    size_bytes    INTEGER,
    mime_type     TEXT,
    sha256        TEXT,
    state         TEXT NOT NULL DEFAULT 'discovered',
                  -- discovered | queued | downloading | done | failed | missing_upstream
    fail_count    INTEGER NOT NULL DEFAULT 0,
    local_path    TEXT,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    downloaded_at TEXT,
    missing_since TEXT,
    needs_review  INTEGER NOT NULL DEFAULT 0   -- feeds the web review queue (§10)
);

CREATE TABLE album_items (
    album_id   TEXT NOT NULL REFERENCES albums(id),
    media_key  TEXT NOT NULL REFERENCES media_items(media_key),
    last_seen_at TEXT NOT NULL,
    PRIMARY KEY (album_id, media_key)
);

CREATE TABLE media_selection (
    media_key TEXT PRIMARY KEY REFERENCES media_items(media_key),
    selected  INTEGER NOT NULL               -- only consulted for 'picked' albums
);

CREATE TABLE sync_runs (
    id          INTEGER PRIMARY KEY,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    outcome     TEXT,                        -- ok | partial | auth_required | drift
                                             -- | interrupted | error
    listed      INTEGER, downloaded INTEGER, failed INTEGER, bytes INTEGER,
    error       TEXT                         -- the failure, the albums skipped, or both
);
```

Items belong to many albums; `album_items` is the join. An item selected through any
followed album is downloaded once and recorded once.

## 8. On-disk layout and metadata preservation

```
/photos/                          (the backup volume — big, user-facing)
  pool/2024/2024-06/AF1QipN…_IMG_2041.HEIC
  .tmp/                           (same filesystem, for atomic rename)
/data/                            (state volume — small, private, mode 0700)
  state.db  state.db-wal
  state.backup.db                 (weekly VACUUM INTO copy, §14)
  chrome/                         (Google Chrome, fetched on the first start, §13)
  profile/                        (Chrome user data dir)
  cache/thumbs/                   (grid thumbnails; disposable, LRU-capped, §11)
  config.toml
  sync.lock
```

Host side these are bind mounts from local disk on the Fedora box — proposed
`/srv/gpb/photos` and `/srv/gpb/data`, mounted with SELinux `:Z` labelling (§13).

- **Pool, keyed by capture date, filename prefixed with a truncated media key** to make
  names collision-free and stable across album renames. Items without a capture date fall
  back to upload date, else `pool/unknown/`.
- **Originals are byte-for-byte untouched** — that is the entire point of the web download
  path. EXIF (including GPS) lives inside the files; nothing is rewritten.
- Google-side metadata that is *not* inside the file (description, favorite flag, album
  membership, upstream deletion time) lives in SQLite only. Per-file JSON sidecar export is
  deliberately deferred (open question) — sidecars are what made Takeout messy.
- **Album-structured browsing** lives at `/photos/albums/<title>/`, one entry per followed
  album, each holding one **symlink** per downloaded item named as the camera named it. The
  pool plus the DB remains the backup; this tree is a convenience and holds nothing that is
  not derivable from the store, which is what makes rebuilding it wholesale safe.
  - **Symlinks, not the hardlinks originally sketched.** Hardlinks would be
    indistinguishable from real files and immune to dangling, but they are same-filesystem
    only and — the deciding point — a hardlink is a second *reference to the data*. Pruning
    a link tree made of hardlinks means calling `unlink` on something that is, as far as
    the filesystem is concerned, the photo. A symlink tree can only ever lose pointers.
  - **Targets are relative** (`../../pool/2024/2024-03/…`). The daemon writes them as
    `/photos/...` inside the container while the host browses the same tree at
    `/srv/gpb/photos/...`; absolute targets would dangle on one side or the other. Relative
    targets also survive the whole photos directory being moved.
  - **Names are the original filename**, disambiguated as `IMG_2041_<9 key chars>.HEIC`
    only when two items collide inside one album. The media-key prefix stays a pool concern.
  - **Pruning only ever removes symlinks and the directories holding nothing else.** A
    directory containing a real file is left entirely alone: this tree is disposable, but a
    file someone put there by hand is not, and a convenience feature must not be able to
    delete data.
  - Rebuilt after every run and whenever a sync mode changes, so an unfollowed album's
    folder disappears when the user says so rather than at the next sync. A rebuild failure
    is logged, never fatal — the backup succeeded.
- **Write protocol:** download to `/photos/.tmp/<media_key>.part`, hash while streaming,
  verify length against the listing metadata (and Content-Length), `fsync`, then `rename(2)`
  into the pool and update the DB row. The rename is the commit; a crash leaves only a
  `.part` file, which startup reconciliation deletes or resumes.
- Both volumes are local filesystems on the server, which the concrete deployment already
  satisfies. One standing constraint if things ever move: SQLite WAL, `flock`, and the
  atomic-rename write protocol all rely on local-filesystem semantics — do not relocate
  `/srv/gpb` onto NFS/SMB later.

## 9. Sync engine

A run is: **warmup → canary → list → plan → download → report.**

- **List:** enumerate albums (upsert all, so the web UI is always current), then page
  through each followed album and, if `[Library]` is followed, the timeline. Update
  `last_seen_at` on everything touched.
- **Plan:** pure function from store state to a work list — selected ∧ not `done` →
  download; selected ∧ `done` but file missing on disk → re-download; new in `picked`
  albums → `needs_review`.
- **Reconcile:** a full album listing that omits an item proves the item has left *that
  album*, and the membership row goes. It proves nothing about Google: `missing_upstream`
  is set only when no other album still lists the item. The library counts as one of those
  albums. Its walk is reconciled too — otherwise a deletion would be undetectable for the
  majority of a library that is in no album — but only over the stretch of timeline the
  walk can vouch for: at or above the date bound, plus 48h of clearance when the walk
  stopped there rather than reaching the end, because capture times carry the camera's own
  timezone and wobble out of order around the boundary. Below that, and for items whose
  capture date Google never reported, the walk skipped rather than missed them, and the
  conservative failure (keeping a photo Google deleted) costs disk while the aggressive one
  (writing off a photo that is still there) costs the backup. A walk that listed nothing
  reconciles nothing: an empty timeline is a failure, not an empty account. A partial
  listing must never reach this step at all.
- **Edits upstream:** Google Photos keeps the original alongside edits. Which variant the
  download endpoint returns for edited photos is settled in the spike; v1 policy is
  **first-downloaded bytes are kept and never silently replaced** (open question).
- **Resume after interruption:** the queue is DB rows, so a killed container resumes where
  it stopped. Large video downloads attempt HTTP `Range` resume of the `.part` file when
  the server advertised `Accept-Ranges`; otherwise restart that file.
- **Concurrency and politeness:** listing is sequential (one page in flight). Downloads
  run on a small worker pool (default 3) behind one global token-bucket limiter (default
  2 req/s) shared with listing. Failures back off exponentially with jitter (1s → 2min),
  honour `Retry-After`, and a circuit breaker aborts the run after N consecutive transport
  failures (default 8) rather than grinding against a broken or throttling server.
  Defaults are deliberately timid; a personal backup does not need to be fast.
- **Thumbnail traffic is budgeted separately.** Interactive grid browsing fetches
  thumbnails through its own, more generous limiter (§11) rather than the sync bucket —
  it is human-paced and indistinguishable from what the real web app generates while
  scrolling, whereas the sync bucket exists to keep *bulk automated* traffic boring. The
  two never compound badly in practice: thumbnails flow only while a person is actively
  clicking around the UI.

## 10. The web UI

### Technology: `html/template` + `embed.FS` + one small vanilla JS file

Requirements, honestly sized: one user, on a LAN, roughly seven screens, of which exactly
one (the item grid) needs client-side interactivity. That is a server-rendered
application:

- **Server-rendered pages** from `html/template`, templates and static assets compiled in
  via `embed.FS` — the whole product stays one static binary with no node toolchain, no
  bundler, no build pipeline, and no framework churn. Routing is the stdlib mux with
  method + wildcard patterns (`GET /album/{id}`, `r.PathValue("id")`).
- **One vanilla JS file** (~150 lines) for the grid: click-to-toggle, shift-click range
  select, batched `fetch` POSTs of pick changes, and the short-poll progress block on the
  runs page. Everything else is plain HTML forms with full-page POSTs.
- **Rejected: React/Vue/any SPA.** A build pipeline, an API layer, and client-side state
  management for a single-user LAN admin tool is precisely the over-engineering the house
  rules prohibit.
- **Considered and not taken: htmx** (verified current: v2, a dependency-free single
  script, no build step). It would be a defensible middle ground, but its value is
  avoiding hand-written fetch calls — and the grid's range-selection logic needs
  hand-written JS regardless, at which point one small file covers everything htmx would.
  Revisit only if the vanilla file sprawls past a few hundred lines.
- **Live regions over one websocket** (settled 2026-08-13, after a spell of 3-second
  polling). The daemon says what changed and each open page re-fetches only the region that
  changed; `live.js` holds the socket and a backoff loop for reconnecting. The protocol is
  strictly server-to-client, which is exactly what Server-Sent Events are for, and SSE would
  have needed no library and no backoff loop — browsers reconnect it themselves. Websockets
  were kept anyway: `gobwas/ws` is already in the module graph through chromedp, so it costs
  no dependency, and the rewrite would buy identical behaviour. Revisit if a reverse proxy
  ever goes in front, where SSE is the friendlier of the two.

```go
mux := http.NewServeMux()
mux.Handle("GET /{$}", requireSession(albumsPage))
mux.Handle("GET /album/{id}", requireSession(albumPage))
mux.Handle("POST /album/{id}/mode", requireSession(setAlbumMode))
mux.Handle("POST /album/{id}/picks", requireSession(updatePicks))
mux.Handle("GET /thumb/{key}", requireSession(thumbHandler))
mux.Handle("GET /reauth/ws", requireSession(reauthVNCProxy))

protection := http.NewCrossOriginProtection()
srv := &http.Server{Addr: cfg.Web.Listen, Handler: protection.Handler(mux)}
```

### Pages and flows

- **`/login`** — the only unauthenticated page. Password form → argon2id verify → session
  cookie → redirect to the originally requested page. Failures are rate-limited (§15).
- **`/` — Albums.** A table: album title, item count, `sync_mode` selector
  (none/all/picked, a plain POST form per row), synced/pending/failed counts, and a
  `needs_review` badge. The `[Library]` pseudo-row sits on top. A "Sync now" button asks
  the daemon for an immediate run (reports "already running" when it is). Works entirely
  without JS.
  *Built,* with three deviations. **(a)** Followed albums sort to the top of the default
  arrangement: with 181 albums and a handful followed, title order buries the only rows worth
  watching. A heading that was clicked sorts by that column alone, or there would be a column
  nobody could sort by. **(b)** A
  "Refresh album list" button was added — without it a fresh install shows an empty table
  and no way forward, since nothing populates `albums` until Google is asked. **(c)** No
  `[Library]` row *in the table*: it is a card above it instead. It is the only entry carrying a
  date, it has no cover, owner or creation date to sort by, and "back up everything" deserves to
  be read before it is clicked. The page polls itself every 5s *only* while a run is in flight,
  so a reload never lands under someone mid-selector. The session status page moved to
  `/status` to give `/` up to the albums, as the routing above intends.
  Four further additions came out of using it against the real library (§5 for the protocol
  behind them). **(d)** A **Created** column, and every column is a sortable heading: 199 rows
  are not browsable in one fixed order, and creation date is the only thing that says which of
  two similarly-named albums is the one being looked for. **(e)** Each row carries its **cover
  image**, served through the same cache as the grid (§11) under the album id, because for
  rows Google gives no name to it is the only thing that tells one from the next. **(f)** The
  49 bundles are collapsed into a **"Shared photos" section** of their own below the albums —
  listed inline they were a wall of near-identical nameless rows between the albums the user
  came for. Its open/closed state is a URL parameter rather than a `<details>` element, because
  every control inside it is a link or a form POST: sorting that table, or saving one of its
  rows, would otherwise close the section the user opened to reach it. **(g)** A row with no title is labelled by who the photos came from — "Photos you
  shared", or "Shared photos from ⟨name⟩" — since that is the only distinguishing thing Google
  serves for them. On this library it separates exactly one row from the other 48, which is
  what (e) and (f) are for.
- **`/album/{id}` — item grid.** Server-side pagination, 200 items per page, `<img
  loading="lazy">` thumbnails from `/thumb/{key}` (§11). Per-item state badges: ✓
  downloaded, ! failed, ? needs review, † missing upstream, ▶ video. In `picked` mode each
  cell gets a checkbox overlay: click toggles, shift-click selects the range between the
  last anchor and the target (keys computed from DOM order), and changes are POSTed as
  media-key batches. **Selection state lives server-side** in `media_selection`, so page
  navigation never loses marks, and **"Select all / Clear all in album" executes
  server-side over the whole album**, not just the loaded page — on a 10⁴-item album the
  client never needs every key.
  *Built,* with three deviations. **(a)** A per-album **"Refresh from Google"** button, and
  it is not optional: nothing lists an album's items until a sync run visits it, so an album
  freshly switched to `picked` would otherwise show an empty grid and no way to fill it. It
  runs `syncer.ListAlbum` — one album, no downloads. **(b)** A picks request is filtered
  through `store.MediaKeysIn` before it is applied. The browser posts keys it claims to have
  rendered, but a request is not evidence of what was rendered, and marking items in an album
  the user was not looking at would surface months later as photos nobody asked for.
  **(c)** No per-item detail view yet — the badges and caption carry filename, date, size and
  state, which is what picking actually needs. The open-in-Google-Photos link is deferred
  with it.
  The album list gained a **"nothing picked"** badge from building this: an album in `picked`
  mode with an empty selection backs up nothing while looking, on every other column,
  exactly like a followed album whose first sync has not run.
  **(d)** Opening an album nobody has listed yet starts the listing itself rather than showing
  an empty grid and a button: arriving at a page that appears to say "this album is empty" is
  a worse first impression than a moment's wait, and the page already polls while it runs.
- **`/review` — the review queue.** All `needs_review` items, grouped by album, as a
  thumbnail grid with two actions per item (or per selection): **approve** (mark selected,
  download next run) and **dismiss** (leave unselected). Both clear the flag. This is the
  workflow for items appearing in `picked` albums after curation.
- **`/runs` — sync status and history.** The `sync_runs` table rendered newest-first with
  outcome, counts, bytes, error; a live progress block (current phase, items done/total)
  while a run is active.
- **`/reauth` — Google session page.** Session state (OK / AUTH_REQUIRED), last warmup
  times, and the "Start browser login" flow that embeds the noVNC canvas. Deliberately
  locked down; full treatment in §15.
- **`/settings`** — schedule, limits, notify hook command, thumbnail cache cap. Writes
  `config.toml`; values that cannot safely apply live take effect next run and the page
  says so.
  *Built,* and it grew two things the sketch did not have. **(a)** The **Home Assistant** fields
  (§20), with the broker's connection state above them and a "Try again" that re-dials without
  changing anything — a broker refusing a credential and a broker that is simply down look
  identical until the page says which. **(b)** **How this page is reached**: the address, and
  encryption (§15). Which is also where the **Restart** button lives, at the foot of the page:
  the schedule, the limits and encryption are read once at startup, so a saved change to any of
  them needed an action a phone could take, and encryption needed one badly enough to justify
  the button on its own. It stops the daemon by signalling itself and leaves starting it again
  to whatever runs it — nothing here talks to systemd or podman, since from inside the container
  there is nothing to talk to. It appears only where `container` is set (§12), so a
  hand-started daemon is never offered a button that would strand it, and it says first what a
  restart would interrupt: a run cut off is picked up by the next one, but the reader deserves
  to know before rather than after.

Curation must not require Google to be reachable: album and item lists render from cached
store state, with an explicit per-album "Refresh from Google" action (and thumbnails
already cached keep displaying). This preserves the old TUI's offline-curation property.

## 11. Thumbnails

*Built.* The measurements below replace what Phase 0 left open.

The grid must show images the user may deliberately *not* have downloaded yet — that is
what curation is. So thumbnails come from Google, and they cannot come directly: the
user's browser has no Google session (and must never be given one), and handing out
possibly-signed, possibly-expiring googleusercontent URLs would tie UI correctness to
their lifetime. The app is always the intermediary. This is a genuinely new subsystem the
TUI never needed.

**Measured 2026-08-10, 1586 items across 10 albums.** Every item carried a thumbnail URL —
videos included, which get a poster frame. Every URL was on
`photos.fife.usercontent.google.com`, path `/<2 chars>/<62 chars>`, **no query string**.
Every one returned HTTP 403 with a tiny PNG anonymously, and `200 image/jpeg` with the
session cookies. So:

- **The cookies are mandatory**, which settles the open question: the fetch goes through the
  jar-carrying transport, and the host is pinned to a one-entry allowlist. An unfamiliar host
  is refused rather than handed the account's cookies, exactly as the download path does.
- **The URL is an opaque content id, not a signature** — no query, no expiry parameter — so
  it is safe for the store to keep. `media_items.thumbnail_url` is written by the listing and
  refreshed by every later one, which costs nothing and covers us if that stops being true.
- **The size suffix works and matters.** The bare URL returns ~90 KB at some default size;
  `=w256-h256` returns ~24 KB fitted inside a 256 box; the `-c` cropping variant returns ~33 KB
  because it fills a square. The grid crops in CSS, so the uncropped form is both smaller and
  more useful — a 3.7× saving on every one of 200 cells per page.

- **Serving:** `GET /thumb/{media_key}` behind the normal session auth. Cache hit → serve
  the file with a long `Cache-Control: private, max-age`; miss → fetch from Google via
  `gphotos` with the current session, write to cache, serve. A singleflight guard
  collapses concurrent misses for the same key (a fresh grid page is 200 near-simultaneous
  requests). Upstream failure returns a placeholder image and caches nothing.
- **One size only:** grid-sized, `=w256-h256`. No full-resolution proxying — this keeps the
  subsystem one directory of small JPEGs instead of a second download pipeline.
- **Cache:** `/data/cache/thumbs/<2-hex-shard>/<sha256 of media key>.jpg`. Thumbnails are
  immutable per media key, so entries never expire on content grounds — only for space.
  Sizing: ~15–25 KB each, so a fully-browsed 10⁵-item library tops out around 2–2.5 GB, but
  the cache fills only for albums actually browsed. Default cap 1 GiB (configurable).
  *Deviation: the filename is a hash of the media key, not the key itself.* A media key is
  remote input that becomes a path, and every sanitiser has an escape nobody thought of — a
  key of `..` with the obvious character mapping walks straight out of the cache directory. A
  hex digest cannot express a path at all, is fixed-width, and shards evenly for free.
- **Eviction: LRU by mtime, no index table.** On every cache hit the file's mtime is
  bumped (one `utimes` call — atime is untrustworthy under `relatime`); a lazy sweep on
  the write path deletes oldest-mtime files when the cap is exceeded. The cache is fully
  disposable: deleting the directory costs re-fetches, nothing else.
- **Load on Google, and the politeness interaction (§9):** thumbnails get their own token
  bucket (default 8 req/s, burst 16), separate from the 2 req/s sync bucket. Justification: this traffic only exists while a human is scrolling a grid,
  and the real Google Photos web app fires dozens of thumbnail requests per scroll — an
  authenticated session fetching thumbnails at human pace is the most normal-looking
  traffic in this entire system. A cold 500-item album fills in about a minute; cached
  albums cost Google nothing. What it deliberately does not do: pre-fetch thumbnails for
  unbrowsed albums during sync runs — that would multiply automated request volume for
  speculative benefit.

## 12. Configuration

One file, `/data/config.toml` (BurntSushi/toml), created with defaults on first run.
Everything has a default except the web password; a fresh install needs `gpb passwd` and
nothing else.

```toml
photos_dir = "/photos"

[web]
listen           = ":8080"
external_url     = "http://<host>:8090"   # used in notification links and in the certificate
password_hash    = ""       # argon2id PHC string; written by `gpb passwd`
session_idle_ttl = "720h"   # 30 days

[web.tls]
mode      = "off"   # off | self-signed | provided; §15
cert_file = ""      # paths inside the container, for mode = "provided"
key_file  = ""

[schedule]
sync_at            = "03:30"   # daily, local time; re-read before every check
keepalive_interval = "12h"

[limits]
download_workers    = 3
requests_per_second = 2.0
min_free_bytes      = 8589934592   # 8 GiB; a run stops rather than eat into it, 0 for no floor

[thumbs]
cache_max_bytes     = 1073741824   # 1 GiB
requests_per_second = 8.0

[notify]
command = ""   # e.g. "/data/hooks/notify.sh"; argv: <event> <message>

[mqtt]
broker           = ""              # e.g. "<host>" or "tcp://mosquitto:1883"; empty = off
username         = ""
password         = ""
topic_prefix     = "gpb"           # state, availability and command topics live under this
discovery_prefix = "homeassistant" # where Home Assistant listens for entity announcements
```

Env overrides exist only where the container, or a machine that is not the container, needs
them: `GPB_DATA_DIR` and `GPB_PHOTOS_DIR` for where the two mounts land, `TZ` for the clock
the schedule reads, `GPB_CHROME_PATH` for a browser that is not on `PATH` as `chromium` —
which is every macOS development box — and `GPB_NOVNC_DIR` for a noVNC that is not at the
Debian package's path. No flags-vs-file layering beyond that.

One further variable is read and is not really an override: **`container`**. Podman sets it
itself, the compose file sets it explicitly because Docker sets nothing, and it is the only
signal the process has that something else owns its lifecycle — which is what the Restart button
(§10) needs to know before it offers to stop the daemon.

Which settings are re-read as they are used and which wait for a restart is not a detail: the
settings page states it per field, because a page that silently accepts a change nothing acts on
is worse than one that refuses it. Re-read live: the backup time, the broker settings, the
password. Read at startup: the keepalive interval, the limits, the thumbnail cache, the notify
hook, and encryption — which is the one people meet, since its whole effect is that the page
moves to another address.

## 13. Deployment: the image, the Quadlet unit, and Compose

- **Image:** multi-stage. Stage 1: `golang` builds the static `gpb` (CGO disabled —
  possible because of the modernc driver). Stage 2: `debian:bookworm-slim` +
  `fonts-liberation fonts-noto-color-emoji xvfb x11vnc novnc websockify tini`, and Chrome's
  shared libraries. Chrome is Google's own `.deb`, not Debian's `chromium` package — see §6, where
  that distinction is the difference between a session that lives and one that dies in two
  minutes. Headful and `--headless=new` come from the one package either way.
  Target is `linux/amd64` only — Google ships Chrome for Linux on that architecture and no other,
  so this is now the browser's constraint rather than the server's. Runs as a non-root user, `tini`
  as PID 1, `deploy/entrypoint.sh` then `gpb daemon`.

  **Chrome is not in the image.** 0.1.0 said no image should ever be published, because one
  carrying Chrome would be redistributing software that is not free and not ours to hand on — and
  that left every reader building their own. 0.1.1 publishes
  [`sodre90/gpb`](https://hub.docker.com/r/sodre90/gpb) instead, and the container downloads
  Chrome from Google's own server into `/data/chrome` the first time it starts: what is
  redistributed is this project, and Chrome reaches the reader from Google under the terms they
  accept there, exactly as it would installing it on a laptop. It lands on the volume rather than
  in the container so a new image does not fetch it again — 110 MB down, ~430 MB unpacked, once.
  The image is smaller for it, 688 MB against 1.3 GB.

  The dependency list is the part that could rot: an image without Chrome still needs every
  library Chrome links against, and a list copied into the Dockerfile would be right until the
  release that added one. A throwaway build stage downloads the `.deb`, reads `dpkg-deb -f … Depends`
  and passes those package names on; nothing else of that stage reaches the image. Measured on the
  box 2026-08-13: the extracted Chrome starts **sandboxed** — no `--no-sandbox` fallback in the
  journal — and renders `accounts.google.com` in 2.2 s. A cold, never-signed-in profile times out
  its first warmup either way, on this image and on the Chrome-carrying one alike; that is the
  fresh-install path, not a regression.
- **Chrome sandbox note:** Chrome's own sandbox wants unprivileged user namespaces
  *inside* the container, which nested under rootless podman may not be available. If the
  browser fails to start, the accepted fallback is `--no-sandbox` — tolerable here because
  this browser only ever visits Google properties, but it should be a fallback, not the
  default. Verify on the real box in Phase 1.

  **Phase 1 result (Docker, with Debian's `chromium`):** the sandbox was unavailable in the
  built image — Chromium reported *"No usable sandbox! If this is a Debian system, please
  install the chromium-sandbox package"* — and the automatic retry with `--no-sandbox` then
  started and navigated successfully. So the fallback was the expected path, not the exception.

  **Superseded 2026-08-10, once the image moved to Google Chrome:** the fallback has not fired
  since. Chrome's `.deb` installs its own setuid `chrome_sandbox`, which needs no user
  namespace, so the browser starts sandboxed under rootless Podman on the Fedora box. Measured
  across the warmups from 22:12 onward that evening; the 17:16 and 19:24 fallbacks in the
  journal are all from the Chromium image. The retry stays in the code — it costs one failed
  start on a host that needs it — and `Manager.Status().UsingNoSandbox` still reports which path
  a given deployment took, which is now the honest way to answer this rather than assuming.
- **Quadlet unit**, at `~/.config/containers/systemd/gpb.container` for the deploying
  user:

  The unit itself is checked in at `deploy/gpb.container` and is the copy to read; it is not
  reproduced here, because a second copy in prose is one that quietly stops being true. What
  belongs here is why it says what it says, which is the rest of this section. The parts worth
  knowing without opening it: the pool is a bind mount the deployer points at whatever disk has
  room, since a library this size outgrows a system disk and filling that one takes the whole box
  down; and the UI is published on **8090** because 8080 is the port everything else on a home
  server wants too — the container still listens on 8080 inside.

  **Since the release it is a template rather than one box's unit** (`%h` for the home directory,
  the pool defaulting beside the data, `TZ` and the publish address called out as settings). The
  guide says to diff before copying it over an install that already runs, because the failure it
  would otherwise cause — a pool quietly moving back under the home directory — is silent until
  the disk fills.

  Activate with `systemctl --user daemon-reload && systemctl --user start gpb`. Quadlet
  generates the real service unit; when a typo in the `.container` file yields a baffling
  "unit not found", inspect the generator output at
  `/run/user/$UID/systemd/generator/gpb.service`.
- **`Restart=always`, not `on-failure`.** The Restart button on the settings page (§10) stops the
  daemon *cleanly* and leaves starting it again to whatever runs it, and `on-failure` takes a
  clean exit at its word. The unit was deployed with this before the button existed, so the
  button could never appear on a daemon nothing would bring back. Proved on the box the same way
  a user would meet it: `podman kill --signal TERM`, then `NRestarts=1` and the UI answering
  again about fifteen seconds later.
- **Compose, for people who do not run systemd.** `docker-compose.yml` at the repository root is
  the second supported way in, and the two guides are separate — `deploy/compose.md` and
  `deploy/quadlet.md` — because following half of each is worse than following either. It builds
  rather than pulls, for the same Chrome reason as everything else; it declares
  `restart: unless-stopped`, which is Docker's spelling of the bullet above; and it sets
  `container=docker` itself, because Docker — unlike Podman — tells the process nothing about
  being supervised, and without it the settings page would hide the Restart button from every
  Docker install. Verified end to end on the box under docker-compose v5.3.1 on a scratch port
  and data dir: built, came up, answered the login redirect, and wrote its config 0600 and its
  profile 0700 as uid 1000. Rootless Podman driving that same file needs `userns_mode: keep-id`
  or the bind mounts land owned by a subordinate uid — which is exactly what the Quadlet unit's
  `UserNS=keep-id` is for, so that guide is the better answer on a Podman box.
- **Boot without login:** rootless user units start at login and die at logout unless
  lingering is enabled — `loginctl enable-linger <user>` is mandatory for an appliance.
- **SELinux is enforcing on Fedora by default.** Without the `:Z` volume suffix the
  container gets `EACCES` on the bind mounts and the failure mode (avc denials in the
  audit log) is obscure. `:Z` applies a private per-container category label — correct
  while gpb is the mounts' only consumer. If the photo pool is later also exported by
  Samba or another container, switch to `:z` (shared label) deliberately rather than
  fighting denials ad hoc.
- **`/dev/shm`:** Chrome uses shared memory heavily and the container default of 64 MB
  causes renderer crashes; `ShmSize=1g`.
- **`UserNS=keep-id`:** without it, files created by the container's UID land on the host
  owned by a subordinate UID (e.g. 100999) — obnoxious for a photo pool the user will
  browse and back up with host tools. `keep-id` maps the container user to the deploying
  host account, so the pool stays owned by the login user.
- **Networking and firewalld:** rootless podman (pasta) publishes the port without privileges,
  and a host address in front of the mapping — `PublishPort=192.168.1.10:8090:8080` — keeps the
  listener off every other interface. The shipped unit leaves that off, because a template cannot
  know the reader's address and an unreachable UI is a worse first run than a broad one; the
  guide says how to pin it.
  Fedora's firewalld blocks unsolicited inbound by default, so the port must be opened
  explicitly — `firewall-cmd --permanent --add-port=8090/tcp && firewall-cmd --reload` in
  the active zone (check with `firewall-cmd --get-default-zone`; `FedoraServer` on Server
  edition). This is the system's single deliberate network exposure.
- **Logging:** podman's default journald driver; `journalctl --user -u gpb.service`. The
  app logs plain lines to stdout and nothing else.
- **Updates vs the persisted profile:** an image rebuild plus restart never touches
  `/data` — the Chrome profile and the DB live on the volume and survive. With
  `AutoUpdate=local` and `podman-auto-update.timer` enabled, retagging a rebuilt local
  image restarts the unit automatically; equally fine is a manual `podman build` +
  `systemctl --user restart gpb`. A Chrome version bump changes the User-Agent, which is
  harmless because the HTTP client reads the UA from the running browser at each warmup
  (§5). Health status is a *signal*, not a trigger: `HealthOnFailure` is deliberately not
  set to kill/restart, because the most likely unhealthy state is `AUTH_REQUIRED`, which a
  restart cannot fix and would just churn. The health command is `gpb status --healthcheck`,
  which asks the *running daemon* over HTTP rather than opening the profile itself — two Chromes
  on one user data directory clash. That made it the one thing encryption broke: dialling `http`
  at a daemon now answering `https` returned 400 and reported a perfectly well daemon as dead,
  minutes after the setting was turned on. It now follows `web.tls` and pins to the certificate
  the config names, which is a *stronger* check than a trust store — the certificate is in none —
  reading the expected name off the certificate itself, since it dials 127.0.0.1 while a provided
  certificate is issued for the name people browse to.
- **Scheduling stays in-process, in the daemon.** A ticker asks, on each tick, whether the
  last window at `sync_at` has gone by unserved; keepalives interleave on their own
  interval. There is no jitter — a backup that starts at exactly 03:30 every night is one
  fewer variable when reading a run log, and one account is not a thundering herd. The time
  is re-read from `config.toml` before every check, so an edit on the settings page applies
  without a restart. Rationale over a
  systemd timer: the scheduler shares state with keepalive, `AUTH_REQUIRED` suppression,
  and the run guard — that is application logic, and ~40 lines of stdlib beats
  coordinating an external timer with a long-lived container. `gpb sync` remains available
  for anyone preferring external scheduling. A missed window (container down) is simply
  run at next startup if the last success is older than one interval.
- Manual operations go through `podman exec -it systemd-gpb gpb passwd|status|sync|version`
  (`docker compose exec gpb gpb …` on the other path).

## 14. Failure modes

| Failure | Detection | Response | Human-visible via |
|---|---|---|---|
| Cookies stale mid-run | Redirect to accounts.google.com / 401 / login HTML | One warmup + single retry, else abort run | `sync_runs.outcome`, notify on abort |
| Session dead (re-auth needed) | Warmup cannot reach logged-in photos.google.com | `AUTH_REQUIRED`: suspend syncs, keep gentle keepalive probing | notify `auth_required` with `/reauth` link, unhealthy healthcheck, `status`, banner on every web page |
| Warmup could not be made | Browser will not start, no network, timeout — anything that is not Google saying no | `WARMUP_FAILED`: keep the session and the last verdict, retry on the next keepalive; never ask for a sign-in the user cannot usefully give | notify `warmup_failed`, warn banner on every web page, `session_problem` in Home Assistant |
| Login rejected as automated | `signin/rejected` URL during login | Surface clearly on the `/reauth` page; retry via desktop-run-container fallback (§6) | `/reauth` page |
| Protocol drift | Strict decoders → `ErrProtocolDrift`, naming the position and a scrubbed skeleton of the payload | One album: skip it, finish `partial`, name it on the run. No album decoded: abort the run as `drift` | `sync_runs.outcome`, the run's detail line on `/runs`, and after a `drift` a warn banner on every page |
| Followed album gone from Google | Its `last_seen_at` predates the refresh at the top of the run | Do not walk it, finish `partial`, name it and say to unfollow it. Album listing named nothing at all: abort the run instead | `sync_runs.outcome`, the run's detail line on `/runs` |
| Partial/corrupt download | Length mismatch, hash recorded at write; `verify` re-hashes later | Delete `.part`, retry with backoff, `failed` after N attempts | run summary, `!` badge in grid |
| Disk full | Preflight free-space check (require estimated run size + margin); `ENOSPC` mid-write | Abort before starting, or abort run cleanly; `.part` removed | notify `disk_full` |
| Chrome won't start / zombie | chromedp context error / warmup timeout | Kill process tree, retry once, else treat as failed warmup | notify on repeat |
| Killed mid-run | Stale `.part` files, `downloading` rows, stale lock | Startup reconciliation requeues; `flock` dies with the process | `status`, `/runs` |
| Web login brute force | Failure counter | Fixed per-attempt delay, then lockout (§15) | notify `web_lockout` |
| Re-auth flow left open | Idle timer on the noVNC stack | Automatic teardown after 15 min idle (§15) | notify `reauth_closed` |
| DB corruption | SQLite error on open/integrity_check at daemon start | Daemon refuses to run; weekly `VACUUM INTO /data/state.backup.db` limits blast radius | notify, unhealthy |

*Built.* The last two rows leaned on things 0.1.0 shipped without, and both are there now.

`gpb verify` re-reads every file the store calls done and compares it with the hash taken while it
was written. It is the only thing that reads a finished file again: a run notices a file that has
*gone* — `reclaimVanishedFiles` does that at the start of every pass — but a file quietly rotted by
a failing disk keeps its name, its size and its place in the pool, and nothing else would ever
look. It reads every downloaded file rather than only the ones still in the sync set, because a
photograph backed up before its album was unfollowed is still a file this backup put on the disk.
`--repair` puts the damaged ones back on the work list, where the next run fetches them and renames
the good copy over the bad one; nothing is deleted, since a corrupt file is still the only copy of
that photograph until its replacement has landed. An *unreadable* file — a permissions error, a
volume that came up late — is reported and never requeued: re-downloading a library on the strength
of a bad mount is a worse outcome than the fault. The command exits non-zero when it found
anything, so a monthly cron entry needs no output parsing to notice.

The weekly database copy is the daemon's own, an hourly tick that asks the age of
`/data/state.backup.db` rather than a timer this process holds — the same reasoning as the backup
schedule in §13, and it matters more here: this daemon is restarted with every image rebuild, and a
week counted in memory would be reset each time and never fall due. `VACUUM INTO` rather than a
file copy, because the live database is in WAL mode and a copy taken mid-run would be a torn one.
It writes `state.backup.db.part` and renames, so last week's copy is never removed before this
week's is known to have worked. A failure logs and fires the notify hook: a backup that fails
silently is not a backup.

Notifications are one mechanism: exec `notify.command` with `<event> <message>`. The user
already runs a Telegram notifier; a two-line hook script plugs into it. No built-in
Telegram/SMTP/webhook clients — the hook is the extension point and it is enough.
Messages that call for action carry the relevant `external_url` link so they are tappable
on the phone.

## 15. Security

The inherited stance, still true:

- The profile directory is a **full Google account credential** — not scoped to Photos.
  Anyone who can read `/data/profile` can read the user's Gmail. This is inherent to the
  approach and must be understood.
- Containment, not theatre: `/data` mode 0700, container runs non-root, cookies and tokens
  never logged (the `Session` type has no `String`/`MarshalJSON` exposing values), and drift
  reports carrying the arity and types of a payload but never a value from inside it — no
  payload is written to disk at all, so there is no second copy of the library to protect.
- At-rest encryption of the profile inside the same always-on box is deliberately **not**
  attempted: the daemon would need the key on the same disk to run unattended, so it adds
  a step, not a barrier. If at-rest protection matters, encrypt the volume itself.
- `--password-store=basic` stores cookies with a fixed obfuscation key — that is exactly
  what makes the profile portable and exactly why filesystem permissions are the real
  boundary. Stated so nobody mistakes it for encryption.

What is new: there is now an HTTP surface on the LAN fronting a live Google session and
able to trigger downloads. Threat model: the attacker is anything on the LAN — a guest
device, a compromised IoT gadget, a laptop with malware. The prize, in ascending order:
browsing the photo metadata, triggering syncs, and — the crown jewel — the re-auth page,
which is an interactive, fully authenticated Google browser.

### Password: argon2id

`golang.org/x/crypto/argon2`, `argon2.IDKey` with time=3, memory=64 MiB, threads=1,
32-byte key; per-hash random salt; stored as a PHC-format string in `config.toml`, written
by `gpb passwd`. Why argon2id over bcrypt: memory-hardness (GPU/ASIC resistance), no
72-byte input truncation, and it is the current recommendation for new systems. The
honest trade-off: bcrypt's `GenerateFromPassword`/`CompareHashAndPassword` are
harder to misuse, while `x/crypto/argon2` ships only the KDF, so we owe a ~30-line PHC
encode/verify helper — acceptable, and the parameters travel inside the stored string.
Verification cost (tens of ms, 64 MiB) is irrelevant at one login per weeks.

### Sessions

- 32 random bytes from `crypto/rand`, base64url, kept in the `web_sessions` table. Only a
  SHA-256 of the value is stored: the row is a bearer token, and a hash of one cannot be
  presented. No salt and no stretching — the input is 32 random bytes, so there is nothing
  to guess at.
- **They used to live in a map, and a restart logged the one user out.** That was recorded
  here as a feature-sized cost and turned out not to be: an appliance is redeployed often
  enough that being dropped on the login page mid-task became the ordinary experience of
  updating it. Reversed 2026-08-13.
- Cookie flags: `HttpOnly`; `SameSite=Lax`; `Path=/`. **Lax, not Strict, deliberately:**
  the `auth_required` notification link tapped in Telegram is a cross-site top-level GET
  navigation, which Strict would strip the cookie from — bouncing an already-logged-in
  user to the login page at exactly the moment we want re-auth to be one tap. Lax still
  withholds the cookie on cross-site POSTs. **`Secure` follows `web.tls`**: set when encryption
  is on, and not before — a `Secure` cookie on a plain-HTTP install is one the browser never
  sends back, which is a locked-out user rather than a safer one.
- Expiry: 30-day idle TTL (sliding), 90-day absolute. Token rotated on every successful
  login; logout deletes server-side.

### CSRF

Two stdlib-cheap layers: `SameSite=Lax` on the cookie, plus Go 1.25's
`http.CrossOriginProtection` wrapped around the mux, which rejects non-safe-method
requests whose `Sec-Fetch-Site`/`Origin` headers indicate another origin. Honest caveat:
browsers only send `Sec-Fetch-Site` to "trustworthy origins" (HTTPS or localhost), so on
plain LAN HTTP the protection rests on the `Origin`-header fallback (browsers do send
`Origin` on cross-origin POSTs regardless of scheme) plus SameSite. No token machinery,
no hidden form fields — and all mutating endpoints are POST, never GET. Turning encryption on
(below) retires that caveat rather than working around it: the origin becomes trustworthy by the
browser's own definition and `Sec-Fetch-Site` starts arriving. A second reason it is worth
turning on, and one nobody would think to look for.

**Build requires the toolchain `go.mod` asks for — currently Go 1.26.4 — and never one below
1.25.1.** Go 1.25.0 shipped `CrossOriginProtection` with a bypass-pattern flaw
(CVE-2025-47910) that let some cross-origin POSTs through; 1.25.1 tightened it to exact
matches. The version lives in `go.mod`, with the builder image tracking the same major line,
so a stale base image cannot silently reintroduce it.

### Login rate limiting

A fixed 1-second delay on every attempt, and after 10 consecutive failures a 15-minute
lockout. Both are **global, not per-IP** — on a LAN, source addresses are trivially
spoofable and there is exactly one legitimate user, so per-IP accounting adds bypasses,
not protection. A lockout fires the notify hook (`web_lockout`): someone is poking the
box, and the user should know.

### Bind address, firewalld

The Quadlet can publish on one host address only — the deployment it was written for did, and
the shipped template does not, because it cannot know the address (§13) — and firewalld requires
the single explicit port opening. Nothing else listens; websockify and any debug surface bind to
`127.0.0.1` inside the container.

### TLS, honestly

The password and the session cookie cross the wire in the clear under plain HTTP; no
amount of hashing at rest changes that. Options weighed:

- **Plain HTTP** — an on-path LAN attacker (rogue Wi-Fi client, ARP spoofing) captures
  the password and cookie, which yields the whole web UI *including the ability to open
  the re-auth page* (its password re-confirmation, §below, is the same sniffable
  password). LAN compromise means Google-session compromise.
- **Self-signed certificate** — encrypts the wire, but browsers scream on every device,
  and unless the cert is installed on each client it mostly trains warning-clicking; it
  authenticates nothing to a user who bypasses the warning.
- **Reverse proxy + ACME** — needs a public domain and DNS plumbing; over-engineering for
  one user on one LAN.

The recommendation here was **plain HTTP for v1**, on the stated assumption that the home LAN is
trusted, with Tailscale as the upgrade path and self-signed certificates dismissed as
warning-training.

**Built anyway, 2026-08-13, and the dismissal was half wrong.** What the argument above gets
right is that a self-signed certificate authenticates nothing; what it skips is that the
alternative was not authenticating nothing — it was *encrypting nothing*, against a threat model
(§15 opening) whose whole content is somebody on the wire. Against a passive listener on the LAN
the certificate is the entire difference, and against an active attacker it is no worse than what
it replaced. The usability objection is also smaller than it reads: this is one appliance that
one household reaches from a handful of devices, and the warning is accepted once per device
rather than per visit.

So `web.tls.mode` is `off | self-signed | provided`:

- **Off is still the default**, and that is deliberate rather than timid. An appliance has to
  answer *somewhere* before it can be configured at all, and an upgrade that moved a running
  install to an address the owner never chose would be a worse failure than the one it prevents.
- **Self-signed** writes a certificate into `/data/tls/` at the next start, key 0600, valid 397
  days — Safari refuses longer, and somebody tired of clicking through the warning will put this
  in a trust store, where that rule applies to a certificate made here exactly as to one from a
  public CA — and renewed a month before expiry at a restart. The names come from `external_url`,
  because inside a container the process cannot learn the address people browse to: its hostname
  is a random hex string and its own IP is on a network nobody browses from.
- **Provided** takes paths inside the container and is *loaded when you save*, not at the next
  start: a daemon that will not listen is a daemon whose settings page cannot be reached to
  correct the typo that stopped it.

Three consequences that only showed up once it existed:

- **The address has to move with the scheme.** `external_url` is what the sign-in links are built
  from, and one left saying `http` leads nowhere. The save that changes encryption rewrites the
  scheme and says so; no other save touches it, so anyone terminating TLS in a proxy in front of
  this is not fought. An address whose scheme is neither `http` nor `https` is left alone — a bare
  `host:8090` parses with the host in the scheme's place, and "fixing" that would eat the host.
- **The container's health command had to follow** (§13), or turning encryption on turns the
  container unhealthy.
- **Encryption is read once at startup**, which is what finally made a Restart button (§10) worth
  building: it is the only setting whose whole effect is that the page moves elsewhere, so
  "restart the way you started it" was advice nobody could act on from a phone.

Tailscale remains the better answer for reaching this from outside the house, and is untouched by
any of the above.

### The re-auth page — the highest-value target in the system

`/reauth` exposes, while active, a headful browser holding a fully authenticated Google
account. Its exposure is designed, not incidental:

- **Off by default.** Xvfb, headful Chrome, x11vnc, and websockify are not running.
  They are spawned only when an authenticated user clicks "Start browser login", and that
  action requires **re-entering the web password** — a stolen idle session cookie alone
  cannot open the flow.
- **Never directly reachable.** websockify binds to `127.0.0.1` inside the container; the
  only path in is the app's `/reauth/ws` proxy, which accepts only the session that started
  the flow — or, once that session no longer exists, the next one to ask. Ownership that
  outlives its owner is how a signed-in Google window ends up running with nobody able to
  close it, so logging out stops the browser it started, and a browser orphaned another way
  (an idle session, an abandoned tab) is adopted rather than stranded until the idle timer.
- **Two display modes, one flow.** The Xvfb/VNC chain exists solely because the server has
  no screen. On a machine that has one — in practice a macOS development box — the stack
  degrades to launching the headful browser on the host's own display, and `/reauth/ws`
  returns 501 because there is no canvas to bridge. This is not a convenience branch: the
  macOS Chrome build is a Cocoa application with no X11 backend, so it *cannot* render
  into Xvfb regardless of what is installed. Everything else — the password
  re-confirmation, the profile lock, ownership, teardown, the verification warmup — is
  shared. The security posture differs in one way worth stating: in host-display mode the
  browser is visible to whoever is at the machine rather than to whoever holds the web
  session, and the idle timer has no VNC traffic to measure, so it becomes a fixed cap
  measured from when the flow opened.
- **Torn down eagerly**: on detected login success (the next warmup asserts a logged-in
  state), on an explicit "Done" click, or after 15 minutes without VNC activity —
  whichever comes first. Teardown kills the headful browser; the profile keeps the
  result.
- **Audited**: every start and stop of the flow is logged and fires the notify hook, so a
  re-auth the user didn't initiate is loud.
- Residual risk, stated plainly: while a flow is open, whoever controls that web session
  controls a live Google browser. The mitigations bound the window and the audience; they
  do not eliminate it.

## 16. Testing strategy

The core problem: the true counterparty is unstable and cannot be meaningfully mocked into
safety. The strategy is to make everything *around* the protocol rigorously testable, keep
the protocol surface thin, and detect real-world drift fast.

- **Fixture/golden tests (`gphotos` decoders):** recorded real batchexecute responses,
  scrubbed of identifiers, checked into the repo. Decoders are pure `[]byte → structs`
  functions; drift fixes start by re-recording a fixture, which doubles as documentation
  of what changed.
- **Engine tests (`syncer`):** `Lister`/`Downloader` implemented by in-memory fakes with
  fault injection — truncated bodies, 429s with `Retry-After`, mid-stream auth failure,
  ENOSPC via a small tmpfs. Assert plan correctness, resume, atomicity (kill between write
  and DB update), and the state machine.
- **HTTP-level tests:** the real client against `httptest.Server` replaying fixtures,
  including the stale-cookie redirect dance.
- **Web handler tests:** `net/http/httptest` against the real mux with fake store/syncer.
  Cover the session middleware (no cookie → redirect to `/login`, expired session, token
  rotation on login), login failures and the lockout counter, cross-origin POST rejection,
  the album mode and pick-batch endpoints (including select-all-in-album against a large
  fake album), and review-queue actions. A render test walks every template with fixture
  data so template errors fail in CI, not in front of the user.
- **Thumbnail tests:** cache hit and miss paths, singleflight collapse under 200
  concurrent requests for one key, LRU eviction under an artificially small cap, and
  upstream failure → placeholder without cache poisoning — all via `httptest` with a fake
  upstream.
- **Live contract tests:** `//go:build live`, run manually (never in CI) against the real
  account using a dedicated small test album; asserts listing shape and one download
  round-trip with hash comparison. This, plus the production canary, is the honest answer
  to "how do you test against an unstable third party": you don't simulate it — you probe
  it cheaply, continuously, and fail loudly.
- **Store tests:** real SQLite files in `t.TempDir()` — the modernc driver makes this free.

## 17. Go package layout

```
gpb/
  cmd/gpb/main.go          subcommand dispatch (stdlib flag; no CLI framework)
  internal/auth/           chromedp lifecycle, warmup, Session export, re-auth flow
  internal/gphotos/        batchexecute client, decoders, download, thumbnails; fixtures/
  internal/store/          sqlite open/migrations, queries, selection model
  internal/syncer/         planner, worker pool, limiter/backoff, file writes
  internal/engine/         assembling a run: warm the profile, harvest the session, wire it up
  internal/links/          the album symlink view of the pool (§8)
  internal/thumbs/         thumbnail cache, fetch orchestration, eviction
  internal/web/            handlers, session/CSRF/rate-limit middleware, TLS, templates/,
                           static/ (embed.FS), noVNC proxy
  internal/daemon/         schedule loop, canary, health state, notify hook
  internal/homeassistant/  MQTT discovery, state publishing, command topics
  internal/config/         toml load/defaults, passwd hashing helper
  internal/version/        the release number, and nothing else
```

Three of those were not in the plan. `engine` came out of `daemon` and the CLI needing the same
five steps before a run; `links` out of §8 turning into more than a helper; `version` out of the
release, which needed one place for the number rather than a constant per caller.

Interfaces (`Lister`, `Downloader`, the thumbnail fetcher) are declared where they are
consumed, per Go convention. No `pkg/`, nothing exported: this is a program, not a
library.

## 18. Build plan (each phase ships something usable)

The web UI replacing the TUI reorders the plan: the auth flow now *lives inside* the web
UI, so a minimal web server moves up from the old "phase 3, UI last" position into
Phase 1 — it is the door to everything else.

- **Phase 0 — spike (days, throwaway code).** In a logged-in desktop browser: record
  batchexecute listing calls, replicate album list + album page in a scratch Go client
  with pasted cookies; capture the Shift+D download request and attempt it over plain
  HTTP, byte-comparing against the browser's file (EXIF/GPS intact, video untranscoded).
  Additionally capture one thumbnail URL from a listing and test it with and without
  cookies, noting expiry (§5, §11). **Gate:** decides pure-HTTP download vs
  browser-download fallback and settles the edited-photos and thumbnail-auth questions.
  Fixtures recorded here seed the test suite.
- **Phase 1 — auth core + web shell.** Warmup, session export, staleness detection,
  keepalive loop, `status`; the minimal web server: password login and sessions,
  `/reauth` with the embedded noVNC flow, a bare status page. Quadlet unit deployed on
  the real box. Deliverable: a container on the Fedora server that can hold a Google
  session for weeks, with re-auth one tap from the phone — and the Chrome-sandbox and
  login-from-server-IP questions answered on real hardware.
- **Phase 2 — headless sync MVP.** Store, `gphotos` listing + download, syncer with
  resume/verify, `gpb sync`. Deliverable: real unattended-capable backups of chosen
  albums — the project is already useful before it is pretty. *Done.* Selection turned
  out not to need hand-edited SQL: `gpb albums`/`follow`/`unfollow` were cheap enough to
  build alongside, so Phase 3's web picker replaces a working CLI rather than a stopgap.
- **Phase 3 — web curation.** Album list with sync modes, the thumbnail subsystem and
  item grid with picks, the review queue, runs/history page. Replaces hand-edited
  selection. *The album list, the album symlink view (§8), the thumbnail subsystem (§11) and
  the item grid with picks are built.* The symlink view turned out to belong here rather than
  in Phase 5: it is what makes the backup browsable in a file manager, and pruning it is how
  an unfollow becomes visible on disk. `sync_mode = picked` is now usable end to end.
  Remaining: the review queue and the runs/history page.
- **Phase 4 — daemon polish.** Scheduler, canary, notifications, healthcheck wiring,
  settings page, Quadlet/auto-update polish. Deliverable: the full always-on appliance. *Done,*
  and it collected the things that only a running appliance asks for: the Home Assistant bridge
  (§20), https (§15), the Restart button (§10), and sessions that survive a redeploy.
- **Phase 5 — niceties, on demand.** `verify` sweep, `[Library]`
  full-library mode if not already trivial, weekly DB backup. *Done.* `[Library]` shipped early
  with Phase 3; the `verify` sweep and the weekly `VACUUM INTO` (§14) are what 0.1.0 left open and
  landed straight after it. Neither is a nicety in the end — between them they are the only reason
  to believe the backup on the disk is still the backup that was made.
- **Phase 6 — the release, which was not in the plan.** Apache-2.0, a version constant and
  subcommand, a compose file beside the Quadlet unit, and the deployment guides rewritten for
  somebody who does not have this box: `<host>` and `%h` where one address and one home
  directory used to be, and every measured fact kept, because the measurements are the argument.
  What the README owed a stranger was different from what it owed its author — chiefly that
  Google neither offers nor supports this, that the profile is a whole Google account, and that
  an image carrying Chrome would be redistributing Chrome.
- **Phase 7 — an image to pull, 0.1.1.** The Chrome constraint had left the whole audience
  building their own; taking Chrome *out* of the image and letting the container fetch it from
  Google on first start (§13) removes the constraint rather than living with it. Published as
  `sodre90/gpb` and deployed straight onto the box it was written for, which is the only test that
  counts: the live install now runs the same artefact a stranger would pull.

## 19. Open Questions / Decisions Needed

Assumptions I made to keep moving are marked (A); genuine unknowns are marked (?).
*(Resolved and removed since the last revision: passkey-only login — the account uses
password + phone prompt; server architecture — x86_64; TUI-runs-on-NAS; photos volume on
a network filesystem — local disk.)*

1. **RESOLVED for stills — pure-HTTP download works.** Phase 0 fetched an original from
   `video-downloads.googleusercontent.com` with **no cookies**: HTTP 200, correct
   `Content-Disposition` filename, GPS coordinates and camera model intact at full
   resolution. The browser-driven `Downloader` fallback is not needed for photos.
   Video confirmed too: 522 MB `video/mp4`, untranscoded, no cookies. **Both of the old
   API's fidelity failures are absent from this path.**

   **Critical corollary — `=d` must never be used.** Item listings embed an `lh3` base URL
   whose `=d` suffix returns a plausible-looking JPEG with *no EXIF segment at all*, no
   GPS, and a 4K 16:9 re-encode instead of native resolution. It would produce backups
   that fail silently. §8's fidelity checks should assert an APP1/Exif marker on stored
   stills so this can never creep in.

   **URL minting also resolved.** `VrseUb` with payload
   `[mediaKey, null, null, null, albumId]` returns a signed download URL at `[1]`. The
   full chain — `F2A0H` albums → `snAcKc` items → `VrseUb` URL → plain GET — is proven end
   to end over HTTP; only auth needs a browser, exactly as §3 assumed. The `Downloader`
   browser fallback can be dropped from Phase 2 scope (keep the interface; it costs
   nothing and is the natural seam if this drifts).

   Remaining sub-question: **signed-URL lifetime is unmeasured**, which matters for how
   long a sync run may hold a minted URL before fetching. Phase 2 sidesteps it by minting
   per fetch attempt rather than caching URLs across a run, so a resume gets a fresh URL
   instead of a mysterious 403. Worth measuring anyway if minting ever becomes a bottleneck.

   **NEW, Phase 2 — `VrseUb` mints on two hosts, and both serve originals.** Measured live
   on 2026-08-10. The same call answers some items with a
   `photos.fife.usercontent.google.com` URL rather than a `video-downloads` one, per item
   and stably (0 of 6 items changed answer on an identical re-probe; `source-path` makes no
   difference; one album gave 4 of 8 on each host, another 3 and 5).

   **I first read the fife host as a display derivative and made `DownloadURL` refuse it.
   That was wrong**, and it would have silently skipped roughly a third of a shared album.
   The two hosts differ in *authentication*, not in fidelity:

   | host | session cookies | serves |
   |---|---|---|
   | `video-downloads.googleusercontent.com` | refuses them / not needed | original |
   | `photos.fife.usercontent.google.com` | **required** — 403 without | original |

   The refusal was an artefact of fetching fife anonymously, which is what the old
   `Fetch` did by construction. Fetched *with* the session, fife returns HTTP 200,
   `Content-Disposition: attachment` carrying the camera's own filename, the item's exact
   advertised resolution, and — decisively — an intact **maker note**, the proprietary EXIF
   block Google's display re-encoder strips and cannot synthesize. A sweep of 22 items
   (stills on both hosts, videos to 4K) delivered full resolution every time, none failed.

   Consequences now implemented:

   - `ErrNoOriginal` is **gone**. There is no known undownloadable item.
   - `contentHosts` maps each host to whether it needs the session; `Fetch` picks the
     transport from it and returns `ErrUnknownContentHost` for anything else, so the
     account's cookies can never be sent to a host named by a remote response.
   - The client keeps a **separate download `http.Client`**: it shares the jar but drops
     the 60 s RPC timeout, which would otherwise have killed any download over ~60 s. The
     live sweep pulled a 335 MB video through it.
   - The listing hint (metadata key `146008172`, flag `37` vs `18`) predicted the *host*
     exactly for stills and not at all for videos. Since the host no longer implies
     availability, the hint is unused — noted only so nobody mines it again.

   Since nothing is undownloadable, the per-item policy setting discussed for this case is
   moot and was not built.

   Caveat worth keeping: "full resolution" here means pixel dimensions matching the item
   listing plus a surviving maker note. That is strong evidence of the original file, not a
   byte-for-byte proof against a known source — which this account cannot supply.
   `TestLiveEveryItemDownloadsAtFullResolution` re-checks it on demand.
2. (?) **Thumbnail URL auth — partially answered, and worse than assumed.** Phase 0 found
   thumbnails live on `photos.fife.usercontent.google.com`, which rejects the app host's
   session cookies: anonymous gets 403, and the full captured cookie set gets redirected
   to `ServiceLogin?passive=true&osid=1` and then to an interactive sign-in. Content hosts
   want a host-scoped `OSID` cookie minted by that passive handshake. Unresolved whether a
   correctly-scoped cookie export (chromedp `storage.GetCookies`, which carries real
   domain/path attributes, unlike a captured `Cookie:` header) succeeds where the spike's
   over-broad jar failed. If it does not, §11's thumbnail cache must be filled through the
   browser rather than the HTTP client — which changes §11's cost model, since thumbnails
   are the highest-volume request class in the system. Retest early in Phase 1.

   **Phase 1 partial:** `Session.Jar()` now exists and preserves real per-cookie scoping,
   so the retest is unblocked — but it needs a signed-in session, which Phase 1 does not
   have on this machine, so the question stays open. One assumption in the original
   write-up is already disproven: `photos.fife.usercontent.google.com` *is* a subdomain of
   `google.com`, so a `.google.com` domain cookie does reach it, exactly as in a browser.
   The missing credential is therefore specifically the host-scoped `OSID`, not the
   account cookies. That narrows the fix to replaying the `ServiceLogin?passive=true&osid=1`
   handshake once per content host and keeping the resulting cookie in the jar.

   **Phase 2 — largely answered, and better than feared.** The download work above fetched
   `photos.fife.usercontent.google.com` successfully using nothing but `Session.Jar()` from
   a warmed-up profile: HTTP 200, no `ServiceLogin` redirect, no bespoke handshake. The
   spike's failure was its over-broad replayed `Cookie:` header, exactly as suspected — a
   properly scoped jar just works. No passive-`OSID` dance is needed, so §11's thumbnail
   cache can stay on the HTTP client and its cost model holds. Still strictly (?) for
   *thumbnail* URLs specifically, which were not themselves retested; but they share the
   host with the download URLs that now demonstrably work.
3. (?) **Session longevity numbers.** "Months, if exercised; rotation roughly daily" is
   community folklore, not documentation. The 12h keepalive default may be tunable way up
   or may need to come down; Phase 1 on the real server will tell.
4. **DECIDED — plain HTTP on the LAN. Reopened and reversed, 2026-08-13.** The original
   sign-off stood on a LAN-trust assumption, with Tailscale as the upgrade and self-signed
   certificates dismissed. Encryption now ships as an off-by-default setting with a self-signed
   or a provided certificate, because the comparison that mattered was not "authenticates
   nothing" against "authenticates properly" but against "encrypts nothing", on a wire whose
   threat model is a listener. Full reasoning, and what it dragged with it — the address moving
   scheme, the health check, the Restart button — in §15. Tailscale is still the answer for
   reaching this from outside the house, which is question 5.
5. (?) **Re-auth reachability away from home.** The tappable `auth_required` link assumes
   the phone is on the home Wi-Fi. If re-auth should work from anywhere, the UI must be
   exposed on a tailnet — which also changes the answer to question 4. Decide whether
   away-from-home re-auth is worth that.
6. (A) **Albums-first scope.** v1 targets album-based curation; `[Library]` whole-library
   mode uses the same machinery but ships only when the timeline enumerator is proven.
   Confirm that album-level backup is the primary need.
   *Settled 2026-08-10:* the enumerator is proven (§5) and the row ships in `all` mode with a
   start date. Picking within the library is deliberately not offered — the presentation
   question that a grid of 122,331 items would raise is left unanswered rather than guessed at.
7. (A) **Never delete local files**, even when items are deleted upstream or unselected
   in the UI (unselect stops future syncs, keeps existing files). Confirm this retention
   policy.
8. (A) **No per-file JSON sidecars in v1**; Google-side metadata lives in SQLite only.
   Confirm — this matters if the pool should be portable into other photo tools that read
   sidecars.
9. (A) **Notification = exec hook** wired to the existing Telegram notifier; no built-in
   channels.
10. (?) **Edited photos.** Which variant does the web download endpoint return for edited
    items, and should edits ever trigger re-download? v1 policy: keep first-downloaded
    bytes, never silently replace. Settle after the spike.
11. (A) **Single Google account.** Multi-account would touch profile layout, DB, and
    paths; not designed for.
12. (A) **Deployment defaults to confirm:** port 8080, host paths `/srv/gpb/{data,photos}`,
    web session TTL 30 days idle / 90 absolute, thumbnail cache cap 1 GiB. All are
    config, none are load-bearing; flagged so they are chosen, not inherited.
    *Settled by the release:* published port **8090** (8080 is the port every other home-server
    container wants), host paths the deployer's own — `%h/gpb/{data,photos}` as the template's
    default — and the TTLs and cache cap kept as they were.
13. **RESOLVED — naming.** The binary is `gpb`, and so is the repository; the `takeout-backup`
    name is gone from everything but the checkout path on the box it was first deployed to.
    Settled at the 0.1.0 tag, which is also where the product name stopped being an internal
    matter: "gpb" is the name, "Google Photos" appears only descriptively, and the
    not-affiliated disclaimer lives in NOTICE.
14. **RESOLVED — how to tell a logged-out session from a logged-in one.** The original
    plan leaned on the landing URL: bounce to `accounts.google.com` means signed out.
    Phase 1 disproved it. A logged-out visit to `photos.google.com` is served Google
    Photos' **public landing page** — same host, no redirect — so a URL check reports a
    healthy session for an account that cannot list a single album. Detection now requires
    both a `WIZ_global_data.SNlM0e` token *and* a signed-in account cookie
    (`SID`/`SAPISID`/`__Secure-*PSID` family); logged-out visitors only ever get
    `NID`/`AEC`/`SOCS`. The URL check is kept, demoted to a secondary signal that still
    usefully distinguishes `signin/rejected` (automation block) from ordinary expiry.
    Phase 2 gets the strongest signal available for free — the first real `F2A0H` call —
    and should upgrade the health check to use it.

## 20. Home Assistant (MQTT)

Named a broker in `[mqtt]` and the daemon publishes what the backup is doing, using Home
Assistant's own MQTT Discovery so nothing has to be written in YAML at the other end.

- **Discovery.** One retained config message per entity at
  `<discovery_prefix>/<component>/<topic_prefix>/<object_id>/config`, each carrying the same
  `device` identifiers so the whole set lands as one device. Republished on every connect: the
  last will marks this gpb offline the moment the broker notices it is gone, and a broker that
  lost its retained messages has to be told everything again.
- **State.** One retained JSON message on `<topic_prefix>/state`; every entity reads a field
  of it with a `value_template`. Published only when something has actually changed — a run
  downloading at a few files a second would otherwise put that rate on somebody's broker for
  no information at all. Sampled every 5s, slower than the web UI's poll because this runs for
  as long as the daemon does and each sample counts rows in a database a backup is writing to.
- **Availability.** `<topic_prefix>/availability` carries `online`/`offline`, set as the
  client's last will and said again on the way out, so the entities grey out rather than
  freeze at their last value when the container stops.
- **Control.** Two buttons (back up now, refresh albums) and a whole-library selector publish
  to `<topic_prefix>/command/<name>`; per-album mode goes to
  `<topic_prefix>/album/<id>/mode/set`. Every one of them goes through the same runner and
  store the web UI's buttons use, so a run asked for from a dashboard is refused by the same
  one-run-at-a-time rule as one asked for from the page.
- **Failure policy.** A broker that cannot be reached is retried every 30 seconds, or at once
  when the settings are saved, and the last refusal is shown on the settings page with the
  reason the broker gave. It stops nothing else: a house automation system is not a reason to
  stop backing photographs up, and it is also not a reason to make somebody restart the
  daemon after typing in the password it was waiting for.
