# go.mod asks for 1.26.4; the floor is never below 1.25.1, whose http.CrossOriginProtection
# fixed the bypass 1.25.0 shipped with (CVE-2025-47910).
FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
# CGO off keeps the binary independent of the runtime stage's glibc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gpb ./cmd/gpb


# Google Chrome is fetched at runtime rather than installed here — deploy/entrypoint.sh says why —
# but its shared libraries still have to be in the image. This stage reads the dependency list out
# of Google's own package metadata rather than keeping a copy of it in this file, where it would
# rot silently until a Chrome release added a library and every install broke at once.
#
# The .deb is downloaded here and goes no further: nothing of it reaches the image below except a
# list of Debian package names.
FROM debian:bookworm-slim AS chrome-dependencies

ADD https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb /tmp/chrome.deb
RUN dpkg-deb -f /tmp/chrome.deb Depends \
    | tr ',' '\n' \
    | sed 's/|.*//; s/(.*)//; s/ //g' \
    | grep . > /tmp/chrome-dependencies


FROM debian:bookworm-slim

# Google Chrome, not Debian's chromium — the entrypoint fetches Chrome from Google on the first
# start. Chromium is built without Google's API keys and tells every site it is "Chromium" rather
# than "Google Chrome", and Google's account machinery treats it accordingly: measured 2026-08-10
# on the Fedora box, its attempt to keep the signed-in session current was answered with 401
# accounts.google.com/RotateCookiesPage, and the account was signed out of every Google property
# within two minutes — every time, for six consecutive sign-ins. The same code driving Google
# Chrome on macOS holds a session for days.
COPY --from=chrome-dependencies /tmp/chrome-dependencies /tmp/chrome-dependencies

RUN apt-get update && apt-get install --yes --no-install-recommends \
        $(cat /tmp/chrome-dependencies) \
        ca-certificates \
        curl \
        fonts-liberation \
        fonts-noto-color-emoji \
        novnc \
        tini \
        websockify \
        x11vnc \
        xvfb \
        xz-utils \
    && rm -rf /var/lib/apt/lists/* /tmp/chrome-dependencies

RUN useradd --create-home --uid 1000 --shell /usr/sbin/nologin gpb \
    && mkdir -p /data /photos \
    && chown gpb:gpb /data /photos

COPY --from=build /out/gpb /usr/local/bin/gpb
COPY deploy/entrypoint.sh /usr/local/bin/gpb-entrypoint

ENV GPB_DATA_DIR=/data \
    GPB_PHOTOS_DIR=/photos \
    GPB_CHROME_HOME=/data/chrome \
    GPB_CHROME_PATH=/data/chrome/opt/google/chrome/google-chrome \
    GPB_CHROME_DEB_URL=https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
    GPB_NOVNC_DIR=/usr/share/novnc \
    HOME=/home/gpb

USER gpb
WORKDIR /home/gpb
VOLUME ["/data", "/photos"]
EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/gpb-entrypoint"]
CMD ["daemon"]
