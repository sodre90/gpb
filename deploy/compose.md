# Installing gpb with Docker Compose

The short road: fetch one file, up. If you would rather run it under rootless Podman as a systemd
service, that is [quadlet.md](quadlet.md) and it is a different page — you do not need both.

You need Docker Engine with the Compose plugin (`docker compose version` ≥ v2), an x86-64 machine
(Google ships Chrome for Linux on amd64 only), about 2 GB of memory free for Chrome, and room on
whatever disk the photographs are going to land on.

## 1. Get the compose file

```bash
mkdir gpb && cd gpb
curl -O https://raw.githubusercontent.com/sodre90/gpb/main/docker-compose.yml
```

The image it names, [`sodre90/gpb`](https://hub.docker.com/r/sodre90/gpb), carries no Google
Chrome: Chrome is not free software and not ours to hand on, so the container fetches it from
Google into its own data volume the first time it starts, under the terms you accept there. It
says so in the log when it does. Clone the repository instead if you would rather build the image
yourself — the compose file has a commented `build: .` for exactly that.

## 2. Say where the two directories go

They are separate on purpose. The data directory must be private — it holds the Chrome profile,
which is a full Google account credential and not a Photos-scoped token, so mode 0700 is the actual
security boundary (DESIGN.md §15) — and it wants about a gigabyte free, since Chrome itself lands
there too. The pool grows without limit and belongs wherever there is room.

Defaults put both in the checkout. To put them anywhere else, write a `.env` beside
`docker-compose.yml` — the compose file reads these four and nothing else, so a `git pull` never
walks over what you set:

```
GPB_DATA=/srv/gpb/data
GPB_PHOTOS=/mnt/media/gpb/photos
GPB_PORT=8090
TZ=Europe/Lisbon
```

Set `TZ` deliberately: the daily backup time is read in that zone, and so is the date a
photograph is filed under in the pool.

Then make them, and give them to the user the container runs as — uid 1000, which is the `gpb`
user inside the image:

```bash
mkdir -p data photos          # or the paths you put in .env
chmod 700 data
sudo chown 1000:1000 data photos
```

## 3. Start it

```bash
docker compose up -d
```

The first start takes a minute or two longer than the rest: it pulls the image, then downloads
Chrome into the data directory, which it does once and never again. `docker compose logs -f gpb`
watches it, and the line to look for is `gpb: Google Chrome <version> at /data/chrome/…`.

## 4. Set the web password

Nothing else needs configuring, but this does: there is no default password and the UI is
unusable without one.

```bash
docker compose exec gpb gpb passwd
```

Then open `http://localhost:8090/` — or the box's address from another machine — and log in.
A fresh install has no Google session yet, so the page will say so and offer the browser sign-in;
that is the next thing to do, and you can do it from a phone.

## 5. Point it at its own address

The daemon writes `config.toml` into the data directory on first start with an `external_url` of
port 8080, which is the port *inside* the container. The links it builds — chiefly the one into
the sign-in browser — have to match the address you actually browse to, or they lead nowhere.

Settings → *How this page is reached* → **Address**, or edit the file directly:

```toml
[web]
external_url = "http://<your-box>:8090"
```

## 6. Encryption, optionally

Off by default, and worth turning on: the password typed into this UI unlocks a whole Google
account, and the session cookie behind it is a bearer token for the same. On a flat home LAN both
are readable by anything else on the wire.

Settings → *How this page is reached* → **Encryption**. Set the address first: the certificate is
made for that name and no other, because inside the container the process has no way to learn the
address you browse to — its own hostname is a random hex string and its own IP is on a network
nobody browses from.

- **A certificate gpb makes.** Written into the data directory under `tls/` at the next start,
  key at mode 0600, valid 397 days and renewed a month before that. Your browser will refuse it
  once and offer to continue; that is what a self-signed certificate is, not a fault.
- **A certificate you provide.** Give the paths *inside* the container — `/data/...` is your
  data directory — so a certificate dropped in there is the easy case. It is loaded when you
  save, so a wrong path is refused on the page rather than discovered as a daemon that will not
  start.

The address moves to `https://…` on its own when you change this, and the page says so. Encryption
is read once at startup, so it needs a restart: the button at the foot of the settings page does
it and names the address to come back to. That button is why the compose file says
`restart: unless-stopped` — the daemon stops itself cleanly and Docker is what brings it back.

## Day to day

```bash
docker compose logs -f gpb                      # what it is doing
docker compose exec gpb gpb version             # which release is running
docker compose exec gpb gpb status              # what the Google session is doing
docker compose exec gpb gpb verify              # re-read the backed-up files, check every hash
docker compose exec gpb gpb duplicates          # photos kept as more than one file (--delete, --link)
docker compose pull && docker compose up -d     # update
```

A new image never touches the data directory: Chrome, the profile, the database and the
certificate live on the volume and survive. Chrome is fetched once and then left alone, so an
install that has been running for a year is running the browser it downloaded a year ago — to
take the current one, `rm -rf <data>/chrome` and restart, and the next start fetches it again.

The daemon runs `verify` itself once a month, an hour or more after it starts, and never starts
one during a backup — though a backup that falls due mid-sweep still runs, sharing the disk. It
reads every file, so it takes as long as reading the pool takes; it repairs nothing, and if
anything has rotted it fires the notify hook as `verify_problems` and leaves
`gpb verify --repair` to you. `<data>/verify.last` says when the last one finished and what it
found — delete it to have the next one start within the hour.

The pool shares a photo held under two keys as one file with two names, so anything that copies it
elsewhere should preserve hardlinks (`rsync -H`). Every backup ends by linking the copies an older
release left as separate files, so the first one after upgrading runs hours longer on a large pool;
the Review page's button, or `gpb duplicates --link`, does it sooner. The daemon keeps its own weekly copy of the database at
`<data>/state.backup.db` — that one needs nothing from you beyond letting whatever backs up the
host pick the file up.

## Two things that look like faults and are not

- **The container reports `unhealthy` until the first Google sign-in.** The health command asks
  after the Google session, not the socket, and a fresh profile has none. It goes healthy within
  a few minutes of signing in.
- **Under *rootless Podman* rather than Docker, the bind mounts come out owned by somebody else**
  and the daemon cannot write. Podman maps the container's uid 1000 to a subordinate uid on the
  host. Add `userns_mode: keep-id` to the service, or use [quadlet.md](quadlet.md), which is
  built for that case.

The [notes that apply to any install](README.md#notes) — why Google Chrome and not Chromium,
the sandbox fallback, reading a warmup in the log — are worth a minute before you need them.
