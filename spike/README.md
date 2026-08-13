# Phase 0 spike

Throwaway code. Its only job is to answer the questions in DESIGN.md §19 that decide the
shape of the real application:

1. **The gate.** Can the original file — the exact bytes the Download button yields — be
   fetched with a plain authenticated HTTP GET, with GPS intact and video untranscoded?
   If no, the real app falls back to browser-driven byte transfer (slower, already
   designed for behind the `Downloader` interface).
2. Do listing calls work outside a browser at all, given only the session cookies and the
   `at` token?
3. Do thumbnail URLs need cookies, and do they expire?
4. For an edited photo, which variant does the download return?

Nothing here is meant to survive into the real binary. What survives is the **captured
fixtures** and what we learn about the wire format.

RPC ids are deliberately not hardcoded anywhere. They drift, and guessing them would send
us debugging phantoms. The tool learns them from traffic you capture.

## Security

`captures/` will contain **live Google session cookies**. Anyone holding one of those
files can act as you on your Google account until the session is revoked. They are
gitignored. Do not paste their contents anywhere. When the spike is done, delete the
directory and — if you want to be thorough — sign out of that browser session to
invalidate them.

## Capturing

Use desktop Chrome, Chromium, or Edge; Safari and Firefox have a different copy format
that this parser does not read.

1. Log in to <https://photos.google.com>.
2. Open DevTools → **Network** tab. Tick **Preserve log**. Filter on `batchexecute`.
3. Navigate to **Albums**. Requests will appear.
4. Right-click the most substantial one → **Copy** → **Copy as cURL (bash)**.
   On macOS the menu item may read *Copy as cURL*; make sure it is the bash variant, not
   cmd or PowerShell.
5. Save it:

```sh
mkdir -p captures
pbpaste > captures/albums.curl
```

Then repeat for an album's contents (open an album, capture the request that fires) into
`captures/album-items.curl`.

## Findings so far

Captured 2026-08-09 from Chrome 151 on macOS, locale `hu`.

**Listing works outside the browser — confirmed.** Replaying a captured
`batchexecute` from Go returns HTTP 200 with real data. The session needs only the
cookie jar and the `at` token; no browser is required for the chatty part of the work.

- Album listing is rpcid **`F2A0H`**, payload `[pageToken, null, 2]`.
- Response payload is `[albums[], nextPageToken]`, 25 albums per page.
- Album entry fields: `[1]` title, `[2]` cover thumbnail `[url, w, h, …]`, `[3]` item
  count, `[4]` created timestamp in ms, `[6]` album id (`AF1Qip…`).
- **`[1]` title is nullable** — 3 of the first 25 albums are untitled. A strict decoder
  must not treat title as required. `[3]` and `[6]` were populated on every entry.
- Paging works with a self-constructed call, not just a replay: passing the cursor back
  as element 0 returned the next 25 albums with **zero id overlap** and a further cursor.
- 25 cookies ride along, including `__Secure-1PSIDTS`/`__Secure-3PSIDTS` — the rotating
  pair the keepalive design in DESIGN.md §6 depends on.

**Content hosts are a separate auth domain — this is the surprise.** The cover-thumbnail
URLs in the listing live on `photos.fife.usercontent.google.com`, and that host does *not*
accept the session cookies that `photos.google.com` accepts:

- No cookies → `403`, with an 832-byte error PNG.
- All captured cookies, in a properly scoped `net/http/cookiejar` → `302` to
  `accounts.google.com/ServiceLogin?passive=true&osid=1&continue=…`, then on to the
  interactive sign-in form. 1.2 MB of HTML, not an image.

The `osid=1` parameter is the tell: content hosts want an `OSID` cookie scoped to
*that host*, which the browser obtains through that passive handshake. Our capture holds
`OSID` and `__Secure-OSID`, but scoped to the app host, and the passive flow did not mint
a new one for us.

Caveat, stated deliberately: a `Cookie:` header captured from `photos.google.com` cannot
tell us each cookie's real domain and path, so this spike marks them all `google.com`,
which is broader than a browser would send and may itself be why the passive flow
refused. A cookie export from chromedp's `storage.GetCookies` carries true scoping and
might behave differently. **Treat this as "plain HTTP did not work with what a capture
gives us", not as "content hosts are impossible".** Retest in Phase 1 with a real export
before concluding.

**The download gate: PASSED, for photos.** Originals are served from
`video-downloads.googleusercontent.com` (that host serves stills too, despite the name)
via a long signed path with a single `authuser=0` query parameter.

- The URL carries **no cookies at all**. Fetched with `-anon`, it returned HTTP 200,
  `Content-Type: image/jpeg`, `Content-Disposition: attachment; filename="…"` with the
  original filename, 7,156,429 bytes.
- **EXIF and GPS survive**: latitude/longitude present, `Galaxy S26 Ultra` in the camera
  model, full 3056×2296 resolution, `ffd8 ffe1 … Exif` APP1 segment intact. This is the
  metadata the old Library API stripped.

So byte transfer needs no browser and no session — a plain `http.Get` suffices. The
`Downloader` browser fallback in DESIGN.md §5 is not needed for stills.

**Video is original too.** The same probe on a video returned `video/mp4`,
**522,414,429 bytes**, again with no cookies. A transcode would be a fraction of that.
Both of the old Library API's fidelity failures — stripped GPS and transcoded video — are
absent from this path.

**`=d` on the listing's `lh3` base URL is a TRAP. Do not use it.** Album items embed an
`lh3.googleusercontent.com/pw/…` base URL, and appending `=d` returns HTTP 200,
`image/jpeg`, `Content-Disposition` with the right filename, no cookies needed. It looks
exactly like a download. It is not the original:

| | signed download URL | `lh3` base + `=d` |
|---|---|---|
| GPS | present | **absent** |
| Camera model | `Galaxy S26 Ultra` | **absent** |
| Header | `ffd8 ffe1 … Exif` (APP1) | `ffd8 ffe0 … JFIF` (APP0 — no EXIF at all) |
| Resolution | 3056×2296 (native) | 3840×2160 (4K 16:9 re-encode) |

This is the single most dangerous finding in the spike. `=d` produces backups that pass a
casual eyeball check while being stripped, re-encoded derivatives. DESIGN.md §5 predicted
this; it is now measured.

**Album listings carry an unauthenticated whole-album ZIP URL.** In the `snAcKc` response,
`[3]` is album metadata — `[0]` id, `[1]` title, `[3]` a
`video-downloads.googleusercontent.com/ADGPM…` URL. A ranged GET with no cookies returns
`application/zip`, magic `PK\x03\x04`. Useful as a bulk-seed or fallback, but it is the
whole album every time, so it cannot serve incremental sync.

**URL minting solved — rpcid `VrseUb`.** Opening a single photo fires it with payload
`[mediaKey, null, null, null, albumId]`, and the response carries a signed
`video-downloads.googleusercontent.com` URL at top-level `[1]`. Fetching that URL with no
cookies returned the true original: 7,296,271 bytes, GPS `39.839178`,
`Galaxy S26 Ultra`, 4000×3000 native, `ffd8 ffe1 … Exif` APP1 present.

(Opening a photo also fires `fDcn4b`, which returns thumbnail base URLs and neighbouring
items — useful for a detail view, not needed for sync.)

### The complete proven chain

```
F2A0H  [pageToken, null, 2]                      -> [albums[25], nextCursor]
snAcKc [albumId, pageToken]                      -> [_, items[], nextCursor, albumMeta]
VrseUb [mediaKey, null, null, null, albumId]     -> [_, signedDownloadURL, …]
GET signedDownloadURL                            -> original bytes, no cookies, EXIF intact
```

Every step works over plain HTTP with a captured session; only the download itself needs
no session at all. Nothing in the sync path requires a browser.

Also unmeasured: **signed-URL lifetime**. Responses carry `Expires` values at roughly
fetch time, but that is a cache directive, not proof the URL expires.

## Running

Note: Go's `flag` package stops at the first positional argument, so flags come **before**
the capture path — `call -save captures foo.curl …`, not `call foo.curl … -save captures`.

```sh
cd spike
go test ./...                                  # parser sanity, no network
go run . inspect captures/albums.curl          # offline: sends nothing
go run . replay  captures/albums.curl          # the first real question
```

`inspect` prints the rpcids, the decoded payload shapes, which cookies are present and
whether an `at` token came along. `replay` re-issues the request from Go. If that returns
frames rather than a login page, the session works outside the browser and step 2 of the
gate is answered.

Once we know an rpcid and its payload shape, we can vary it without recapturing:

```sh
go run . call captures/albums.curl <rpcid> '<payload-json>'
```

## The download probe — the actual gate

1. In Google Photos, open a **single photo you know has GPS data** (something shot on a
   phone with location on).
2. With DevTools Network open and **Preserve log** ticked, press **Shift+D**.
3. In the request list find the one that actually serves the bytes — it will be a GET,
   usually to a `googleusercontent.com` host, with a large response and a
   `Content-Disposition` header. Copy as cURL → `captures/download.curl`.
4. Also let the browser finish saving the file normally; we need it for comparison.

```sh
go run . get captures/download.curl -o probe.jpg     # replays the captured URL
go run . hash probe.jpg
go run . hash ~/Downloads/<the browser's copy>.jpg
```

**Identical sha256 means the gate is passed.** Different, or a non-200, means read the
status and redirect chain the tool prints before concluding anything — a redirect to
`accounts.google.com` means the session was rejected, which is a different failure from
the URL being unusable outside the browser.

If the hashes match, confirm the bytes are genuinely the original rather than a stripped
re-encode:

```sh
exiftool -gps:all -make -model -createdate probe.jpg
```

GPS fields present means the old Library API's stripping does not apply here. Repeat the
whole probe with a **video** — that is where the old API transcoded, and it is the case
most likely to differ.

Also worth capturing while you are here: whether the download URL still works later
(re-run `get` an hour on) and whether it works without the session:

```sh
go run . get captures/download.curl -anon
```

## The thumbnail probe

Take any `googleusercontent.com` thumbnail URL out of an `inspect` payload and:

```sh
go run . get captures/albums.curl '<thumb-url>=w256' -o thumb.jpg
go run . get captures/albums.curl '<thumb-url>=w256' -anon
```

Whether `-anon` succeeds decides whether §11's cache needs the session on every fetch.

## Recording what we find

Answers go back into DESIGN.md §19 as resolutions, and any payload we successfully decode
should be saved under `captures/` as a fixture for the real test suite — scrubbed of
cookies first.
