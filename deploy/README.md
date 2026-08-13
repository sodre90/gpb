# Deploying gpb

Two ways in, and they are separate guides because following half of each is worse than following
either:

- **[Docker Compose](compose.md)** — clone, build, up. Pick this if you have Docker and no
  particular opinion about how services are managed.
- **[Rootless Podman + systemd Quadlet](quadlet.md)** — what gpb actually runs on. Pick this on a
  Fedora-ish box, or wherever you want systemd to own the lifecycle and the photo pool to stay
  owned by your login user.

Either way the image is [`sodre90/gpb`](https://hub.docker.com/r/sodre90/gpb) and it is
**linux/amd64**, because Google ships Chrome for Linux on that architecture and no other. It
contains **no Google Chrome**: Chrome is not free software and not ours to hand on, so the
container downloads it from Google into its data volume the first time it starts — once, about
110 MB, unpacking to some 430 MB — and says so in the log. Both guides also tell you how to build
the image yourself if you would rather.

Whichever you follow, run it as an unprivileged user. Nothing here wants root beyond making a
directory on a media disk and opening a port.

## Notes

These apply to both, and are worth a minute before you need them.

- **Google Chrome, not Chromium — this is load-bearing.** The container installs
  `google-chrome-stable` from Google's own `.deb`. Chromium is built without Google's API keys
  and identifies itself as "Chromium", and Google's account machinery refuses to keep such a
  session current: measured 2026-08-10, every warmup's call to
  `accounts.google.com/RotateCookiesPage` came back `401` and the account was signed out of
  every Google property within two minutes of signing in — six sign-ins, six times. Swapping to
  Google Chrome at the same version number fixed it outright. If sessions ever start dying
  minutes after login again, check the browser binary first:

  ```bash
  podman exec systemd-gpb google-chrome --version    # want "Google Chrome", not "Chromium"
  ```

  It is also why there is no image to pull: Chrome is not free software, and publishing a built
  image would be redistributing it. Building it yourself downloads Chrome from Google under the
  terms you accept there.

- **Chrome sandbox.** Under rootless Podman, unprivileged user namespaces inside the container
  may be unavailable and Chrome will refuse to start. The daemon detects this and retries once
  with `--no-sandbox`, then keeps using it; the status page reports when the fallback is active.
- **Stale Chrome profile lock.** A killed Chrome leaves `profile/SingletonLock` naming the
  *container's* hostname, which is new on every start, so Chrome reads the leftover as "in
  use by another computer" and refuses to open the profile — permanently. It wedged the daemon
  into a restart loop on the first deploy, 2026-08-10. The daemon now clears the marker when
  it takes the profile lock, so this should not recur; if a start ever fails with "the profile
  appears to be in use by another Chrome process", that fix has regressed.
- **Reading a warmup in the log.** Each one prints the trail Google routed it through. A healthy
  one ends on `photos.google.com` having taken `200 accounts.google.com/RotateCookiesPage`
  along the way; a `401` there means the session is being refused and is about to die.

  ```bash
  journalctl --user -u gpb --no-pager -o cat | grep "routed the warmup" | tail -3
  ```
- **A fresh install reports `unhealthy`** until the first Google sign-in. The health command asks
  after the Google session rather than the socket, and an empty profile has no session.
- **The web password has no default.** `gpb passwd` is the one configuration step that is not
  optional; until it runs, the UI has nothing to let you in with.
