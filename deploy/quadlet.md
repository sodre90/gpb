# Installing gpb under rootless Podman and systemd

This is what gpb actually runs on: a rootless Podman container managed by a systemd Quadlet unit,
written against Fedora. It survives reboots, restarts itself, and keeps the photo pool owned by
your own login user. If you would rather have three commands and a `docker-compose.yml`, that is
[compose.md](compose.md) and it is a different page — you do not need both.

Everything below runs as the *unprivileged* user that will own the container, never as root.
`<host>` is the box you are deploying to.

## 1. Host directories

The two volumes are separate on purpose. The data dir must be private and wants about a gigabyte
free — Chrome lands in it alongside the profile and the database; the pool grows without limit and
belongs on whatever disk has room.

```bash
mkdir -p ~/gpb/data
chmod 700 ~/gpb/data

sudo mkdir -p /mnt/<media-disk>/gpb/photos
sudo chown -R "$USER:$USER" /mnt/<media-disk>/gpb
```

`~/gpb/data` holds the Chrome profile, which is a full Google account credential — not scoped
to Photos. Mode 0700 is the real security boundary here (DESIGN.md §15).

The `:Z` on both volumes is a no-op where SELinux is disabled, and is kept so the unit stays
correct where it is not.

## 2. Get the image

```bash
podman pull docker.io/sodre90/gpb:0.2.4
```

It is linux/amd64: Google ships Chrome for Linux on that architecture and no other. The image
carries no Chrome — Chrome is not free software and not ours to hand on, so the container fetches
it from Google into `~/gpb/data` the first time it starts, under the terms you accept there. See
the [notes](README.md#notes) and the Licence section of the top-level README.

To build it yourself instead, get the source onto the box — clone it if the box can reach the
repository, or copy the tree over if it cannot:

```bash
rsync -a --delete --exclude .git --exclude .beads --exclude spike \
    ~/prj/gpb/ <host>:~/prj/gpb/
ssh <host> 'cd ~/prj/gpb && podman build -t localhost/gpb:latest .'
```

`spike/` is excluded because a local run of it can leave captured Google session cookies behind,
and `.beads/` because the issue tracker's database is not part of what runs. Point `Image=` in the
unit below at `localhost/gpb:latest` if you go this way.

## 3. Install the Quadlet unit

The unit is [`deploy/gpb.container`](gpb.container) in the repository — fetch that one file if you
did not clone it. Three lines in it are yours to set before the first start: the pool volume (the
default puts it under your home, beside the data), `TZ`, and — if you want the listener off every
interface but one — a host address on `PublishPort`.

```bash
mkdir -p ~/.config/containers/systemd
cp deploy/gpb.container ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user start gpb
```

On an install that already runs, diff before you copy: the file in the repository is a template
carrying defaults, and overwriting a unit whose three lines you set months ago is how a pool
quietly moves back under the home directory.

`Restart=always` is what the settings page's Restart button rests on: it stops the daemon cleanly,
and only `always` takes a clean exit as something to undo. Copy the unit over and reload after
changing it, or the button stops gpb and nothing brings it back.

Quadlet generates the real service unit. When a typo yields a baffling "unit not found",
read the generator output:

```bash
/usr/lib/systemd/system-generators/podman-system-generator --user --dryrun
cat /run/user/$UID/systemd/generator/gpb.service
```

`UserNS=keep-id` in the unit is what keeps the pool owned by your login user rather than by a
subordinate uid you cannot browse it with.

## 4. Survive logout and reboot

Rootless user units die at logout unless lingering is enabled:

```bash
loginctl show-user $USER --property=Linger    # want Linger=yes
loginctl enable-linger $USER                  # only if it says no
```

## 5. Open the port

The unit publishes on **8090**, not 8080: on a home server 8080 is the port everything else
wants too. Inside the container gpb still listens on 8080.

```bash
sudo firewall-cmd --list-ports
sudo firewall-cmd --permanent --add-port=8090/tcp && sudo firewall-cmd --reload
```

## 6. Set the web password

```bash
podman exec -it systemd-gpb gpb passwd
```

Then open `http://<host>:8090/` and log in. The status page shows the Google session state; the
"Google session" page starts the browser login when Google needs an interactive sign-in — which
a fresh install always does, since the profile starts empty.

## 7. Point the app at its own address

The daemon writes `~/gpb/data/config.toml` on first start with a default `external_url` of
port 8080. The links it builds — chiefly the one into the VNC sign-in — have to match the
published port, or they lead nowhere:

```toml
[web]
external_url = "http://<host>:8090"
```

```bash
systemctl --user restart gpb
```

## 8. Encryption, optionally

Off by default, and worth turning on: the password typed into this UI unlocks the Chrome profile
in `~/gpb/data`, which is a whole Google account rather than a Photos-scoped token, and the
session cookie behind it is a bearer token for the same. On a flat home LAN both are readable by
anything else on the wire.

Settings → *How this page is reached* → **Encryption**. Set the address first: the certificate is
made for that name and no other, because inside the container the process has no way to learn the
address you browse to — its own hostname is a random hex string and its own IP is on a network
nobody browses from.

- **A certificate gpb makes.** Written to `~/gpb/data/tls/` on the next start, key at mode 0600,
  valid 397 days and renewed a month before that at a restart. Your browser will refuse it once
  and offer to continue; that is what a self-signed certificate is, not a fault.
- **A certificate you provide.** Give paths *inside the container* — `/data/...` is
  `~/gpb/data/...` on the host, so a certificate dropped there is the easy case. It is loaded when
  you save, so a wrong path is refused on the page rather than discovered as a daemon that will
  not start.

The address moves to `https://…` on its own when you change this setting, and says so when it does
— the sign-in links are built from it, and one left saying `http` leads nowhere. Only that save
touches it, so an address you set yourself afterwards stays as you wrote it. The port does not
change: the unit still publishes 8090 and the container still listens on 8080. Restart to pick it
up — the button at the foot of the settings page will do it, and names the address to come back to.

The unit's health command follows the setting with it: `gpb status` dials the daemon over whatever
it serves and trusts the certificate the config names, so turning encryption on does not turn the
container unhealthy.

If the certificate ever goes missing from under a running install, the daemon refuses to start
rather than quietly falling back to plain http — read `journalctl --user -u gpb` for the path it
could not load.

## Day to day

```bash
journalctl --user -u gpb -f                        # what it is doing
podman exec systemd-gpb gpb version                # which release is running
podman exec systemd-gpb gpb status                 # what the Google session is doing
podman exec systemd-gpb gpb verify                 # re-read the backed-up files, check every hash
podman auto-update && systemctl --user restart gpb # update (the unit carries AutoUpdate=registry)
```

A new image never touches `/data`: Chrome, the profile, the database and the certificate live on
the volume and survive. Chrome is fetched once and then left alone, so an install running for a
year is running the browser it downloaded a year ago — `rm -rf ~/gpb/data/chrome` and restart to
take the current one.

`verify` is worth a monthly timer once the library is large: it reads every file, so it takes as
long as reading the pool takes, and it exits non-zero if anything has rotted — which is all a
systemd timer or a cron entry needs to page you. The daemon keeps its own weekly copy of the
database at `~/gpb/data/state.backup.db`; whatever backs up the host should pick that file up
rather than the live `state.db`, which is in WAL mode and is not a copy on its own. The container reports `unhealthy` until the first
Google sign-in — the health command asks after the session, not the socket, and a fresh profile
has none.

The [notes that apply to any install](README.md#notes) — why Google Chrome and not Chromium,
the sandbox fallback, the stale profile lock, reading a warmup in the log — are worth a minute
before you need them.
